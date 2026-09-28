package team

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// Reject sends the proposal back with the human's reason. The live
// configuration and the target repository were never changed by planning,
// so nothing needs undoing.
func (r *Runner) Reject(note string) (*ws.State, error) {
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	if st.Phase != ws.PhaseWaiting && st.Phase != ws.PhaseAnalyzed {
		return nil, fmt.Errorf("cycle %03d is in phase %q; only a plan in %q can be rejected (use 'yanai invalidate' for approved work)", st.Cycle, st.Phase, ws.PhaseWaiting)
	}
	if strings.TrimSpace(note) == "" {
		return nil, fmt.Errorf("a rejection needs a reason: use --note \"...\"")
	}
	st.Phase = ws.PhaseRejected
	st.Log("plan REJECTED by the human", "human", note)
	text := fmt.Sprintf("# Approval\n\nStatus: REJECTED\nReason:\n\n%s\n", note)
	if _, err := r.Workspace.WriteDocument(st.Cycle, "05-aprobacion.md", text); err != nil {
		return nil, err
	}
	return st, r.Workspace.SaveState(st, workflow.ActorHuman)
}

// sameTaskProjection compares a persisted task list against the projection
// its plan would produce now. "No dependencies" does not round-trip
// symmetrically between the ticket and task JSON shapes (omitempty on one
// side only), so both spellings compare equal here.
func sameTaskProjection(actual, expected []ws.Task) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i := range actual {
		a, e := actual[i], expected[i]
		if len(a.DependsOn) == 0 {
			a.DependsOn = nil
		}
		if len(e.DependsOn) == 0 {
			e.DependsOn = nil
		}
		if !reflect.DeepEqual(a, e) {
			return false
		}
	}
	return true
}
