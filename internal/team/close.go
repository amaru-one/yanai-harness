package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/orchestrator"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// maxReportDiffBytes bounds the patch embedded in the final report; the
// branch itself always holds the complete change.
const maxReportDiffBytes = 256 << 10

// Handoff is the durable evidence that a worker's branch is ready for
// review: the commits, the cumulative diff, and the final checks.
type Handoff struct {
	Schema       string                     `json:"schema"`
	Cycle        int                        `json:"cycle"`
	Contract     string                     `json:"contract"`
	Worker       string                     `json:"worker"`
	TicketBranch string                     `json:"ticket_branch"`
	BaseSHA      string                     `json:"base_sha"`
	TipSHA       string                     `json:"tip_sha"`
	Original     string                     `json:"original_branch"`
	Commits      []workflow.GitCommitRecord `json:"commits"`
	Checks       json.RawMessage            `json:"final_checks"`
	DiffStat     string                     `json:"diff_stat"`
	RecordedAt   time.Time                  `json:"recorded_at"`
}

// completeImplementation runs after the worker finished and the checkout is
// back on its original branch: it records the report, moves the cycle to
// awaiting_review, and asks the parent for the project-state update.
func (r *Runner) completeImplementation(ctx context.Context, st *ws.State, c workflow.ExecutionContract, backend *executor.Native, retry bool) (*ws.State, error) {
	session, found, err := backend.GitSession()
	if err != nil || !found {
		return st, errors.Join(err, errors.New("no Git session recorded"))
	}
	if session.State != workflow.GitSessionReturned || len(session.Commits) == 0 {
		return st, errors.New("implementation is not complete: the branch has no commits or the checkout has not returned")
	}
	stat, patch, err := backend.CumulativeDiff(maxReportDiffBytes)
	if err != nil {
		return st, err
	}
	checks, err := r.lastFinalChecks(st, c)
	if err != nil {
		return st, err
	}
	tip := session.Commits[len(session.Commits)-1].SHA
	handoff := Handoff{Schema: "1", Cycle: st.Cycle, Contract: st.Approval.ContractHash, Worker: c.Level0.Worker.ID, TicketBranch: session.TicketBranch, BaseSHA: session.BaseSHA, TipSHA: tip, Original: session.OriginalBranch, Commits: session.Commits, Checks: checks, DiffStat: stat, RecordedAt: time.Now().UTC()}
	report := r.renderReport(st, c, handoff, patch)
	artifacts := workflow.ArtifactStore{Root: r.Workspace.Root}
	handoffRef, err := artifacts.Publish(r.Workspace.Store, st.Cycle, workflow.ArtifactRef{ID: "handoff-" + shortHash(tip), Path: fmt.Sprintf("cycles/%03d/execution/handoff-%s.json", st.Cycle, shortHash(tip)), Version: tip, Media: "application/json"}, mustJSON(handoff))
	if err != nil {
		return st, err
	}
	reportRef, err := artifacts.Publish(r.Workspace.Store, st.Cycle, workflow.ArtifactRef{ID: "report-" + shortHash(tip), Path: fmt.Sprintf("cycles/%03d/execution/report-%s.md", st.Cycle, shortHash(tip)), Version: tip, Media: "text/markdown"}, []byte(report))
	if err != nil {
		return st, err
	}
	if st.Phase != ws.PhaseAwaitingReview {
		st.Phase = ws.PhaseAwaitingReview
		st.Orchestration.Report, st.Orchestration.Commits, st.Orchestration.Outcome = &reportRef, session.Commits, "completed"
		setTaskStatus(st, workflow.TicketImplemented)
		st.Log("the worker's branch is ready for review; the checkout is back on "+session.OriginalBranch, c.Level0.Worker.ID, handoffRef.ID)
		if err = r.Workspace.SaveState(st, workflow.ActorEngine); err != nil {
			return st, err
		}
	}
	fmt.Fprintf(os.Stderr, "\nBranch %s is ready for review (%d commit(s)); %s is unchanged at %s.\n", session.TicketBranch, len(session.Commits), session.OriginalBranch, shortHash(session.OriginalSHA)[:12])
	r.guard = nil
	return r.close(ctx, st, retry)
}

// lastFinalChecks returns the final check records the worker's accepted
// finish produced.
func (r *Runner) lastFinalChecks(st *ws.State, c workflow.ExecutionContract) (json.RawMessage, error) {
	steps, err := r.Workspace.Store.AgentSteps(st.Cycle, r.workerRunKey(st, c))
	if err != nil {
		return nil, err
	}
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		if s.ToolName == "finish" && s.Terminal && s.Result != nil {
			raw, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(r.Workspace.Store, st.Cycle, s.Result.ID)
			if err != nil {
				return nil, err
			}
			var result struct {
				FinalChecks json.RawMessage `json:"final_checks"`
			}
			if err = json.Unmarshal(raw, &result); err != nil {
				return nil, err
			}
			return result.FinalChecks, nil
		}
	}
	return nil, errors.New("no accepted completion with final checks is recorded")
}

func (r *Runner) renderReport(st *ws.State, c workflow.ExecutionContract, h Handoff, patch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Final report — %s (%s)\n\n", st.Markdown.Title, st.Markdown.Type)
	fmt.Fprintf(&b, "- Worker: %s\n- Review branch: %s (base %s → tip %s)\n- Original branch: %s (unchanged)\n\n## Commits\n\n", h.Worker, h.TicketBranch, h.BaseSHA[:12], h.TipSHA[:12], h.Original)
	for _, commit := range h.Commits {
		fmt.Fprintf(&b, "- `%s` %s — %s\n", commit.SHA[:12], commit.Subject, strings.Join(commit.Paths, ", "))
	}
	var checks []workflow.CheckRecord
	_ = json.Unmarshal(h.Checks, &checks)
	b.WriteString("\n## Final checks (against the final committed state)\n\n")
	for _, check := range checks {
		fmt.Fprintf(&b, "- %s: exit %d, passed=%t (evidence %s)\n", check.CheckID, check.ExitCode, check.Passed, check.Evidence.ID)
	}
	b.WriteString("\n## Usage\n\n")
	if worker, err := r.Workspace.Store.Budget(st.Cycle); err == nil {
		fmt.Fprintf(&b, "- Worker: %d tokens, $%.4f, %d calls, %d repairs\n", worker.Tokens, worker.Cost, worker.Calls, worker.Repairs)
	}
	if parent, err := r.Workspace.Store.ParentBudget(st.Cycle); err == nil {
		fmt.Fprintf(&b, "- Parent: %d tokens, $%.4f, %d calls\n", parent.Tokens, parent.Cost, parent.Calls)
	}
	fmt.Fprintf(&b, "\n## Cumulative diff (git diff %s %s)\n\n```\n%s```\n\n```diff\n%s```\n", h.BaseSHA[:12], h.TicketBranch, h.DiffStat, patch)
	return b.String()
}

// close runs the parent's closing phase: a proposed complete estado.md for
// the human to accept. A proposal made against a state file the operator
// has since edited is replaced by a fresh one, never applied over the edit.
func (r *Runner) close(ctx context.Context, st *ws.State, retry bool) (*ws.State, error) {
	store := r.Workspace.Store
	estado, err := r.Workspace.ReadWorkspaceFile(ws.EstadoPath)
	if err != nil {
		return st, err
	}
	if existing, found, err := store.LatestStateProposal(st.Cycle); err != nil {
		return st, err
	} else if found && existing.OldHash == estado.SHA256 && st.Orchestration.StateUpdate == existing.Hash {
		fmt.Fprintf(os.Stderr, "A project-state update is waiting for you: yanai review, then yanai close --state-update %s\n", existing.Hash)
		return st, nil
	}
	c, err := store.Contract(st.Cycle, st.Approval.ContractHash)
	if err != nil {
		return st, err
	}
	var parent struct {
		Model       string                  `json:"model"`
		Temperature float64                 `json:"temperature"`
		MaxTokens   int                     `json:"max_tokens"`
		MaxSteps    int                     `json:"max_steps"`
		Budget      workflow.PlanningBudget `json:"budget"`
	}
	if err = json.Unmarshal(st.Orchestration.ParentSettings, &parent); err != nil {
		return st, err
	}
	documents, err := r.Workspace.ProjectDocuments()
	if err != nil {
		return st, err
	}
	report, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(store, st.Cycle, st.Orchestration.Report.ID)
	if err != nil {
		return st, err
	}
	cc := orchestrator.ClosingContext{Ticket: *st.Markdown, Documents: documents, Branch: c.Level0.TicketBranch, Commits: st.Orchestration.Commits, Report: string(report), Worker: c.Level0.Worker.ID}
	initial, err := orchestrator.ClosingMessage(cc)
	if err != nil {
		return st, err
	}
	ctx, finish, err := r.beginWork(ctx, st.Cycle, workflow.BucketParent, parent.Budget.Policy())
	if err != nil {
		return st, fmt.Errorf("the implementation is recorded, but closing is paused: %w", err)
	}
	defer func() {
		if e := finish(); err == nil {
			err = e
		}
	}()
	runKey := "close-" + shortHash(workflow.Digest(estado.SHA256+st.Approval.ContractHash))
	var update orchestrator.StateUpdate
	run := agentRun{
		Key: runKey, Role: parentRole, Bucket: workflow.BucketParent, Model: parent.Model, Temperature: parent.Temperature,
		MaxTokens: parent.MaxTokens, MaxSteps: parent.MaxSteps, Policy: parent.Budget.Policy(),
		System: orchestrator.ClosingSystem(), Initial: initial, Tools: orchestrator.ClosingTools(), Final: "submit_state_update", Retry: retry,
		Execute: func(ctx context.Context, step workflow.AgentStep, mark func(any) error) (toolOutcome, error) {
			var u orchestrator.StateUpdate
			if err := workflow.DecodeStrict(string(step.Arguments), &u); err != nil {
				return toolError("submit_state_update arguments: %v", err), nil
			}
			if err := orchestrator.ValidateStateUpdate(u, cc); err != nil {
				return toolError("%v; submit the corrected complete document", err), nil
			}
			update = u
			return toolOutcome{Result: map[string]any{"status": "proposed for human acceptance"}, Terminal: true}, nil
		},
	}
	result, err := r.runLoop(ctx, run)
	if err != nil {
		return st, fmt.Errorf("the implementation is recorded, but closing is paused (rerun 'yanai run' to retry): %w", err)
	}
	if update.Content == "" {
		if err = workflow.DecodeStrict(string(result.Step.Arguments), &update); err != nil {
			return st, err
		}
	}
	tip := st.Orchestration.Commits[len(st.Orchestration.Commits)-1].SHA
	hash := workflow.Digest(estado.SHA256 + "\x00" + tip + "\x00" + update.Content)[:24]
	contentRef, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Publish(store, st.Cycle, workflow.ArtifactRef{ID: "state-update-" + hash, Path: fmt.Sprintf("cycles/%03d/closing/estado-%s.md", st.Cycle, hash), Version: hash, Media: "text/markdown"}, []byte(update.Content))
	if err != nil {
		return st, err
	}
	proposal := workflow.StateProposal{Hash: hash, OldHash: estado.SHA256, BranchSHA: tip, Branch: c.Level0.TicketBranch, Summary: update.Summary, Content: contentRef, Report: *st.Orchestration.Report}
	if err = store.SaveStateProposal(st.Cycle, proposal); err != nil {
		return st, err
	}
	if st, err = r.Workspace.LoadState(); err != nil {
		return st, err
	}
	st.Orchestration.StateUpdate = hash
	st.Log("the parent proposed a project-state update", parentRole, hash)
	if err = r.Workspace.SaveState(st, workflow.ActorEngine); err != nil {
		return st, err
	}
	fmt.Fprintf(os.Stderr, "Review the report and the state update: yanai review, then yanai close --state-update %s\n", hash)
	return st, nil
}

// closingReview shows the final report and the proposed state-file diff.
func (r *Runner) closingReview(st *ws.State) (string, error) {
	store := r.Workspace.Store
	var b strings.Builder
	if st.Orchestration.Report == nil {
		return "", errors.New("no final report is recorded")
	}
	report, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(store, st.Cycle, st.Orchestration.Report.ID)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "Report: cycle %03d · phase %s\n\n%s\n", st.Cycle, st.Phase, report)
	proposal, found, err := store.LatestStateProposal(st.Cycle)
	if err != nil {
		return "", err
	}
	if !found {
		b.WriteString("\nNo project-state update has been proposed yet; run 'yanai run' to let the parent propose one.\n")
		return b.String(), nil
	}
	content, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(store, st.Cycle, proposal.Content.ID)
	if err != nil {
		return "", err
	}
	current, err := r.Workspace.ReadWorkspaceFile(ws.EstadoPath)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\n== Proposed %s (%s) ==\n%s\n\n%s", ws.EstadoPath, proposal.State, proposal.Summary, lineDiff(current.Content, string(content), ws.EstadoPath, ws.EstadoPath+" (proposed)"))
	if current.SHA256 != proposal.OldHash && proposal.State != workflow.StateUpdateAccepted {
		fmt.Fprintf(&b, "\n%s changed after this proposal was made; run 'yanai run' for a refreshed proposal.\n", ws.EstadoPath)
	} else if proposal.State != workflow.StateUpdateAccepted {
		fmt.Fprintf(&b, "\nAccept with: yanai close --state-update %s\n", proposal.Hash)
	}
	return b.String(), nil
}

// Close applies the reviewed project-state update and completes the cycle.
// It never calls a model and never touches the target repository.
func (r *Runner) Close(hash string) (*ws.State, error) {
	store := r.Workspace.Store
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	proposal, found, err := store.LatestStateProposal(st.Cycle)
	if err != nil {
		return st, err
	}
	if !found || proposal.Hash != hash {
		return st, errors.New("that is not the latest proposed state update for this cycle; run 'yanai review' and use the hash it shows")
	}
	if st.Phase == ws.PhaseCompleted && proposal.State == workflow.StateUpdateAccepted {
		return st, nil
	}
	if st.Phase != ws.PhaseAwaitingReview {
		return st, fmt.Errorf("cycle %03d is in phase %q; only a reviewed branch can be closed", st.Cycle, st.Phase)
	}
	content, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(store, st.Cycle, proposal.Content.ID)
	if err != nil {
		return st, err
	}
	// The branch must still point where the report says.
	target, err := repository.Open(r.Cfg.Repo, r.Workspace.Root)
	if err != nil {
		return st, err
	}
	if tip, err := target.BranchTip(proposal.Branch); err != nil {
		return st, err
	} else if tip != proposal.BranchSHA {
		return st, fmt.Errorf("branch %s moved from the reviewed %s; the state update no longer describes it", proposal.Branch, proposal.BranchSHA[:12])
	}
	current, err := r.Workspace.ReadWorkspaceFile(ws.EstadoPath)
	if err != nil {
		return st, err
	}
	switch current.SHA256 {
	case proposal.OldHash:
		path := filepath.Join(r.Workspace.Root, filepath.FromSlash(ws.EstadoPath))
		tmp, err := os.CreateTemp(filepath.Dir(path), ".estado-*")
		if err != nil {
			return st, err
		}
		defer os.Remove(tmp.Name())
		if _, err = tmp.Write(content); err == nil {
			err = tmp.Sync()
		}
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Chmod(tmp.Name(), 0o644)
		}
		if err != nil {
			return st, err
		}
		if err = os.Rename(tmp.Name(), path); err != nil {
			return st, err
		}
	case workflow.Digest(string(content)):
		// Applied before an interruption.
	default:
		return st, fmt.Errorf("%s was edited after the proposal; nothing was overwritten. Run 'yanai run' for a refreshed proposal", ws.EstadoPath)
	}
	st.Phase = ws.PhaseCompleted
	st.Log("project state update accepted; cycle completed (the branch was reviewed, not merged)", workflow.ActorHuman, hash)
	payload, _ := json.Marshal(st)
	if err = store.CloseCycle(st.Cycle, st.StateVersion, hash, string(payload)); err != nil {
		return st, err
	}
	if st, err = r.Workspace.LoadState(); err != nil {
		return st, err
	}
	return st, r.Workspace.RefreshProjection(st)
}
