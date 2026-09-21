package workflow

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPolicy() ExecutionPolicy {
	return ExecutionPolicy{MaxTokens: 1000, MaxCostUSD: 2, MaxCalls: 3, MaxRepairs: 1, MaxActiveSeconds: 60, Prices: map[string]ModelPrice{"model": {Input: 1, Output: 2}}, Checks: []Check{{ID: "test", Args: []string{"go", "test", "./example"}, Dir: "yanai-server", TimeoutSeconds: 10}}}
}
func budgetStore(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t, "yanai")
	if _, err := s.CreateCycle(1, Product); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBudget(1, testPolicy()); err != nil {
		t.Fatal(err)
	}
	return s
}
func reservation() Reservation {
	return Reservation{AttemptInput: AttemptInput{Cycle: 1, Role: "ingeniero", Kind: "model_request", RequestHash: "request"}, Model: "model", Tokens: 300, Cost: .5}
}
func TestBudgetReservationUnknownCostAndReconciliation(t *testing.T) {
	s := budgetStore(t)
	id, err := s.ReserveCall(reservation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveCall(reservation()); err == nil {
		t.Fatal("in-flight exposure allowed another dispatch")
	}
	if err = s.SettleCall(id, CallResult{State: AttemptCompleted, Usage: Usage{TotalTokens: 20}, UsageKnown: true}); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAttempt(id)
	if err != nil || a.CostKnown || a.CostUSD != nil {
		t.Fatalf("missing cost became zero: %+v %v", a, err)
	}
	if _, err = s.ReserveCall(reservation()); err == nil {
		t.Fatal("unknown billing allowed dispatch")
	}
	if err = s.ReconcileBilling(id, .2, 20, "invoice-1"); err != nil {
		t.Fatal(err)
	}
	b, err := s.Budget(1)
	if err != nil || b.Tokens != 20 || b.Cost != .2 || b.Calls != 1 || b.Unknown != 0 {
		t.Fatalf("budget %+v %v", b, err)
	}
	if err = s.ReconcileBilling(id, 0, 0, "erase charge"); err == nil {
		t.Fatal("reconciled charge overwritten")
	}
	if _, err = s.ReserveCall(reservation()); err != nil {
		t.Fatal(err)
	}
}
func TestAcknowledgementDoesNotReleaseExposure(t *testing.T) {
	s := budgetStore(t)
	id, err := s.ReserveCall(reservation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileAttempts(); err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveAttempt(id); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Budget(1)
	if b.Cost != .5 || b.Tokens != 300 || b.Unknown != 1 {
		t.Fatalf("ack released exposure: %+v", b)
	}
	if _, err = s.ReserveCall(reservation()); err == nil {
		t.Fatal("ack bypassed billing")
	}
	if err = s.ReconcileBilling(id, .3, 25, "invoice"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveCall(reservation()); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentReservationsAndEveryLimit(t *testing.T) {
	s := budgetStore(t)
	var wg sync.WaitGroup
	wins := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := s.ReserveCall(reservation()); err == nil {
				wins <- id
			}
		}()
	}
	wg.Wait()
	close(wins)
	if len(wins) != 1 {
		t.Fatalf("reservation winners: %d", len(wins))
	}
	for id := range wins {
		cost := .5
		if err := s.SettleCall(id, CallResult{State: AttemptFailed, Cost: &cost, UsageKnown: true, Usage: Usage{TotalTokens: 300}, FinishReason: "length"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"tokens", "cost", "calls", "active_ms"} {
		t.Run(field, func(t *testing.T) {
			s := budgetStore(t)
			limits := map[string]any{"tokens": 1000, "cost": 2, "calls": 3, "active_ms": 60000}
			if _, err := s.db.Exec("UPDATE workflow_budgets SET "+field+"=?", limits[field]); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReserveCall(reservation()); err == nil {
				t.Fatal("exhausted " + field + " allowed dispatch")
			}
		})
	}
}
func TestRepairLimitsAndPolicyRevisionPreserveUsage(t *testing.T) {
	s := budgetStore(t)
	if err := s.ChargeRepair(1, "T-1", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.ChargeRepair(1, "T-1", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.ChargeRepair(1, "T-1", 2); err == nil {
		t.Fatal("ticket repair cap ignored")
	}
	if err := s.ChargeRepair(1, "T-2", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.ChargeRepair(1, "T-2", 3); err == nil {
		t.Fatal("cycle repair cap ignored")
	}
	p := testPolicy()
	p.MaxCalls = 9
	if err := s.EnsureBudget(1, p); err == nil {
		t.Fatal("silent policy increase")
	}
	before, _ := s.Budget(1)
	if err := s.RevisePolicy(1, p, "additional reviewed allowance"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Budget(1)
	if after.Repairs != before.Repairs || after.Policy.MaxCalls != 9 {
		t.Fatal("revision reset consumption")
	}
}
func TestActiveTimeRecoveryChargesLeaseAndExcludesReview(t *testing.T) {
	s := budgetStore(t)
	id, deadline, err := s.StartSession(1)
	if err != nil {
		t.Fatal(err)
	}
	if deadline.Before(time.Now()) {
		t.Fatal("bad deadline")
	}
	if err = s.HeartbeatSession(id); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverSessions(); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Budget(1)
	if b.ActiveMS < 30000 || b.ActiveMS > 31000 {
		t.Fatalf("recovery charge %d", b.ActiveMS)
	}
	if err = s.RecoverSessions(); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Budget(1)
	if again.ActiveMS != b.ActiveMS {
		t.Fatal("recovery double charged")
	}
	id, _, err = s.StartSession(1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EndSession(id, false); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Budget(1)
	if after.ActiveMS > b.ActiveMS+1000 {
		t.Fatal("normal close charged idle review time")
	}
}
func TestMigrationDoesNotInventHistoricalBilling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE workflow_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);INSERT INTO workflow_meta VALUES('schema_version','2')`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:2] {
		if _, err = db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.Exec(`INSERT INTO workflow_cycles(project,cycle,phase,created_at,updated_at) VALUES('yanai',1,'approved','x','x');INSERT INTO workflow_attempts(id,project,cycle,role,kind,request_hash,state,cost_known,total_tokens,started_at) VALUES('old','yanai',1,'ingeniero','role_turn','r','completed',1,42,'x')`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := OpenStore(path, "yanai")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.EnsureBudget(1, testPolicy()); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAttempt("old")
	if err != nil || a.CostKnown {
		t.Fatalf("historical billing %+v %v", a, err)
	}
	b, _ := s.Budget(1)
	if b.Calls != 1 || b.Tokens != 42 {
		t.Fatalf("historical usage lost %+v", b)
	}
	if _, err = s.ReserveCall(reservation()); err == nil || !strings.Contains(err.Error(), "unreconciled") {
		t.Fatal(err)
	}
}
func TestBudgetPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(path, "yanai")
	if err != nil {
		t.Fatal(err)
	}
	s.CreateCycle(1, Product)
	s.EnsureBudget(1, testPolicy())
	id, err := s.ReserveCall(reservation())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path, "yanai")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.ReconcileAttempts()
	a, _ := s.GetAttempt(id)
	if a.State != AttemptUnknown {
		t.Fatal(a.State)
	}
	b, _ := s.Budget(1)
	raw, _ := json.Marshal(b)
	if b.Calls != 1 || b.Cost != .5 || b.Tokens != 300 {
		t.Fatal(string(raw))
	}
}
