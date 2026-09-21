package workflow

import (
	"encoding/json"
	"testing"
	"time"
)

func approvedContract(t *testing.T) (*Store, string, RepositoryState) {
	t.Helper()
	s := budgetStore(t)
	cycle, _ := s.GetCycle(1)
	cycle, err := s.ApplyCyclePhase(1, PhaseAnalyzed, ActorEngine, cycle.StateVersion, CycleFields{}, Event{})
	if err != nil {
		t.Fatal(err)
	}
	cycle, err = s.ApplyCyclePhase(1, PhaseAwaitingApproval, ActorEngine, cycle.StateVersion, CycleFields{}, Event{})
	if err != nil {
		t.Fatal(err)
	}
	repo := RepositoryState{Root: "/yanai", CommonDir: "/yanai/.git", Head: "head", Content: map[string]string{"yanai-server/a.go": "old", "yanai-server/other.go": "unchanged"}, IndexHash: "index", StatusHash: "clean"}
	raw, _ := json.Marshal(repo)
	c := ExecutionContract{Version: 1, Plan: Proposal{ID: "p", Tickets: []Ticket{{ID: "T-1", Outputs: []string{"yanai-server/a.go"}, AllowedPaths: []string{"yanai-server"}}}}, ContextHash: "context", Repository: raw, Baseline: repo.Baseline(), Policy: testPolicy()}
	hash, err := s.SaveContract(1, c)
	if err != nil {
		t.Fatal(err)
	}
	ph, _ := Hash(c.Plan)
	a := Approval{ID: "A-1", Cycle: 1, Actor: ActorHuman, ContractHash: hash, PlanHash: ph, ScopeHash: c.ContextHash, Baseline: c.Baseline, ApprovedAt: time.Now()}
	if err = s.ApproveContract(a, cycle.StateVersion-1, "{}"); err == nil {
		t.Fatal("stale approval committed")
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM workflow_approvals`).Scan(&count)
	if count != 0 {
		t.Fatal("partial approval survived rollback")
	}
	if err = s.ApproveContract(a, cycle.StateVersion, "{}"); err != nil {
		t.Fatal(err)
	}
	return s, hash, repo
}
func copyRepo(r RepositoryState) RepositoryState {
	n := r
	n.Content = map[string]string{}
	for k, v := range r.Content {
		n.Content[k] = v
	}
	return n
}
func TestOwnPatchSuccessorsAndInterruptedRecovery(t *testing.T) {
	s, hash, base := approvedContract(t)
	next := copyRepo(base)
	next.Content["yanai-server/a.go"] = "first"
	next.Dirty = true
	next.StatusHash = "dirty"
	if err := s.PreparePatch(1, hash, "T-1", next, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PatchState(1, hash); err == nil {
		t.Fatal("pending patch silently authorized")
	}
	external := copyRepo(next)
	external.Content["yanai-server/other.go"] = "external"
	if err := s.ReconcilePatch(1, hash, external); err == nil {
		t.Fatal("external edit accepted")
	}
	if err := s.ReconcilePatch(1, hash, next); err != nil {
		t.Fatal(err)
	}
	current, revision, err := s.PatchState(1, hash)
	if err != nil || current.Baseline() != next.Baseline() {
		t.Fatalf("patch state %v", err)
	}
	second := copyRepo(next)
	second.Content["yanai-server/a.go"] = "second"
	if err = s.PreparePatch(1, hash, "T-1", second, revision); err != nil {
		t.Fatal(err)
	}
	// Process died before applying the second patch: exact pre-state is safe.
	if err = s.ReconcilePatch(1, hash, next); err != nil {
		t.Fatal(err)
	}
	current, revision, _ = s.PatchState(1, hash)
	if current.Baseline() != next.Baseline() {
		t.Fatal("unapplied patch advanced current content")
	}
	if err = s.PreparePatch(1, hash, "T-1", external, revision); err == nil {
		t.Fatal("outside-ticket path authorized")
	}
	if err = s.PreparePatch(1, hash, "T-1", second, revision-1); err == nil {
		t.Fatal("stale revision authorized")
	}
}
func TestPolicyRevisionRevokesActiveApprovalWithoutDeletingHistory(t *testing.T) {
	s, hash, _ := approvedContract(t)
	if err := s.HasApproval(1, hash); err != nil {
		t.Fatal(err)
	}
	p := testPolicy()
	p.MaxCalls++
	if err := s.RevisePolicy(1, p, "review additional calls"); err != nil {
		t.Fatal(err)
	}
	if err := s.HasApproval(1, hash); err == nil {
		t.Fatal("old authorization remains active")
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM workflow_approvals`).Scan(&count)
	if count != 1 {
		t.Fatal("approval history removed")
	}
}

func TestNoActorCanPromoteAModelCandidateToVerified(t *testing.T) {
	s := budgetStore(t)
	rec, err := s.SaveTicket(1, ticket("T-1", "ingeniero"))
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{TicketPending, TicketClaimed, TicketResponseRecorded, TicketCandidateReady} {
		if state != TicketPending {
			rec, err = s.ApplyTicketStatus(1, "T-1", state, ActorEngine, rec.StateVersion, Event{})
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, actor := range []string{ActorEngine, ActorHuman, "ingeniero"} {
			if _, err = s.ApplyTicketStatus(1, "T-1", "verified", actor, rec.StateVersion, Event{}); err == nil {
				t.Fatalf("%s promoted %s to verified", actor, state)
			}
		}
	}
}
