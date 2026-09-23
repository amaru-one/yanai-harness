package workflow

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Round states, in the only order they may be reached. Each one names what is
// already durable, so a process that dies anywhere can tell what it must not
// redo: a round at RoundApplied has a confirmed repository hash and never
// replays its writes, and a round at RoundCandidate has a validated candidate
// and never pays for another generation.
const (
	RoundOpen        = "open"      // inputs recorded; a model call may or may not have happened
	RoundCandidate   = "candidate" // a validated candidate is durable
	RoundApplied     = "applied"   // the patch is confirmed against the actual repository
	RoundChecked     = "checked"   // every approved check finished and passed
	RoundImplemented = "implemented"
	RoundNoChange    = "no_change"
	RoundFailed      = "failed"
)

// CheckRecord links one approved check's run to the exact repository state it
// ran against. Tested is the pre-state baseline: a result that was produced
// against a different state proves nothing about this one.
type CheckRecord struct {
	CheckID  string      `json:"check_id"`
	RunID    string      `json:"run_id"`
	Evidence ArtifactRef `json:"evidence"`
	ExitCode int         `json:"exit_code"`
	Passed   bool        `json:"passed"`
	Tested   string      `json:"tested_state"`
	Failure  string      `json:"failure,omitempty"`
}

// ExecutionRound is one complete generation → application → validation attempt
// for one ticket revision under one contract. It is the durable answer to
// "what exactly was tested, from what, and what happened" — keyed by contract
// and ticket revision so a revised plan or a re-approval can never resume a
// round that was reasoned about under different terms.
type ExecutionRound struct {
	Cycle          int           `json:"cycle"`
	Contract       string        `json:"contract"`
	Ticket         string        `json:"ticket"`
	TicketRevision int           `json:"ticket_revision"`
	Round          int           `json:"round"`
	State          string        `json:"state"`
	Inputs         []ArtifactRef `json:"inputs,omitempty"`
	Attempt        string        `json:"attempt,omitempty"`
	Response       *ArtifactRef  `json:"response,omitempty"`
	Candidate      *ArtifactRef  `json:"candidate,omitempty"`
	Expected       string        `json:"expected_pre_state,omitempty"`
	Patch          *ArtifactRef  `json:"patch,omitempty"`
	Confirmed      string        `json:"confirmed_state,omitempty"`
	Diff           *ArtifactRef  `json:"diff,omitempty"`
	Checks         []CheckRecord `json:"checks,omitempty"`
	Manifest       *ArtifactRef  `json:"manifest,omitempty"`
	Outcome        string        `json:"outcome,omitempty"`
	Failure        string        `json:"failure,omitempty"`
	Explanation    string        `json:"explanation,omitempty"`
}

func (r ExecutionRound) key() string {
	return fmt.Sprintf("%s/%d/%s/%s/%d/%d", r.Contract, r.Cycle, r.Ticket, "round", r.TicketRevision, r.Round)
}

// OpenRound records a new round, or returns the existing one unchanged. The
// insert is what makes a crash between "decided to work" and "called the
// model" recoverable: the row exists before anything was spent.
func (s *Store) OpenRound(r ExecutionRound) (ExecutionRound, int64, error) {
	if r.Ticket == "" || r.Contract == "" || r.Round < 1 {
		return r, 0, errors.New("a round requires a contract, ticket and positive round number")
	}
	if err := s.HasApproval(r.Cycle, r.Contract); err != nil {
		return r, 0, err
	}
	if existing, version, found, err := s.LatestRound(r.Cycle, r.Contract, r.Ticket, r.TicketRevision); err != nil {
		return r, 0, err
	} else if found && existing.Round >= r.Round {
		if existing.Round != r.Round {
			return r, 0, fmt.Errorf("round %d already superseded by round %d", r.Round, existing.Round)
		}
		return existing, version, nil
	}
	r.State = RoundOpen
	payload, err := json.Marshal(r)
	if err != nil {
		return r, 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return r, 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	ts := now()
	if _, err = tx.Exec(`INSERT INTO workflow_rounds(project,cycle,contract_hash,ticket,ticket_revision,round,state,outcome,failure,payload,state_version,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,'','',?,1,?,?)`, s.project, r.Cycle, r.Contract, r.Ticket, r.TicketRevision, r.Round, r.State, string(payload), ts, ts); err != nil {
		return r, 0, err
	}
	if err = s.appendEventTx(tx, Event{Cycle: r.Cycle, TicketID: r.Ticket, Actor: ActorEngine, Type: "round.opened", Payload: r.key()}); err != nil {
		return r, 0, err
	}
	return r, 1, tx.Commit()
}

// CommitRound advances a round's durable record under its conditional version.
// Every transition is append-only in effect: nothing here can walk a round
// back to a state whose evidence has already been superseded, and a stale
// version means another process committed first.
func (s *Store) CommitRound(r ExecutionRound, expected int64, evt Event) (int64, error) {
	if err := validRoundState(r.State); err != nil {
		return 0, err
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	var current string
	var version int64
	if err = tx.QueryRow(`SELECT state,state_version FROM workflow_rounds WHERE project=? AND cycle=? AND contract_hash=? AND ticket=? AND ticket_revision=? AND round=?`,
		s.project, r.Cycle, r.Contract, r.Ticket, r.TicketRevision, r.Round).Scan(&current, &version); err != nil {
		return 0, err
	}
	if version != expected {
		return 0, &ErrStaleVersion{Kind: "round", Expected: expected}
	}
	if !roundAdvances(current, r.State) {
		return 0, fmt.Errorf("round cannot move from %q to %q", current, r.State)
	}
	res, err := tx.Exec(`UPDATE workflow_rounds SET state=?,outcome=?,failure=?,payload=?,state_version=state_version+1,updated_at=?
		WHERE project=? AND cycle=? AND contract_hash=? AND ticket=? AND ticket_revision=? AND round=? AND state_version=?`,
		r.State, r.Outcome, r.Failure, string(payload), now(), s.project, r.Cycle, r.Contract, r.Ticket, r.TicketRevision, r.Round, expected)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, &ErrStaleVersion{Kind: "round", Expected: expected}
	}
	evt.Cycle, evt.TicketID, evt.Actor = r.Cycle, r.Ticket, ActorEngine
	if evt.Type == "" {
		evt.Type = "round.committed:" + r.State
	}
	if evt.Payload == "" {
		evt.Payload = r.key()
	}
	if err = s.appendEventTx(tx, evt); err != nil {
		return 0, err
	}
	return expected + 1, tx.Commit()
}

// LatestRound returns the highest-numbered round recorded for this exact
// contract and ticket revision, with its conditional version.
func (s *Store) LatestRound(cycle int, contract, ticket string, revision int) (ExecutionRound, int64, bool, error) {
	var payload string
	var version int64
	err := s.db.QueryRow(`SELECT payload,state_version FROM workflow_rounds WHERE project=? AND cycle=? AND contract_hash=? AND ticket=? AND ticket_revision=? ORDER BY round DESC LIMIT 1`,
		s.project, cycle, contract, ticket, revision).Scan(&payload, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionRound{}, 0, false, nil
	}
	if err != nil {
		return ExecutionRound{}, 0, false, err
	}
	var r ExecutionRound
	if err = json.Unmarshal([]byte(payload), &r); err != nil {
		return r, 0, false, err
	}
	return r, version, true, nil
}

// GetRound returns one exact round. A repair reads the round it is replacing
// through this, not through LatestRound — by the time it asks, the latest
// round is the replacement itself.
func (s *Store) GetRound(cycle int, contract, ticket string, revision, round int) (ExecutionRound, int64, bool, error) {
	var payload string
	var version int64
	err := s.db.QueryRow(`SELECT payload,state_version FROM workflow_rounds WHERE project=? AND cycle=? AND contract_hash=? AND ticket=? AND ticket_revision=? AND round=?`,
		s.project, cycle, contract, ticket, revision, round).Scan(&payload, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionRound{}, 0, false, nil
	}
	if err != nil {
		return ExecutionRound{}, 0, false, err
	}
	var r ExecutionRound
	if err = json.Unmarshal([]byte(payload), &r); err != nil {
		return r, 0, false, err
	}
	return r, version, true, nil
}

// Rounds returns every round recorded for a cycle, oldest first. It is the
// evidence trail a reviewer reads; nothing reads it back as state.
func (s *Store) Rounds(cycle int) ([]ExecutionRound, error) {
	rows, err := s.db.Query(`SELECT payload FROM workflow_rounds WHERE project=? AND cycle=? ORDER BY ticket,ticket_revision,round`, s.project, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []ExecutionRound
	for rows.Next() {
		var payload string
		if err = rows.Scan(&payload); err != nil {
			return nil, err
		}
		var r ExecutionRound
		if err = json.Unmarshal([]byte(payload), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func validRoundState(state string) error {
	switch state {
	case RoundOpen, RoundCandidate, RoundApplied, RoundChecked, RoundImplemented, RoundNoChange, RoundFailed:
		return nil
	}
	return fmt.Errorf("unknown round state %q", state)
}

// roundAdvances is the round's own transition table. Failure is reachable from
// anywhere except a finished round, because a round can be abandoned at any
// point; the successful states are strictly ordered.
func roundAdvances(from, to string) bool {
	if from == RoundImplemented || from == RoundNoChange || from == RoundFailed {
		return false
	}
	if to == RoundFailed {
		return true
	}
	// A round accrues evidence within a state before it advances — the inputs
	// and the raw response are both recorded while it is still open — so
	// staying put is allowed. What is never allowed is going back.
	if from == to {
		return true
	}
	order := map[string]int{RoundOpen: 0, RoundCandidate: 1, RoundApplied: 2, RoundChecked: 3}
	switch to {
	case RoundNoChange:
		return from == RoundOpen || from == RoundCandidate
	case RoundImplemented:
		return from == RoundChecked
	}
	f, okFrom := order[from]
	t, okTo := order[to]
	return okFrom && okTo && t > f
}

// MarkImplemented is the evidence gate. A ticket reaches "implemented" only
// here, and only when its round already holds a confirmed patch against the
// exact tested state, every approved check passed against that same state, and
// the implementation manifest is published. ApplyTicketStatus cannot reach
// this status at all — there is no edge for it in the transition table — so no
// generic status update can bypass what this verifies.
func (s *Store) MarkImplemented(r ExecutionRound, expected int64, ticketVersion int64) error {
	if err := s.HasApproval(r.Cycle, r.Contract); err != nil {
		return err
	}
	if r.State != RoundChecked {
		return fmt.Errorf("round %s is %q, not %q; implementation requires completed checks", r.key(), r.State, RoundChecked)
	}
	if r.Patch == nil || r.Candidate == nil || r.Manifest == nil || r.Diff == nil {
		return errors.New("implementation requires a durable candidate, patch, diff and manifest")
	}
	if strings.TrimSpace(r.Confirmed) == "" || r.Confirmed == r.Expected {
		return errors.New("implementation requires a confirmed repository state that differs from the pre-state")
	}
	if len(r.Checks) == 0 {
		return errors.New("implementation requires at least one completed check")
	}
	for _, c := range r.Checks {
		if !c.Passed || c.ExitCode != 0 {
			return fmt.Errorf("check %s did not pass; implementation is not claimable", c.CheckID)
		}
		if c.Tested != r.Confirmed {
			return fmt.Errorf("check %s ran against %s, not the confirmed state %s", c.CheckID, c.Tested, r.Confirmed)
		}
		if c.Evidence.ID == "" || c.Evidence.SHA256 == "" {
			return fmt.Errorf("check %s has no published evidence", c.CheckID)
		}
	}
	r.State, r.Outcome = RoundImplemented, RoundImplemented
	return s.finishRound(r, expected, ticketVersion, TicketImplemented, "ticket.implemented")
}

// MarkNoChange records an explicit, explained no-change finding. It publishes
// the same durable evidence an implementation does minus a patch — because
// there is none — and deliberately reaches a status that satisfies no
// dependency: "nothing to do here" is a finding, not an implementation.
func (s *Store) MarkNoChange(r ExecutionRound, expected int64, ticketVersion int64) error {
	if err := s.HasApproval(r.Cycle, r.Contract); err != nil {
		return err
	}
	if r.Candidate == nil || r.Manifest == nil {
		return errors.New("a no-change outcome requires its durable candidate and manifest")
	}
	if strings.TrimSpace(r.Explanation) == "" {
		return errors.New("a no-change outcome requires an explanation")
	}
	if r.Patch != nil || (r.Confirmed != "" && r.Confirmed != r.Expected) {
		return errors.New("a no-change outcome cannot carry a repository change")
	}
	r.State, r.Outcome = RoundNoChange, RoundNoChange
	return s.finishRound(r, expected, ticketVersion, TicketNoChangeReported, "ticket.no_change_reported")
}

// finishRound commits the round's final record and its ticket's status in one
// transaction, so evidence and status can never disagree.
func (s *Store) finishRound(r ExecutionRound, expected, ticketVersion int64, status, eventType string) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var current string
	var version int64
	if err = tx.QueryRow(`SELECT state,state_version FROM workflow_rounds WHERE project=? AND cycle=? AND contract_hash=? AND ticket=? AND ticket_revision=? AND round=?`,
		s.project, r.Cycle, r.Contract, r.Ticket, r.TicketRevision, r.Round).Scan(&current, &version); err != nil {
		return err
	}
	if version != expected {
		return &ErrStaleVersion{Kind: "round", Expected: expected}
	}
	if !roundAdvances(current, r.State) {
		return fmt.Errorf("round cannot move from %q to %q", current, r.State)
	}
	res, err := tx.Exec(`UPDATE workflow_rounds SET state=?,outcome=?,failure='',payload=?,state_version=state_version+1,updated_at=?
		WHERE project=? AND cycle=? AND contract_hash=? AND ticket=? AND ticket_revision=? AND round=? AND state_version=?`,
		r.State, r.Outcome, string(payload), now(), s.project, r.Cycle, r.Contract, r.Ticket, r.TicketRevision, r.Round, expected)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return &ErrStaleVersion{Kind: "round", Expected: expected}
	}
	res, err = tx.Exec(`UPDATE workflow_tickets SET status=?,state_version=state_version+1,updated_at=?
		WHERE project=? AND cycle=? AND id=? AND revision=? AND state_version=? AND status=? AND legacy=0`,
		status, now(), s.project, r.Cycle, r.Ticket, r.TicketRevision, ticketVersion, TicketCandidateReady)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return &ErrStaleVersion{Kind: "ticket", Expected: ticketVersion}
	}
	if err = s.appendEventTx(tx, Event{Cycle: r.Cycle, TicketID: r.Ticket, Actor: ActorEngine, Type: eventType, Payload: r.key()}); err != nil {
		return err
	}
	return tx.Commit()
}
