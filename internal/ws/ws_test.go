package ws

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/yanai/yanai-harness/internal/workflow"
)

// The phase constants here and workflow's are deliberately the same
// strings — SaveState's dispatch (phase changed vs. payload-only) and every
// ApplyCyclePhase call site depend on that. This pins it down so a typo in
// either package fails loudly instead of silently desyncing the two.
func TestPhasesMatchWorkflowConstants(t *testing.T) {
	pairs := []struct{ ws, wf string }{
		{PhaseEmpty, workflow.PhaseNoCycle},
		{PhaseAnalyzed, workflow.PhaseAnalyzed},
		{PhaseNoChange, workflow.PhaseNoChangeNeeded},
		{PhaseNeedsEvidence, workflow.PhaseNeedsEvidence},
		{PhaseOutOfScope, workflow.PhaseOutOfScope},
		{PhaseBlockedBaseline, workflow.PhaseBlockedByBaseline},
		{PhaseWaiting, workflow.PhaseAwaitingApproval},
		{PhaseApproved, workflow.PhaseApproved},
		{PhaseRejected, workflow.PhaseRejected},
		{PhaseAwaitingExecution, workflow.PhaseAwaitingExecution},
	}
	for _, p := range pairs {
		if p.ws != p.wf {
			t.Errorf("ws %q != workflow %q", p.ws, p.wf)
		}
	}
}

func openStoreBacked(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := workflow.OpenStore(filepath.Join(root, "workflow.db"), "test-project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	w.Store = store
	return w
}

func TestNewCycleAndSaveStateRoundTripThroughTheStore(t *testing.T) {
	w := openStoreBacked(t)
	st, err := w.NewCycle(workflow.Product)
	if err != nil {
		t.Fatal(err)
	}
	if st.Cycle != 1 || st.Phase != PhaseEmpty || st.StateVersion != 1 {
		t.Fatalf("new cycle = %+v", st)
	}
	st.BaseCommit = "abc123"
	if err := w.SaveState(st, workflow.ActorEngine); err != nil {
		t.Fatal(err)
	}
	if st.StateVersion != 2 {
		t.Fatalf("state_version after a payload-only save = %d, want 2", st.StateVersion)
	}
	st.Phase = PhaseAnalyzed
	if err := w.SaveState(st, workflow.ActorEngine); err != nil {
		t.Fatal(err)
	}
	if st.StateVersion != 3 {
		t.Fatalf("state_version after a phase change = %d, want 3", st.StateVersion)
	}

	reloaded, err := w.LoadCycleState(1)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Phase != PhaseAnalyzed || reloaded.BaseCommit != "abc123" || reloaded.StateVersion != 3 {
		t.Fatalf("reloaded = %+v", reloaded)
	}
}

func TestSaveStateRejectsAStaleVersion(t *testing.T) {
	w := openStoreBacked(t)
	st, err := w.NewCycle(workflow.Product)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a second process racing ahead: commit directly through the
	// store while the caller's own *State (below) still holds the old
	// version.
	if _, err := w.Store.SetCyclePayload(st.Cycle, st.StateVersion, workflow.ActorEngine, workflow.CycleFields{BaseCommit: "raced-ahead"}, workflow.Event{}); err != nil {
		t.Fatal(err)
	}

	st.BaseCommit = "stale-write"
	err = w.SaveState(st, workflow.ActorEngine)
	if err == nil {
		t.Fatal("a stale write silently overwrote a concurrent commit")
	}
	var stale *workflow.ErrStaleVersion
	if !errors.As(err, &stale) {
		t.Fatalf("error type = %T, want *workflow.ErrStaleVersion", err)
	}

	reloaded, err := w.LoadCycleState(st.Cycle)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.BaseCommit != "raced-ahead" {
		t.Fatalf("the racing commit was lost: base_commit = %q", reloaded.BaseCommit)
	}
}

func TestLegacyPathIsUnaffectedByAnAttachedStoreOnADifferentWorkspace(t *testing.T) {
	// A bare ws.Open (Store == nil) must behave exactly as it always has:
	// this is what lets a test (or yanai import) construct a legacy,
	// pre-store cycle by hand.
	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := w.NewCycle(workflow.Product)
	if err != nil {
		t.Fatal(err)
	}
	if st.StateVersion != 0 {
		t.Fatalf("a legacy cycle should never carry a store version, got %d", st.StateVersion)
	}
	st.Phase = PhaseWaiting
	if err := w.SaveState(st, workflow.ActorEngine); err != nil {
		t.Fatal(err)
	}
	reloaded, err := w.LoadCycleState(st.Cycle)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Phase != PhaseWaiting {
		t.Fatalf("reloaded legacy state = %+v", reloaded)
	}

	legacy, err := w.LegacyCycles()
	if err != nil {
		t.Fatal(err)
	}
	if legacy != nil {
		t.Fatalf("LegacyCycles with no store attached should report nothing to import, got %v", legacy)
	}
}

func TestLegacyCyclesDetectsAFileEraCycleNotYetImported(t *testing.T) {
	root := t.TempDir()
	bare, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.NewCycle(workflow.Product); err != nil {
		t.Fatal(err)
	}

	attached := openStoreBacked(t)
	attached.Root = root // same on-disk cycles/, now viewed with a store attached
	legacy, err := attached.LegacyCycles()
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 1 || legacy[0] != 1 {
		t.Fatalf("LegacyCycles = %v, want [1]", legacy)
	}

	if _, err := attached.Store.ImportLegacyCycle(1, PhaseWaiting, "NUEVO_PLAN", workflow.Product, "{}"); err != nil {
		t.Fatal(err)
	}
	legacy, err = attached.LegacyCycles()
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 0 {
		t.Fatalf("an imported cycle is still reported as legacy: %v", legacy)
	}
}
