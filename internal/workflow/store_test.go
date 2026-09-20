package workflow

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T, project string) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "workflow.db"), project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenStoreBindsProjectAndRejectsMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	s, err := OpenStore(path, "yanai")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := OpenStore(path, "yanai"); err != nil {
		t.Fatalf("reopening the same project: %v", err)
	}
	if _, err := OpenStore(path, "other-project"); err == nil {
		t.Fatal("a mismatched project was accepted")
	}
	if _, err := OpenStore(path, ""); err == nil {
		t.Fatal("an empty project was accepted")
	}
}

func TestMigrationRefusesANewerSchema(t *testing.T) {
	s := openTestStore(t, "yanai")
	if _, err := s.db.Exec(`UPDATE workflow_meta SET value = '999' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	path := s.path
	s.Close()
	if _, err := OpenStore(path, "yanai"); err == nil {
		t.Fatal("an old binary accepted a newer schema")
	}
}

func TestCyclePhaseTransitionIsConditionalAndActorChecked(t *testing.T) {
	s := openTestStore(t, "yanai")
	c, err := s.CreateCycle(1, PhaseAnalyzed, Product)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateVersion != 1 {
		t.Fatalf("state_version = %d, want 1", c.StateVersion)
	}

	// The model's verdict is an engine-requested transition, not a human act.
	if _, err := s.ApplyCyclePhase(1, PhaseApproved, ActorEngine, c.StateVersion, CycleFields{}, Event{}); err == nil {
		t.Fatal("engine reached the human-only phase 'approved'")
	}
	var notAllowed *ErrTransitionNotAllowed
	if _, err := s.ApplyCyclePhase(1, PhaseApproved, ActorEngine, c.StateVersion, CycleFields{}, Event{}); !errors.As(err, &notAllowed) {
		t.Fatalf("error type = %T, want *ErrTransitionNotAllowed", err)
	}

	c, err = s.ApplyCyclePhase(1, PhaseAwaitingApproval, ActorEngine, c.StateVersion, CycleFields{PlanHash: "p1"}, Event{Type: "plan.consolidated"})
	if err != nil {
		t.Fatal(err)
	}
	if c.StateVersion != 2 || c.PlanHash != "p1" {
		t.Fatalf("cycle after transition: %+v", c)
	}

	// A stale expected version is refused, not silently overwritten.
	var stale *ErrStaleVersion
	if _, err := s.ApplyCyclePhase(1, PhaseApproved, ActorHuman, 1, CycleFields{}, Event{}); !errors.As(err, &stale) {
		t.Fatalf("error type = %T, want *ErrStaleVersion", err)
	}

	// The human gate itself works with the current version.
	c, err = s.ApplyCyclePhase(1, PhaseApproved, ActorHuman, c.StateVersion, CycleFields{}, Event{Type: "plan.approved"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Phase != PhaseApproved {
		t.Fatalf("phase = %q, want approved", c.Phase)
	}
}

func TestTerminalPhasesAreReachableFromAnalyzedAndRejectedOnly(t *testing.T) {
	for _, terminal := range TerminalPhases {
		if !actorAllowed(cycleTransitions, PhaseNoCycle, terminal, ActorEngine) {
			t.Errorf("no_cycle -> %s should be reachable on first analysis", terminal)
		}
		if !actorAllowed(cycleTransitions, PhaseAnalyzed, terminal, ActorEngine) {
			t.Errorf("analyzed -> %s should be reachable", terminal)
		}
		if !actorAllowed(cycleTransitions, PhaseRejected, terminal, ActorEngine) {
			t.Errorf("rejected -> %s should be reachable after a second look", terminal)
		}
		if actorAllowed(cycleTransitions, terminal, PhaseAwaitingApproval, ActorEngine) {
			t.Errorf("%s should be terminal: it has no outgoing edge", terminal)
		}
	}
}

func TestOnlyAHumanReachesApprovedOrRejected(t *testing.T) {
	for _, to := range []string{PhaseApproved, PhaseRejected} {
		if actorAllowed(cycleTransitions, PhaseAwaitingApproval, to, ActorEngine) {
			t.Errorf("engine was allowed to reach %s", to)
		}
		if !actorAllowed(cycleTransitions, PhaseAwaitingApproval, to, ActorHuman) {
			t.Errorf("human was not allowed to reach %s", to)
		}
	}
}

func TestTicketLifecycleClaimAndValidation(t *testing.T) {
	s := openTestStore(t, "yanai")
	if _, err := s.CreateCycle(1, PhaseAnalyzed, Product); err != nil {
		t.Fatal(err)
	}
	rec, err := s.SaveTicket(1, ticket("T-1", "ingeniero"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != TicketPending || rec.StateVersion != 1 {
		t.Fatalf("new ticket = %+v", rec)
	}

	rec, claim, err := s.ClaimTicket(1, "T-1", "host-a/pid-1", time.Minute, Event{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != TicketClaimed || claim.Holder != "host-a/pid-1" {
		t.Fatalf("claimed ticket = %+v claim = %+v", rec, claim)
	}

	// A different holder cannot claim it while the lease is live.
	if _, _, err := s.ClaimTicket(1, "T-1", "host-b/pid-2", time.Minute, Event{}); !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("second claim: %v, want ErrClaimHeld", err)
	}

	if err := s.Heartbeat(1, "T-1", "host-a/pid-1", time.Minute); err != nil {
		t.Fatal(err)
	}

	rec, err = s.ApplyTicketStatus(1, "T-1", TicketResponseRecorded, ActorEngine, rec.StateVersion, Event{Type: "response.recorded"})
	if err != nil {
		t.Fatal(err)
	}
	rec, err = s.ApplyTicketStatus(1, "T-1", TicketCandidateReady, ActorEngine, rec.StateVersion, Event{Type: "candidate.ready"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != TicketCandidateReady {
		t.Fatalf("status = %q, want candidate_ready", rec.Status)
	}

	// candidate_ready has no outgoing edge in this step.
	if _, err := s.ApplyTicketStatus(1, "T-1", TicketPending, ActorEngine, rec.StateVersion, Event{}); err == nil {
		t.Fatal("candidate_ready was reopened")
	}
}

func TestResponseRejectedReturnsToPendingForARetry(t *testing.T) {
	s := openTestStore(t, "yanai")
	s.CreateCycle(1, PhaseAnalyzed, Product)
	rec, _ := s.SaveTicket(1, ticket("T-1", "ingeniero"))
	rec, _, err := s.ClaimTicket(1, "T-1", "h", time.Minute, Event{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err = s.ApplyTicketStatus(1, "T-1", TicketResponseRecorded, ActorEngine, rec.StateVersion, Event{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err = s.ApplyTicketStatus(1, "T-1", TicketResponseRejected, ActorEngine, rec.StateVersion, Event{Type: "response.rejected"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTicketStatus(1, "T-1", TicketPending, ActorEngine, rec.StateVersion, Event{Type: "retry"}); err != nil {
		t.Fatalf("response_rejected should return to pending for a retry: %v", err)
	}
}

func TestLegacyUnverifiedTicketHasNoOutgoingEdge(t *testing.T) {
	for _, to := range []string{TicketPending, TicketClaimed, TicketResponseRecorded, TicketCandidateReady, TicketResponseRejected} {
		if actorAllowed(ticketTransitions, TicketLegacyUnverified, to, ActorEngine) {
			t.Errorf("legacy_unverified -> %s should not be a transition", to)
		}
	}
}

func TestClaimExpiryReturnsTicketToPending(t *testing.T) {
	s := openTestStore(t, "yanai")
	s.CreateCycle(1, PhaseAnalyzed, Product)
	s.SaveTicket(1, ticket("T-1", "ingeniero"))
	if _, _, err := s.ClaimTicket(1, "T-1", "h", -time.Second, Event{}); err != nil {
		// Negative TTL: the lease is already expired the instant it's taken.
		t.Fatal(err)
	}
	expired, err := s.ExpireClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != "T-1" || expired[0].Status != TicketPending {
		t.Fatalf("expired = %+v", expired)
	}
	// The ticket can be claimed again now that the lease is gone.
	if _, _, err := s.ClaimTicket(1, "T-1", "other-holder", time.Minute, Event{}); err != nil {
		t.Fatalf("reclaiming after expiry: %v", err)
	}
}

// TestConcurrentClaimsProduceExactlyOneWinner drives real goroutine
// concurrency at the store, not just sequential calls: many holders race for
// one ticket, and the single-connection pool (see OpenStore) makes SQLite
// serialize them, so this asserts the *outcome* of that serialization is
// correct — exactly one winner, no duplicate transition, no lost update —
// rather than merely that the calls don't crash.
func TestConcurrentClaimsProduceExactlyOneWinner(t *testing.T) {
	s := openTestStore(t, "yanai")
	s.CreateCycle(1, PhaseAnalyzed, Product)
	s.SaveTicket(1, ticket("T-1", "ingeniero"))

	const holders = 8
	results := make(chan error, holders)
	for i := 0; i < holders; i++ {
		holder := fmt.Sprintf("holder-%d", i)
		go func() {
			_, _, err := s.ClaimTicket(1, "T-1", holder, time.Minute, Event{})
			results <- err
		}()
	}
	wins, losses := 0, 0
	for i := 0; i < holders; i++ {
		switch err := <-results; {
		case err == nil:
			wins++
		case errors.Is(err, ErrClaimHeld):
			losses++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if wins != 1 || losses != holders-1 {
		t.Fatalf("wins=%d losses=%d, want exactly one winner", wins, losses)
	}
	rec, err := s.GetTicket(1, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != TicketClaimed || rec.StateVersion != 2 {
		t.Fatalf("ticket after the race = %+v, want claimed at version 2", rec)
	}
}

func TestAttemptLedgerRecordsUnresolvedWorkOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	s, err := OpenStore(path, "yanai")
	if err != nil {
		t.Fatal(err)
	}
	s.CreateCycle(1, PhaseAnalyzed, Product)
	id, err := s.BeginAttempt(AttemptInput{Cycle: 1, TicketID: "T-1", Role: "ingeniero", Kind: "role_turn", RequestHash: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	// The process "dies" here: no CompleteAttempt/FailAttempt is ever called.
	s.Close()

	s2, err := OpenStore(path, "yanai")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	unresolved, err := s2.ReconcileAttempts()
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 1 || unresolved[0].ID != id {
		t.Fatalf("reconciled = %+v", unresolved)
	}
	if unresolved[0].State != AttemptUnknown || unresolved[0].CostKnown {
		t.Fatalf("attempt = %+v, want state=unknown cost_known=false", unresolved[0])
	}

	listed, err := s2.UnresolvedAttempts()
	if err != nil || len(listed) != 1 || listed[0].ID != id {
		t.Fatalf("UnresolvedAttempts = %+v, %v", listed, err)
	}

	// CompleteAttempt cannot resurrect an attempt already stamped unknown.
	if err := s2.CompleteAttempt(id, "resp-hash", Usage{TotalTokens: 10}); err == nil {
		t.Fatal("completed an attempt reconciliation already marked unknown")
	}

	if err := s2.ResolveAttempt(id); err != nil {
		t.Fatal(err)
	}
	if listed, err := s2.UnresolvedAttempts(); err != nil || len(listed) != 0 {
		t.Fatalf("after ResolveAttempt: %+v, %v", listed, err)
	}
}

func TestCompletedAttemptRecordsUsageAndIsNotReconciled(t *testing.T) {
	s := openTestStore(t, "yanai")
	s.CreateCycle(1, PhaseAnalyzed, Product)
	id, err := s.BeginAttempt(AttemptInput{Cycle: 1, Role: "ingeniero", Kind: "role_turn", RequestHash: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAttempt(id, "resp", Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}); err != nil {
		t.Fatal(err)
	}
	unresolved, err := s.ReconcileAttempts()
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("a completed attempt was reconciled as unresolved: %+v", unresolved)
	}
	a, err := s.GetAttempt(id)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != AttemptCompleted || !a.CostKnown || a.TotalTokens != 7 {
		t.Fatalf("attempt = %+v", a)
	}
}

func TestAppendEventRejectsDuplicateIdempotencyKey(t *testing.T) {
	s := openTestStore(t, "yanai")
	s.CreateCycle(1, PhaseAnalyzed, Product)
	e := Event{Type: "ticket.created", IdempotencyKey: "k-1", Actor: ActorEngine}
	if err := s.AppendEvent(e); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(e); err == nil {
		t.Fatal("duplicate idempotency key accepted")
	}
}

func TestRecordApprovalRequiresHashes(t *testing.T) {
	s := openTestStore(t, "yanai")
	s.CreateCycle(1, PhaseAnalyzed, Product)
	if err := s.RecordApproval(Approval{ID: "A-1", Cycle: 1, Actor: "human"}); err == nil {
		t.Fatal("approval accepted without plan/scope/baseline hashes")
	}
	if err := s.RecordApproval(Approval{ID: "A-1", Cycle: 1, Actor: "human", PlanHash: "p", ScopeHash: "s", Baseline: "b"}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandIdempotency(t *testing.T) {
	s := openTestStore(t, "yanai")
	key := "analyze/" + Digest("same input")
	if _, found, err := s.CheckCommand(key); err != nil || found {
		t.Fatalf("found=%v err=%v, want not found", found, err)
	}
	if err := s.RecordCommand(key, "analyze", "cycle-1"); err != nil {
		t.Fatal(err)
	}
	result, found, err := s.CheckCommand(key)
	if err != nil || !found || result != "cycle-1" {
		t.Fatalf("result=%q found=%v err=%v", result, found, err)
	}
	if err := s.RecordCommand(key, "analyze", "cycle-2"); err == nil {
		t.Fatal("a second recording of the same command key was accepted")
	}
}
