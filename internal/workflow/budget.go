package workflow

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Budget struct {
	Policy            ExecutionPolicy `json:"policy"`
	Tokens            int64           `json:"tokens_used_or_reserved"`
	Cost              float64         `json:"cost_used_or_reserved_usd"`
	Calls             int             `json:"calls"`
	Repairs           int             `json:"repairs"`
	ActiveMS          int64           `json:"active_ms"`
	Unknown           int             `json:"unreconciled_attempts"`
	RemainingTokens   int64           `json:"remaining_tokens"`
	RemainingCostUSD  float64         `json:"remaining_cost_usd"`
	RemainingCalls    int             `json:"remaining_calls"`
	RemainingRepairs  int             `json:"remaining_repairs"`
	RemainingActiveMS int64           `json:"remaining_active_ms"`
}

// Budget buckets. The worker spends from the execution policy a human
// approved; the parent spends from the planning budget captured when its
// cycle started. They are recorded separately and reported together.
const (
	BucketWorker = "worker"
	BucketParent = "parent"
)

func budgetTable(bucket string) (string, error) {
	switch bucket {
	case BucketWorker, "":
		return "workflow_budgets", nil
	case BucketParent:
		return "workflow_parent_budgets", nil
	}
	return "", fmt.Errorf("unknown budget bucket %q", bucket)
}

func bucketName(bucket string) string {
	if bucket == "" {
		return BucketWorker
	}
	return bucket
}

func (s *Store) EnsureBudget(cycle int, p ExecutionPolicy) error {
	return s.EnsureBucket(cycle, BucketWorker, p)
}

// EnsureParentBudget records the parent's captured planning budget. It is
// created once per cycle and never changes afterwards.
func (s *Store) EnsureParentBudget(cycle int, p PlanningBudget) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.ensureBucket(cycle, BucketParent, p.Policy())
}

func (s *Store) EnsureBucket(cycle int, bucket string, p ExecutionPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.ensureBucket(cycle, bucket, p)
}

func (s *Store) ensureBucket(cycle int, bucket string, p ExecutionPolicy) error {
	table, err := budgetTable(bucket)
	if err != nil {
		return err
	}
	if _, err := s.GetCycle(cycle); err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	_, err = s.db.Exec(fmt.Sprintf(`INSERT INTO %s(project,cycle,policy,tokens,cost,calls) SELECT ?,?,?,COALESCE(SUM(reserved_tokens),0),COALESCE(SUM(reserved_cost),0),COUNT(*) FROM workflow_attempts WHERE project=? AND cycle=? AND bucket=? ON CONFLICT DO NOTHING`, table), s.project, cycle, string(raw), s.project, cycle, bucketName(bucket))
	if err != nil {
		return err
	}
	b, err := s.BucketBudget(cycle, bucket)
	if err != nil {
		return err
	}
	old, _ := json.Marshal(b.Policy)
	if string(old) != string(raw) {
		if bucket == BucketParent {
			return fmt.Errorf("the parent budget captured for this cycle differs from the one in use; parent settings apply from the next cycle")
		}
		return fmt.Errorf("execution policy changed; use yanai policy --note to revise it explicitly (spending is preserved)")
	}
	return nil
}
func (s *Store) Budget(cycle int) (Budget, error) { return s.BucketBudget(cycle, BucketWorker) }

// ParentBudget reports the parent's planning and closing spending.
func (s *Store) ParentBudget(cycle int) (Budget, error) { return s.BucketBudget(cycle, BucketParent) }

func (s *Store) BucketBudget(cycle int, bucket string) (Budget, error) {
	var b Budget
	var raw string
	table, err := budgetTable(bucket)
	if err != nil {
		return b, err
	}
	err = s.db.QueryRow(fmt.Sprintf(`SELECT policy,tokens,cost,calls,repairs,active_ms FROM %s WHERE project=? AND cycle=?`, table), s.project, cycle).Scan(&raw, &b.Tokens, &b.Cost, &b.Calls, &b.Repairs, &b.ActiveMS)
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal([]byte(raw), &b.Policy); err != nil {
		return b, err
	}
	err = s.db.QueryRow(`SELECT count(*) FROM workflow_attempts WHERE project=? AND cycle=? AND bucket=? AND (cost_known=0 OR usage_known=0 OR state='unknown')`, s.project, cycle, bucketName(bucket)).Scan(&b.Unknown)
	b.RemainingTokens = max(0, b.Policy.MaxTokens-b.Tokens)
	b.RemainingCostUSD = max(0, b.Policy.MaxCostUSD-b.Cost)
	b.RemainingCalls = max(0, b.Policy.MaxCalls-b.Calls)
	b.RemainingRepairs = max(0, b.Policy.MaxRepairs-b.Repairs)
	b.RemainingActiveMS = max(0, b.Policy.MaxActiveSeconds*1000-b.ActiveMS)
	return b, err
}

// RevisePolicy is a local human operation; budget consumption never resets.
func (s *Store) RevisePolicy(cycle int, p ExecutionPolicy, note string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(note) == "" {
		return errors.New("policy revision requires --note")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var phase string
	if err = tx.QueryRow(`SELECT phase FROM workflow_cycles WHERE project=? AND cycle=?`, s.project, cycle).Scan(&phase); err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	_, err = tx.Exec(`INSERT INTO workflow_budgets(project,cycle,policy,tokens,cost,calls) SELECT ?,?,?,COALESCE(SUM(reserved_tokens),0),COALESCE(SUM(reserved_cost),0),COUNT(*) FROM workflow_attempts WHERE project=? AND cycle=? ON CONFLICT(project,cycle) DO UPDATE SET policy=excluded.policy`, s.project, cycle, string(raw), s.project, cycle)
	if err != nil {
		return err
	}
	if phase == PhaseApproved || phase == PhaseAwaitingExecution {
		_, err = tx.Exec(`UPDATE workflow_cycles SET phase=?,active_contract='',state_version=state_version+1 WHERE project=? AND cycle=?`, PhaseAwaitingApproval, s.project, cycle)
		if err != nil {
			return err
		}
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorHuman, Type: "policy.revised", Payload: note + "\n" + string(raw)}); err != nil {
		return err
	}
	return tx.Commit()
}

type Reservation struct {
	AttemptInput
	Bucket string
	Model  string
	Tokens int64
	Cost   float64
}

func (s *Store) ReserveCall(r Reservation) (string, error) {
	if r.Tokens <= 0 || !finite(r.Cost) {
		return "", errors.New("invalid reservation")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var raw string
	var unknown int
	table, err := budgetTable(r.Bucket)
	if err != nil {
		return "", err
	}
	if err = tx.QueryRow(fmt.Sprintf(`SELECT policy FROM %s WHERE project=? AND cycle=?`, table), s.project, r.Cycle).Scan(&raw); err != nil {
		return "", err
	}
	var p ExecutionPolicy
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return "", err
	}
	if err = tx.QueryRow(`SELECT count(*) FROM workflow_attempts WHERE project=? AND (cost_known=0 OR usage_known=0 OR state='unknown')`, s.project).Scan(&unknown); err != nil {
		return "", err
	}
	if unknown > 0 {
		return "", errors.New("unreconciled billing/usage: inspect status --attempts and reconcile-attempt before paid calls")
	}
	res, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET tokens=tokens+?,cost=cost+?,calls=calls+1 WHERE project=? AND cycle=? AND tokens+?<=? AND cost+?<=? AND calls<? AND active_ms<?`, table), r.Tokens, r.Cost, s.project, r.Cycle, r.Tokens, p.MaxTokens, r.Cost, p.MaxCostUSD, p.MaxCalls, p.MaxActiveSeconds*1000)
	if err != nil {
		return "", err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return "", fmt.Errorf("%s budget exhausted; no request dispatched", bucketName(r.Bucket))
	}
	id := newID("att")
	_, err = tx.Exec(`INSERT INTO workflow_attempts(id,project,cycle,ticket_id,role,kind,request_hash,state,started_at,reserved_tokens,reserved_cost,model,bucket) VALUES(?,?,?,?,?,?,?,'in_flight',?,?,?,?,?)`, id, s.project, r.Cycle, r.TicketID, r.Role, r.Kind, r.RequestHash, now(), r.Tokens, r.Cost, r.Model, bucketName(r.Bucket))
	if err != nil {
		return "", err
	}
	if err = s.appendEventTx(tx, Event{Cycle: r.Cycle, TicketID: r.TicketID, Actor: ActorEngine, Type: "attempt.reserved", Payload: id}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

type CallResult struct {
	State        string
	ResponseHash string
	Usage        Usage
	UsageKnown   bool
	Cost         *float64
	ProviderID   string
	FinishReason string
	Error        string
}

func (s *Store) SettleCall(id string, r CallResult) error {
	if r.State != AttemptCompleted && r.State != AttemptFailed && r.State != AttemptUnknown {
		return errors.New("invalid call outcome")
	}
	if r.Cost != nil && !finite(*r.Cost) {
		return errors.New("invalid provider cost")
	}
	if r.Usage.PromptTokens < 0 || r.Usage.CompletionTokens < 0 || r.Usage.TotalTokens < 0 {
		return errors.New("invalid provider usage")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cycle int
	var tokens int64
	var cost float64
	var bucket string
	if err = tx.QueryRow(`SELECT cycle,reserved_tokens,reserved_cost,bucket FROM workflow_attempts WHERE id=? AND project=? AND state='in_flight'`, id, s.project).Scan(&cycle, &tokens, &cost, &bucket); err != nil {
		return err
	}
	table, err := budgetTable(bucket)
	if err != nil {
		return err
	}
	actualTokens := tokens
	if r.UsageKnown {
		actualTokens = int64(r.Usage.TotalTokens)
	}
	actualCost := cost
	if r.Cost != nil {
		actualCost = *r.Cost
	}
	_, err = tx.Exec(fmt.Sprintf(`UPDATE %s SET tokens=tokens+?,cost=cost+? WHERE project=? AND cycle=?`, table), actualTokens-tokens, actualCost-cost, s.project, cycle)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE workflow_attempts SET state=?,response_hash=?,prompt_tokens=?,completion_tokens=?,total_tokens=?,cost_known=?,usage_known=?,cost_usd=?,reserved_tokens=?,reserved_cost=?,provider_id=?,finish_reason=?,error=?,ended_at=? WHERE project=? AND id=?`, r.State, r.ResponseHash, r.Usage.PromptTokens, r.Usage.CompletionTokens, r.Usage.TotalTokens, r.Cost != nil, r.UsageKnown, r.Cost, actualTokens, actualCost, r.ProviderID, r.FinishReason, r.Error, now(), s.project, id)
	if err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "attempt.settled", Payload: id}); err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileBilling records an explicit human-supplied invoice/reference. It does
// not acknowledge an uncertain call's retry risk; that is a separate operation.
func (s *Store) ReconcileBilling(id string, cost float64, tokens int64, reference string) error {
	if !finite(cost) || tokens < 0 || strings.TrimSpace(reference) == "" {
		return errors.New("reconciliation requires nonnegative cost/tokens and --reference")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cycle, known, usage int
	var oldTokens int64
	var oldCost float64
	var state, bucket string
	if err = tx.QueryRow(`SELECT cycle,cost_known,usage_known,reserved_tokens,reserved_cost,state,bucket FROM workflow_attempts WHERE project=? AND id=?`, s.project, id).Scan(&cycle, &known, &usage, &oldTokens, &oldCost, &state, &bucket); err != nil {
		return err
	}
	table, err := budgetTable(bucket)
	if err != nil {
		return err
	}
	if state == AttemptInFlight {
		return errors.New("cannot reconcile an active call")
	}
	if known != 0 && cost != oldCost {
		return errors.New("confirmed billing amount cannot be replaced")
	}
	if usage != 0 && tokens != oldTokens {
		return errors.New("confirmed token usage cannot be replaced")
	}
	if known != 0 && usage != 0 {
		return errors.New("attempt already reconciled")
	}
	_, err = tx.Exec(fmt.Sprintf(`UPDATE %s SET tokens=tokens+?,cost=cost+? WHERE project=? AND cycle=?`, table), tokens-oldTokens, cost-oldCost, s.project, cycle)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE workflow_attempts SET cost_known=1,usage_known=1,cost_usd=?,total_tokens=?,reserved_tokens=?,reserved_cost=? WHERE project=? AND id=?`, cost, tokens, tokens, cost, s.project, id)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"attempt": id, "cost_usd": cost, "tokens": tokens, "reference": reference})
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorHuman, Type: "billing.reconciled", Payload: string(payload)}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ChargeRepair(cycle int, ticket string, maxAttempts int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts int
	err = tx.QueryRow(`SELECT attempts FROM workflow_repairs WHERE project=? AND cycle=? AND ticket=?`, s.project, cycle, ticket).Scan(&attempts)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if attempts >= maxAttempts {
		return errors.New("ticket attempt limit exhausted")
	}
	if attempts > 0 {
		var raw string
		if err = tx.QueryRow(`SELECT policy FROM workflow_budgets WHERE project=? AND cycle=?`, s.project, cycle).Scan(&raw); err != nil {
			return err
		}
		var p ExecutionPolicy
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE workflow_budgets SET repairs=repairs+1 WHERE project=? AND cycle=? AND repairs<?`, s.project, cycle, p.MaxRepairs)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("cycle repair limit exhausted")
		}
	}
	_, err = tx.Exec(`INSERT INTO workflow_repairs(project,cycle,ticket,attempts) VALUES(?,?,?,1) ON CONFLICT(project,cycle,ticket) DO UPDATE SET attempts=attempts+1`, s.project, cycle, ticket)
	if err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "repair.attempt", Payload: ticket}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) StartSession(cycle int) (string, time.Time, error) {
	return s.StartBucketSession(cycle, BucketWorker)
}

// StartBucketSession brackets active time charged to one budget bucket.
func (s *Store) StartBucketSession(cycle int, bucket string) (string, time.Time, error) {
	b, err := s.BucketBudget(cycle, bucket)
	if err != nil {
		return "", time.Time{}, err
	}
	remaining := b.Policy.MaxActiveSeconds*1000 - b.ActiveMS
	if remaining <= 0 {
		return "", time.Time{}, errors.New("active-time budget exhausted")
	}
	start := time.Now()
	deadline := start.Add(time.Duration(remaining) * time.Millisecond)
	expires := start.Add(30 * time.Second)
	if expires.After(deadline) {
		expires = deadline
	}
	id := newID("work")
	_, err = s.db.Exec(`INSERT INTO workflow_sessions(id,project,cycle,started_ms,expires_ms,deadline_ms,bucket) VALUES(?,?,?,?,?,?,?)`, id, s.project, cycle, start.UnixMilli(), expires.UnixMilli(), deadline.UnixMilli(), bucketName(bucket))
	return id, deadline, err
}
func (s *Store) HeartbeatSession(id string) error {
	res, err := s.db.Exec(`UPDATE workflow_sessions SET expires_ms=MIN(deadline_ms,?) WHERE project=? AND id=? AND closed=0 AND expires_ms>?`, time.Now().Add(30*time.Second).UnixMilli(), s.project, id, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("active work lease expired")
	}
	return nil
}
func (s *Store) EndSession(id string, recovered bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cycle int
	var started, expires int64
	var bucket string
	if err = tx.QueryRow(`SELECT cycle,started_ms,expires_ms,bucket FROM workflow_sessions WHERE project=? AND id=? AND closed=0`, s.project, id).Scan(&cycle, &started, &expires, &bucket); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	table, err := budgetTable(bucket)
	if err != nil {
		return err
	}
	end := time.Now().UnixMilli()
	if recovered || end > expires {
		end = expires
	}
	if end < started {
		end = started
	}
	_, err = tx.Exec(fmt.Sprintf(`UPDATE %s SET active_ms=active_ms+? WHERE project=? AND cycle=?`, table), end-started, s.project, cycle)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE workflow_sessions SET closed=1 WHERE project=? AND id=?`, s.project, id)
	if err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "work.ended", Payload: fmt.Sprintf("%s recovered=%t charged_ms=%d", id, recovered, end-started)}); err != nil {
		return err
	}
	return tx.Commit()
}

// Called only under the workspace writer lock: any remaining session is abandoned.
func (s *Store) RecoverSessions() error {
	rows, err := s.db.Query(`SELECT id FROM workflow_sessions WHERE project=? AND closed=0`, s.project)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.EndSession(id, true); err != nil {
			return err
		}
	}
	return nil
}
