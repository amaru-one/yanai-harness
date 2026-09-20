package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// openWorkflowStore opens the same workflow.db a CLI command against this
// workspace would, under the same project identifier attachStore derives
// (cfg.Project, or the workspace's basename) — a mismatched project string
// would make OpenStore refuse the reopen outright.
func openWorkflowStore(t *testing.T, workspace string) *workflow.Store {
	t.Helper()
	cfg, err := config.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	project := strings.TrimSpace(cfg.Project)
	if project == "" {
		abs, err := filepath.Abs(workspace)
		if err != nil {
			t.Fatal(err)
		}
		project = filepath.Base(abs)
	}
	store, err := workflow.OpenStore(filepath.Join(workspace, "workflow.db"), project)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestConcurrentWorkspaceLockRejectsASecondProcess exercises the workspace
// writer lock this step adds (internal/ws's LockWriter, taken by
// attachStore): a second process attached to the same workspace must be
// refused outright, not race the first for the store.
func TestConcurrentWorkspaceLockRejectsASecondProcess(t *testing.T) {
	_, workspace := approvedCycle(t)
	w, err := ws.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := w.LockWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if err := cmdRun([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "already using this workspace") {
		t.Fatalf("concurrent access: %v", err)
	}
	if err := cmdStatus([]string{"--ws", workspace}); err != nil {
		t.Fatalf("status should still work read-only despite the lock: %v", err)
	}
}

// TestAnalyzeReplayIsIdempotentAndCallsNoModel proves the exact same
// reviewed source, scope and baseline as an already-completed analyze opens
// no second cycle and never reaches the provider a second time.
func TestAnalyzeReplayIsIdempotentAndCallsNoModel(t *testing.T) {
	workspace, input, provider := decisionSetup(t, "A synthetic teacher need, once.", func(c workflow.DecisionContext, _ int) string {
		return encode(proposalFor(c, workflow.OutcomeProposeChange))
	})
	if err := cmdAnalyze(analyzeArgs(workspace, input)); err != nil {
		t.Fatal(err)
	}
	before := load(t, workspace)

	provider.mu.Lock()
	callsBefore := provider.decisions
	provider.mu.Unlock()

	if err := cmdAnalyze(analyzeArgs(workspace, input)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	after := load(t, workspace)
	if after.Cycle != before.Cycle {
		t.Fatalf("replay opened a second cycle: %d -> %d", before.Cycle, after.Cycle)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.decisions != callsBefore {
		t.Fatalf("replay called the model again: %d -> %d decisions", callsBefore, provider.decisions)
	}
}

// TestDiscussReplayIsIdempotentAndCallsNoModel is the same guarantee at the
// consolidated-plan stage: re-running discuss against an unchanged
// proposal/scope/baseline returns the same plan without re-running the
// specialist reviews or the PO's consolidation.
func TestDiscussReplayIsIdempotentAndCallsNoModel(t *testing.T) {
	workspace, input, provider := decisionSetup(t, "A synthetic teacher need, once.", func(c workflow.DecisionContext, _ int) string {
		return encode(proposalFor(c, workflow.OutcomeProposeChange))
	})
	if err := cmdAnalyze(analyzeArgs(workspace, input)); err != nil {
		t.Fatal(err)
	}
	if err := cmdDiscuss([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	before := load(t, workspace)

	provider.mu.Lock()
	requestsBefore := len(provider.requests)
	provider.mu.Unlock()

	if err := cmdDiscuss([]string{"--ws", workspace}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	after := load(t, workspace)
	if after.PlanHash != before.PlanHash {
		t.Fatal("replay produced a different plan")
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != requestsBefore {
		t.Fatalf("replay called the model again: %d -> %d requests", requestsBefore, len(provider.requests))
	}
}

// TestApproveReplayIsIdempotent covers §5.5's third row: re-approving an
// already-approved cycle under the same plan/scope/baseline is a no-op, not
// an error a retried script would have to special-case.
func TestApproveReplayIsIdempotent(t *testing.T) {
	_, workspace := approvedCycle(t)
	if err := cmdApprove([]string{"--ws", workspace}); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

// TestUnresolvedAttemptBlocksRunAndDiscussUntilAcknowledged simulates what
// reconciliation finds after a process dies mid-call: an attempt row stuck
// in_flight. Both run and discuss must refuse until --retry-unresolved.
func TestUnresolvedAttemptBlocksRunAndDiscussUntilAcknowledged(t *testing.T) {
	_, workspace := approvedCycle(t)
	store := openWorkflowStore(t, workspace)
	id, err := store.BeginAttempt(workflow.AttemptInput{Cycle: 1, TicketID: "T-001", Role: "ingeniero", Kind: "role_turn", RequestHash: "r"})
	if err != nil {
		t.Fatal(err)
	}
	store.Close() // the "process" holding this attempt is gone

	if err := cmdRun([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("run should refuse on an unresolved attempt: %v", err)
	}
	if err := cmdDiscuss([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("discuss should refuse on an unresolved attempt: %v", err)
	}

	if err := cmdRun([]string{"--ws", workspace, "--retry-unresolved"}); err != nil {
		t.Fatalf("run --retry-unresolved: %v", err)
	}

	store2 := openWorkflowStore(t, workspace)
	defer store2.Close()
	a, err := store2.GetAttempt(id)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != workflow.AttemptFailed {
		t.Fatalf("acknowledged attempt state = %q, want %q", a.State, workflow.AttemptFailed)
	}
	if unresolved, err := store2.UnresolvedAttempts(); err != nil || len(unresolved) != 0 {
		t.Fatalf("unresolved = %v, %v, want none", unresolved, err)
	}
}

// TestConcurrentClaimsThroughTheCLIProduceExactlyOneWinner drives the same
// race internal/workflow's store_test.go covers, but through ClaimTicket as
// team.Execute actually calls it, with the ttl a real run would use.
func TestConcurrentClaimsThroughTheCLIProduceExactlyOneWinner(t *testing.T) {
	_, workspace := approvedCycle(t)
	store := openWorkflowStore(t, workspace)
	defer store.Close()

	const holders = 6
	results := make(chan error, holders)
	for i := 0; i < holders; i++ {
		holder := "holder"
		go func(n int) {
			_, _, err := store.ClaimTicket(1, "T-001", holder+string(rune('A'+n)), time.Minute, workflow.Event{})
			results <- err
		}(i)
	}
	wins := 0
	for i := 0; i < holders; i++ {
		if <-results == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}
}

// TestArtifactReconciliationRunsThroughAttachStore proves attachStore's
// reconciliation reaches artifact rows too, the same way
// TestUnresolvedAttemptBlocksRunAndDiscussUntilAcknowledged proves it for
// attempts: a pending row left by a process that died between linking the
// file and marking it published is flipped to published the next time any
// command opens the workspace store.
func TestArtifactReconciliationRunsThroughAttachStore(t *testing.T) {
	_, workspace := approvedCycle(t)
	store := openWorkflowStore(t, workspace)

	content := []byte("deliverable")
	path := "entregables/one.md"
	if _, err := store.BeginArtifact(1, workflow.ArtifactRef{ID: "a-1", Path: path, SHA256: contentHash(content)}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(workspace, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		t.Fatal(err)
	}
	store.Close() // the "process" that would have finished the publish is gone

	if err := cmdStatus([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}

	store2 := openWorkflowStore(t, workspace)
	defer store2.Close()
	rec, err := store2.GetArtifact(1, "a-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != "published" {
		t.Fatalf("artifact state = %q, want published", rec.State)
	}
}
