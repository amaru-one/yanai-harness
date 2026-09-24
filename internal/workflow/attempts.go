package workflow

import (
	"database/sql"
	"errors"
	"time"
)

// Attempt states. in_flight means the model call may or may not have
// happened from the provider's point of view — BeginAttempt commits it
// *before* the call. completed/failed are what a live process observed;
// unknown is what reconciliation stamps an in_flight row found at startup,
// because the process that owned it is gone and whether it was billed is not
// knowable from here.
const (
	AttemptInFlight  = "in_flight"
	AttemptCompleted = "completed"
	AttemptFailed    = "failed"
	AttemptUnknown   = "unknown"
)

// AttemptInput describes a model call about to be made.
type AttemptInput struct {
	Cycle       int
	TicketID    string
	Role        string
	Kind        string // e.g. "select_files" | "role_turn" | "decision"
	RequestHash string
}

// Usage mirrors openrouter.Usage without importing it, keeping this package
// free of a dependency on the HTTP client.
type Usage struct{ PromptTokens, CompletionTokens, TotalTokens int }

// Attempt is an attempt's durable row.
type Attempt struct {
	ID               string
	Project          string
	Cycle            int
	TicketID         string
	Role             string
	Kind             string
	RequestHash      string
	ResponseHash     string
	State            string
	CostKnown        bool
	UsageKnown       bool
	CostUSD          *float64
	ProviderID       string
	FinishReason     string
	Acknowledged     bool
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Error            string
	StartedAt        time.Time
	EndedAt          time.Time
}

// BeginAttempt commits an in_flight row before the model is called. If the
// process dies during the call, this row is what lets a later run tell "a
// call may have happened, cost unknown" apart from "nothing was ever asked".
func (s *Store) BeginAttempt(a AttemptInput) (string, error) {
	id := newID("att")
	_, err := s.db.Exec(`INSERT INTO workflow_attempts(id, project, cycle, ticket_id, role, kind, request_hash, state, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, s.project, a.Cycle, a.TicketID, a.Role, a.Kind, a.RequestHash, AttemptInFlight, now())
	return id, err
}

// CompleteAttempt records a successful call. It only takes effect from
// in_flight, so it cannot resurrect an attempt reconciliation already
// stamped unknown.
func (s *Store) CompleteAttempt(id, responseHash string, usage Usage) error {
	res, err := s.db.Exec(`UPDATE workflow_attempts SET state = ?, response_hash = ?, cost_known = 0, usage_known = 1, prompt_tokens = ?, completion_tokens = ?, total_tokens = ?, ended_at = ?
		WHERE id = ? AND project = ? AND state = ?`,
		AttemptCompleted, responseHash, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, now(), id, s.project, AttemptInFlight)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("attempt is not in_flight; it may already have been reconciled as unresolved")
	}
	return nil
}

// FailAttempt records a call that the provider itself reported as failed
// (client error, timeout with a clean response): still a known outcome, not
// an unresolved one.
func (s *Store) FailAttempt(id, errMsg string) error {
	res, err := s.db.Exec(`UPDATE workflow_attempts SET state = ?, error = ?, ended_at = ? WHERE id = ? AND project = ? AND state = ?`,
		AttemptFailed, errMsg, now(), id, s.project, AttemptInFlight)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("attempt is not in_flight; it may already have been reconciled as unresolved")
	}
	return nil
}

func (s *Store) GetAttempt(id string) (Attempt, error) {
	var a Attempt
	var started, ended string
	var costKnown int
	err := s.db.QueryRow(`SELECT id, cycle, ticket_id, role, kind, request_hash, response_hash, state, cost_known, prompt_tokens, completion_tokens, total_tokens, error, started_at, ended_at
		FROM workflow_attempts WHERE id = ? AND project = ?`, id, s.project).
		Scan(&a.ID, &a.Cycle, &a.TicketID, &a.Role, &a.Kind, &a.RequestHash, &a.ResponseHash, &a.State, &costKnown, &a.PromptTokens, &a.CompletionTokens, &a.TotalTokens, &a.Error, &started, &ended)
	if err != nil {
		return Attempt{}, err
	}
	a.Project = s.project
	a.CostKnown = costKnown != 0
	a.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if ended != "" {
		a.EndedAt, _ = time.Parse(time.RFC3339Nano, ended)
	}
	var cost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT cost_usd,usage_known,provider_id,finish_reason,acknowledged FROM workflow_attempts WHERE project=? AND id=?`, s.project, id).Scan(&cost, &a.UsageKnown, &a.ProviderID, &a.FinishReason, &a.Acknowledged); err != nil {
		return Attempt{}, err
	}
	if cost.Valid {
		a.CostUSD = &cost.Float64
	}
	return a, nil
}

// ReconcileAttempts flips every in_flight attempt to unknown: found at
// startup, it means the process that owned it died mid-call, and whether
// OpenRouter billed for it is unknowable from here. cost_known is left 0 —
// unknown, never assumed zero — and attempt.unresolved is logged for each.
func (s *Store) ReconcileAttempts() ([]Attempt, error) {
	rows, err := s.db.Query(`SELECT id, cycle FROM workflow_attempts WHERE project = ? AND state = ?`, s.project, AttemptInFlight)
	if err != nil {
		return nil, err
	}
	type found struct {
		id    string
		cycle int
	}
	var items []found
	for rows.Next() {
		var f found
		if err := rows.Scan(&f.id, &f.cycle); err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		items = append(items, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return nil, err
	}
	rows.Close() //nolint:errcheck

	var out []Attempt
	for _, it := range items {
		if _, err := s.db.Exec(`UPDATE workflow_attempts SET state = ?, ended_at = ? WHERE id = ? AND project = ? AND state = ?`,
			AttemptUnknown, now(), it.id, s.project, AttemptInFlight); err != nil {
			return out, err
		}
		if err := s.AppendEvent(Event{Cycle: it.cycle, Actor: ActorEngine, Type: "attempt.unresolved", Payload: it.id}); err != nil {
			return out, err
		}
		a, err := s.GetAttempt(it.id)
		if err != nil {
			return out, err
		}
		out = append(out, a)
	}
	return out, nil
}

// UnresolvedAttempts lists attempts left in the unknown state. Planning and execution
// check this and refuse — per the recorded decision, never retrying an
// interrupted model call silently, since it may already have been billed.
func (s *Store) UnresolvedAttempts() ([]Attempt, error) {
	rows, err := s.db.Query(`SELECT id FROM workflow_attempts WHERE project = ? AND state = ?`, s.project, AttemptUnknown)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return nil, err
	}
	// Close before the nested queries below: with a single pooled
	// connection (see OpenStore), an open Rows would otherwise starve
	// GetAttempt of a connection and the two would deadlock.
	rows.Close() //nolint:errcheck

	var out []Attempt
	for _, id := range ids {
		a, err := s.GetAttempt(id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// ResolveAttempt clears an unknown attempt after the operator has explicitly
// acknowledged the risk (the CLI's --retry-unresolved flag), so it stops
// blocking future runs. It is never called automatically.
func (s *Store) ResolveAttempt(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE workflow_attempts SET state='failed',acknowledged=1 WHERE id=? AND project=? AND state='unknown'`, id, s.project)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("attempt is not in the unknown state")
	}
	if err = s.appendEventTx(tx, Event{Actor: ActorHuman, Type: "attempt.retry_acknowledged", Payload: id}); err != nil {
		return err
	}
	return tx.Commit()
}

// UnreconciledAttempts includes successful responses with absent billing, not
// only interrupted calls. The local status command must expose both.
func (s *Store) UnreconciledAttempts() ([]Attempt, error) {
	rows, err := s.db.Query(`SELECT id FROM workflow_attempts WHERE project=? AND (cost_known=0 OR usage_known=0 OR state='unknown') ORDER BY started_at`, s.project)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var result []Attempt
	for _, id := range ids {
		a, err := s.GetAttempt(id)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, nil
}
