package team

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// repairable marks a failure this round caused and a replacement generation
// might fix: an invalid candidate, a patch that would be a no-op, a failed
// check. Everything else — a stale approval, an external edit, an unknown
// check outcome, an exhausted budget — stops execution instead, because
// generating again cannot make any of those true.
type repairable struct{ reason string }

func (e *repairable) Error() string { return e.reason }

func repair(format string, args ...any) error {
	return &repairable{reason: fmt.Sprintf(format, args...)}
}

// ImplementationManifest is the durable link between an approval and what
// actually changed: which inputs were shown, what the model returned, which
// candidate was validated, which patch was confirmed, which diff resulted, and
// which checks passed against exactly that state. It is published before the
// ticket may claim to be implemented.
type ImplementationManifest struct {
	Schema         string                 `json:"schema"`
	Cycle          int                    `json:"cycle"`
	Contract       string                 `json:"contract"`
	Approval       *ws.ApprovalBinding    `json:"approval"`
	Ticket         string                 `json:"ticket"`
	TicketRevision int                    `json:"ticket_revision"`
	Round          int                    `json:"round"`
	Outcome        string                 `json:"outcome"`
	Explanation    string                 `json:"explanation"`
	Baseline       string                 `json:"approved_baseline"`
	Expected       string                 `json:"pre_state"`
	Confirmed      string                 `json:"tested_state"`
	Paths          []string               `json:"changed_paths,omitempty"`
	Inputs         []workflow.ArtifactRef `json:"inputs"`
	Response       *workflow.ArtifactRef  `json:"response,omitempty"`
	Candidate      *workflow.ArtifactRef  `json:"candidate,omitempty"`
	Patch          *workflow.ArtifactRef  `json:"patch,omitempty"`
	Diff           *workflow.ArtifactRef  `json:"diff,omitempty"`
	Checks         []workflow.CheckRecord `json:"checks,omitempty"`
	RecordedAt     time.Time              `json:"recorded_at"`
}

// execution holds everything one `yanai run` needs, bound once so no step can
// quietly re-derive a different repository, contract or store than the step
// before it used.
type execution struct {
	r         *Runner
	st        *ws.State
	store     *workflow.Store
	backend   *executor.Native
	artifacts workflow.ArtifactStore
	contract  workflow.ExecutionContract
	target    *repository.Target
	holder    string
	ttl       time.Duration
	plan      string
	scope     string
	discuss   string
}

// Execute applies the approved plan to the real application checkout.
//
// The recovery order is deliberate and is the whole reason this is not simply
// a loop over tickets: the workspace lock and artifact reconciliation already
// happened when the store was attached; the contract's revision and checkout
// identity are validated before anything is locked or written; the executor is
// then opened, which takes the repository writer lock and reconciles a patch
// an interrupted process may have left half-applied; only after that does
// anything read patch state, re-check approval or resume a durable round —
// and only after *that* may a model be called. Reading patch state earlier
// would refuse outright on a pending patch, which is exactly the situation
// recovery exists to resolve.
func (r *Runner) Execute(ctx context.Context, onlyID string, retryUnresolved bool) (result *ws.State, err error) {
	if os.Getenv("YANAI_NO_REPO") == "1" {
		return nil, fmt.Errorf("run requires repository context; unset YANAI_NO_REPO")
	}
	if err = r.checkUnresolvedAttempts(retryUnresolved); err != nil {
		return nil, err
	}
	store := r.Workspace.Store
	if store == nil {
		return nil, fmt.Errorf("execution requires the workflow store")
	}
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	// awaiting_review is accepted so that rerunning a finished cycle is a
	// no-op rather than an error: every ticket is already implemented, so the
	// loop below has nothing to do and nothing is regenerated or reapplied.
	if st.Phase != ws.PhaseApproved && st.Phase != ws.PhaseAwaitingExecution && st.Phase != ws.PhaseAwaitingReview {
		return nil, fmt.Errorf("cycle %03d is in phase %q: nothing runs without the human's approval ('yanai approve')", st.Cycle, st.Phase)
	}
	if st.Approval == nil || st.Approval.ContractHash == "" {
		return st, fmt.Errorf("approval is stale or incomplete; plan, scope, or repository baseline changed")
	}
	target, err := repository.Open(r.Cfg.Repo, r.Workspace.Root)
	if err != nil {
		return st, err
	}
	contract, err := store.Contract(st.Cycle, st.Approval.ContractHash)
	if err != nil {
		return st, fmt.Errorf("approved contract is not recorded; review and approve again: %w", err)
	}
	live, err := target.Snapshot()
	if err != nil {
		return st, err
	}
	branch, err := target.Branch()
	if err != nil {
		return st, err
	}
	if err = contract.ValidateExecutable(live, branch); err != nil {
		return st, err
	}
	if err = store.HasApproval(st.Cycle, st.Approval.ContractHash); err != nil {
		return st, err
	}

	options, err := executor.Defaults(executor.Options{
		Repo:      r.Cfg.Repo,
		Store:     store,
		Artifacts: workflow.ArtifactStore{Root: r.Workspace.Root},
		Cycle:     st.Cycle,
		Contract:  st.Approval.ContractHash,
	})
	if err != nil {
		return st, err
	}
	backend, err := executor.Open(options)
	if err != nil {
		return st, err
	}
	defer func() {
		if closeErr := backend.Close(); err == nil {
			err = closeErr
		}
	}()

	if err = r.validateCurrentPlan(st); err != nil {
		return st, err
	}
	if st.Intake != nil {
		r.Redactions = st.Intake.Redactions
	}
	scopeHash := contentHash(r.Workspace.ReadContext())
	if st.Markdown != nil {
		scopeHash, _ = workflow.Hash(st.Markdown)
	}
	if st.Approval.PlanHash != st.PlanHash || st.Approval.ScopeHash != scopeHash {
		return st, fmt.Errorf("approval is stale or incomplete; plan, scope, or repository baseline changed")
	}
	r.guard = func() error { return r.checkApproval(st) }
	defer func() { r.guard = nil }()
	if err = r.guard(); err != nil {
		return st, err
	}

	discussion, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(store, st.Cycle, "discussion-"+st.Approval.ContractHash)
	if err != nil {
		return st, err
	}
	host, _ := os.Hostname()
	e := &execution{
		r: r, st: st, store: store, backend: backend, target: target,
		artifacts: workflow.ArtifactStore{Root: r.Workspace.Root},
		contract:  contract,
		holder:    fmt.Sprintf("%s/%d", host, os.Getpid()),
		ttl:       claimTTL(r.Cfg.OpenRouter.TimeoutSec),
		plan:      r.Workspace.ReadDocument(st.Cycle, "04-plan.md"),
		scope:     r.Workspace.ReadContext(),
		discuss:   string(discussion),
	}

	if st.Markdown != nil {
		data, e2 := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(store, st.Cycle, st.Markdown.Source.ID)
		if e2 != nil {
			return st, e2
		}
		e.scope = string(data)
	}

	// One active session covers generation, application, checks and repairs:
	// applying a patch and running its checks is active work whether or not a
	// model is called inside it.
	ctx, finish, err := r.beginWork(ctx, st.Cycle)
	if err != nil {
		return st, err
	}
	defer func() {
		if closeErr := finish(); err == nil {
			err = closeErr
		}
	}()

	if err = e.syncTaskStatuses(); err != nil {
		return st, err
	}
	for i := range st.Tasks {
		t := &st.Tasks[i]
		if onlyID != "" && !strings.EqualFold(t.ID, onlyID) {
			continue
		}
		if finished(t.Status) {
			continue
		}
		if err = e.ticket(ctx, t); err != nil {
			return st, fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err = r.Workspace.SaveState(st, workflow.ActorEngine); err != nil {
			return st, err
		}
	}
	if err = e.handoff(ctx); err != nil {
		return st, err
	}
	return st, r.Workspace.SaveState(st, workflow.ActorEngine)
}

func claimTTL(timeoutSec int) time.Duration {
	ttl := 15 * time.Minute
	if timeoutSec > 0 {
		if t := 4 * time.Duration(timeoutSec) * time.Second; t > ttl {
			ttl = t
		}
	}
	return ttl
}

// finished reports whether a ticket reached a recorded terminal outcome for
// this contract. A staged candidate is not one: it has been applied to
// nothing and checked by nothing.
func finished(status string) bool {
	return status == workflow.TicketImplemented || status == workflow.TicketNoChangeReported
}

// syncTaskStatuses replaces the projection's fixed "pending" with what the
// store actually knows, and drops a Deliverable left by the candidate-staging
// era so a stale path cannot be read as an applied change.
func (e *execution) syncTaskStatuses() error {
	for i := range e.st.Tasks {
		rec, err := e.store.GetTicket(e.st.Cycle, e.st.Tasks[i].ID)
		if err != nil {
			return err
		}
		e.st.Tasks[i].Status = rec.Status
		e.st.Tasks[i].Deliverable = ""
	}
	return nil
}

func (e *execution) approved(id string) (workflow.Ticket, error) {
	for _, t := range e.contract.Plan.Tickets {
		if t.ID == id {
			return t, nil
		}
	}
	return workflow.Ticket{}, fmt.Errorf("ticket %s is not part of the approved contract", id)
}

// ticket drives one ticket to a recorded outcome, opening a replacement round
// for each repairable failure until the ticket's own attempt limit or the
// cycle's repair budget refuses another one.
func (e *execution) ticket(ctx context.Context, t *ws.Task) error {
	ticket, err := e.approved(t.ID)
	if err != nil {
		return err
	}
	for _, dep := range ticket.DependsOn {
		rec, err := e.store.GetTicket(e.st.Cycle, dep)
		if err != nil {
			return err
		}
		if !workflow.SatisfiesDependency(rec.Status) {
			return fmt.Errorf("depends on %s, which is %q; only an implemented dependency unblocks it", dep, rec.Status)
		}
	}
	defer e.store.ReleaseClaim(e.st.Cycle, t.ID, e.holder) //nolint:errcheck

	var failures []string
	for {
		// Re-claimed each round: a replacement round starts from a ticket a
		// failed round left at response_rejected, which has to return to
		// pending before it can be worked on again.
		if err := e.claim(t.ID); err != nil {
			return err
		}
		round, version, err := e.currentRound(ticket)
		if err != nil {
			return err
		}
		err = e.round(ctx, ticket, round, version)
		if err == nil {
			rec, getErr := e.store.GetTicket(e.st.Cycle, t.ID)
			if getErr != nil {
				return getErr
			}
			t.Status = rec.Status
			return nil
		}
		var again *repairable
		if !errors.As(err, &again) {
			return err
		}
		failures = append(failures, fmt.Sprintf("round %d: %s", round.Round, again.reason))
		fmt.Fprintf(os.Stderr, "  %s round %d failed: %s\n", t.ID, round.Round, again.reason)
		if err := ctx.Err(); err != nil {
			return err
		}
		// ChargeRepair is what actually decides whether another generation is
		// allowed: it enforces the ticket's max_attempts and the cycle's
		// repair budget together, and it is charged before the replacement
		// round is opened rather than after it has already been paid for.
		if err := e.store.ChargeRepair(e.st.Cycle, t.ID, ticket.MaxAttempts); err != nil {
			return fmt.Errorf("%w; the recorded diff and evidence are preserved:\n  %s", err, strings.Join(failures, "\n  "))
		}
	}
}

// claim takes this ticket's lease. A ticket left mid-generation by a dead
// process goes back to pending first; a durable candidate keeps its status,
// because the round record — not the status — decides whether it is reusable.
func (e *execution) claim(id string) error {
	rec, err := e.store.GetTicket(e.st.Cycle, id)
	if err != nil {
		return err
	}
	if rec.Status == workflow.TicketResponseRecorded {
		rec, err = e.store.ApplyTicketStatus(e.st.Cycle, id, workflow.TicketResponseRejected, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "candidate.interrupted"})
		if err != nil {
			return err
		}
	}
	if rec.Status == workflow.TicketResponseRejected {
		if _, err = e.store.ApplyTicketStatus(e.st.Cycle, id, workflow.TicketPending, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "retry"}); err != nil {
			return err
		}
	}
	_, _, err = e.store.ClaimTicket(e.st.Cycle, id, e.holder, e.ttl, workflow.Event{})
	return err
}

// currentRound resumes the round in progress or opens the next one. A ticket
// showing candidate_ready with no round for this exact contract and revision
// is a candidate staged under older terms: it is withdrawn rather than
// promoted, because nobody approved applying it.
func (e *execution) currentRound(ticket workflow.Ticket) (workflow.ExecutionRound, int64, error) {
	rec, err := e.store.GetTicket(e.st.Cycle, ticket.ID)
	if err != nil {
		return workflow.ExecutionRound{}, 0, err
	}
	latest, version, found, err := e.store.LatestRound(e.st.Cycle, e.st.Approval.ContractHash, ticket.ID, rec.Revision)
	if err != nil {
		return workflow.ExecutionRound{}, 0, err
	}
	if found && latest.State != workflow.RoundFailed {
		return latest, version, nil
	}
	if rec.Status == workflow.TicketCandidateReady && (!found || latest.Candidate == nil) {
		if _, err = e.store.ApplyTicketStatus(e.st.Cycle, ticket.ID, workflow.TicketResponseRejected, workflow.ActorEngine, rec.StateVersion, workflow.Event{
			Type:    "candidate.superseded",
			Payload: "staged under different terms; not promoted",
		}); err != nil {
			return workflow.ExecutionRound{}, 0, err
		}
		if err = e.claim(ticket.ID); err != nil {
			return workflow.ExecutionRound{}, 0, err
		}
	}
	next := 1
	if found {
		next = latest.Round + 1
	}
	// A replacement round starts clean. What went wrong last time belongs to
	// the round it went wrong in; the repair reads it back from there by its
	// own key, so nothing here has to carry it forward.
	return e.store.OpenRound(workflow.ExecutionRound{
		Cycle:          e.st.Cycle,
		Contract:       e.st.Approval.ContractHash,
		Ticket:         ticket.ID,
		TicketRevision: rec.Revision,
		Round:          next,
	})
}
