package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// CycleHandoff is the cycle-level evidence that the applied change, taken as a
// whole, passes its approved checks. Per-ticket checks ran against the state
// each ticket produced; this one runs against the state the reviewer will
// actually see, which is not the same claim when a cycle has more than one
// ticket.
type CycleHandoff struct {
	Schema     string                 `json:"schema"`
	Cycle      int                    `json:"cycle"`
	Contract   string                 `json:"contract"`
	Approval   *ws.ApprovalBinding    `json:"approval"`
	Baseline   string                 `json:"approved_baseline"`
	Final      string                 `json:"final_state"`
	Paths      []string               `json:"changed_paths"`
	Tickets    map[string]string      `json:"ticket_outcomes"`
	Manifests  []workflow.ArtifactRef `json:"manifests"`
	Checks     []workflow.CheckRecord `json:"checks"`
	RecordedAt time.Time              `json:"recorded_at"`
}

// handoff closes the cycle when every ticket has a recorded outcome. It
// refuses to claim anything when a ticket is still open, and it records an
// awaiting-review handoff only when at least one real change is in the
// checkout and the approved checks pass against exactly that final state.
func (e *execution) handoff(ctx context.Context) error {
	tickets, err := e.store.ListTickets(e.st.Cycle)
	if err != nil {
		return err
	}
	outcomes := map[string]string{}
	implemented := 0
	for _, t := range tickets {
		outcomes[t.ID] = t.Status
		if !finished(t.Status) {
			return nil // work remains; nothing to hand off yet
		}
		if t.Status == workflow.TicketImplemented {
			implemented++
		}
	}
	if len(tickets) == 0 || implemented == 0 {
		// Every ticket reported no change. Nothing was applied, so there is
		// nothing for a reviewer to review and no implementation to claim.
		e.st.Log("every ticket reported no change; nothing was applied", "", "")
		return nil
	}
	if e.st.Phase == ws.PhaseAwaitingReview {
		return nil
	}

	final, err := e.backend.State()
	if err != nil {
		return err
	}
	base := e.backend.Base()
	checks, err := e.finalChecks(ctx, final)
	if err != nil {
		return err
	}
	var manifests []workflow.ArtifactRef
	rounds, err := e.store.Rounds(e.st.Cycle)
	if err != nil {
		return err
	}
	for _, r := range rounds {
		if r.Contract == e.st.Approval.ContractHash && r.Manifest != nil {
			manifests = append(manifests, *r.Manifest)
		}
	}
	handoff := CycleHandoff{
		Schema: "1", Cycle: e.st.Cycle, Contract: e.st.Approval.ContractHash, Approval: e.st.Approval,
		Baseline: base.Baseline(), Final: final.Baseline(), Paths: changedPaths(base, final),
		Tickets: outcomes, Manifests: manifests, Checks: checks, RecordedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(handoff)
	if err != nil {
		return err
	}
	digest := workflow.Digest(string(data))
	ref := workflow.ArtifactRef{
		ID:      fmt.Sprintf("handoff-%s-%s", short(e.st.Approval.ContractHash), short(digest)),
		Path:    fmt.Sprintf("cycles/%03d/execution/%s/handoff-%s.json", e.st.Cycle, e.st.Approval.ContractHash, short(digest)),
		Version: digest,
		Media:   "application/json",
	}
	if _, err = e.artifacts.Publish(e.store, e.st.Cycle, ref, data); err != nil {
		return err
	}
	e.st.Phase = ws.PhaseAwaitingReview
	e.st.Log("all tickets recorded an outcome; awaiting independent review", "", ref.ID)
	fmt.Fprintf(os.Stderr, "\nCycle %03d is awaiting review: %d change(s) in %s\n", e.st.Cycle, len(handoff.Paths), e.target.Root)
	return nil
}

// finalChecks re-runs the approved checks against the cycle's final state,
// unless a per-ticket run already covered exactly that state and passed — in
// which case those results are the same evidence and are reused rather than
// spent again.
func (e *execution) finalChecks(ctx context.Context, final workflow.RepositoryState) ([]workflow.CheckRecord, error) {
	if reused, ok, err := e.reusableChecks(final); err != nil || ok {
		return reused, err
	}
	var records []workflow.CheckRecord
	for _, c := range e.contract.Policy.Checks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := e.r.guard(); err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "  final check %s…\n", c.ID)
		result, runErr := e.backend.Check(ctx, c.ID)
		record := workflow.CheckRecord{
			CheckID: c.ID, RunID: result.ID, Evidence: result.Evidence,
			ExitCode: result.ExitCode, Tested: result.Before,
			Passed: runErr == nil && result.ExitCode == 0 && !result.TimedOut && !result.Truncated,
		}
		if runErr != nil {
			record.Failure = runErr.Error()
		}
		if record.Passed {
			if failure := verifyTestEvidence(c, result); failure != "" {
				record.Passed, record.Failure = false, failure
			}
		}
		records = append(records, record)
		if !record.Passed {
			return nil, fmt.Errorf("final check %s failed against the cycle's applied state; the diff is preserved for inspection: %s", c.ID, record.Failure)
		}
		if record.Tested != final.Baseline() {
			return nil, errors.New("final checks did not run against the cycle's final state")
		}
	}
	if len(records) == 0 {
		return nil, errors.New("the approved contract has no checks; an unchecked cycle cannot await review")
	}
	return records, nil
}

// reusableChecks reports the last round's check results when they already
// cover exactly this state, so a single-ticket cycle does not pay for the
// identical run twice.
func (e *execution) reusableChecks(final workflow.RepositoryState) ([]workflow.CheckRecord, bool, error) {
	rounds, err := e.store.Rounds(e.st.Cycle)
	if err != nil {
		return nil, false, err
	}
	approved := map[string]bool{}
	for _, c := range e.contract.Policy.Checks {
		approved[c.ID] = true
	}
	for _, r := range rounds {
		if r.Contract != e.st.Approval.ContractHash || r.State != workflow.RoundImplemented || r.Confirmed != final.Baseline() {
			continue
		}
		covered := map[string]bool{}
		for _, c := range r.Checks {
			if c.Passed && c.Tested == final.Baseline() {
				covered[c.CheckID] = true
			}
		}
		if len(covered) != len(approved) {
			continue
		}
		complete := true
		for id := range approved {
			if !covered[id] {
				complete = false
			}
		}
		if complete {
			records := append([]workflow.CheckRecord(nil), r.Checks...)
			sort.Slice(records, func(i, j int) bool { return records[i].CheckID < records[j].CheckID })
			return records, true, nil
		}
	}
	return nil, false, nil
}
