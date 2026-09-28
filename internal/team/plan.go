package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/orchestrator"
	"github.com/yanai/yanai-harness/internal/repoctx"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// parentRole names the parent in steps, attempts and observations.
const parentRole = "parent"

// maxParentReadBytes bounds everything the parent may read in one planning
// run through read_file, on top of the repository index.
const maxParentReadBytes = 1 << 20

// Plan runs the parent for an already-decided ticket. The same ticket
// revision resumes its cycle; a different one starts a new cycle, and
// approved or in-progress work must be invalidated (or closed) first.
func (r *Runner) Plan(ctx context.Context, raw string, retry bool) (*ws.State, error) {
	ticket, err := workflow.ParseMarkdownTicket(raw)
	if err != nil {
		return nil, err
	}
	store := r.Workspace.Store
	if store == nil {
		return nil, errors.New("planning requires SQLite authority")
	}
	if err = r.checkUnresolvedAttempts(retry); err != nil {
		return nil, err
	}
	if unresolved, e := store.UnreconciledAttempts(); e != nil {
		return nil, e
	} else if len(unresolved) > 0 {
		return nil, errors.New("reconcile outstanding billing/usage before planning or consuming a cached response")
	}
	// Everything checkable without paying is checked first.
	documents, err := r.Workspace.ProjectDocuments()
	if err != nil {
		return nil, err
	}
	if err = r.Cfg.ValidateOrchestrator(); err != nil {
		return nil, err
	}
	target, err := repository.Open(r.Cfg.Repo, r.Workspace.Root)
	if err != nil {
		return nil, err
	}
	baseline, err := target.Snapshot()
	if err != nil {
		return nil, err
	}
	liveConfig, err := os.ReadFile(filepath.Join(r.Workspace.Root, config.FileName))
	if err != nil {
		return nil, err
	}

	var st *ws.State
	if r.Workspace.LastCycle() > 0 {
		prior, e := r.Workspace.LoadState()
		if e != nil {
			return nil, e
		}
		if e = store.RequireResolved(prior.Cycle); e != nil && prior.Phase == ws.PhaseAnalyzed {
			return prior, e
		}
		switch {
		case prior.Markdown != nil && prior.Markdown.Revision == ticket.Revision && prior.Orchestration != nil && prior.Phase != ws.PhaseRejected && prior.Phase != ws.PhaseCompleted && prior.Phase != ws.PhaseNoChange:
			st = prior
		case prior.Phase == ws.PhaseApproved || prior.Phase == ws.PhaseAwaitingExecution:
			return prior, errors.New("invalidate approved work before starting a different ticket; confirmed changes are preserved")
		case prior.Phase == ws.PhaseAwaitingReview:
			return prior, errors.New("the previous ticket awaits review: accept its state update with 'yanai close' or invalidate it before starting another ticket")
		}
	}
	if st == nil {
		if st, err = r.newCycle(raw, ticket, liveConfig, baseline); err != nil {
			return st, err
		}
	}
	if st.Phase != ws.PhaseAnalyzed {
		return st, nil
	}
	if err = store.RequireResolved(st.Cycle); err != nil {
		return st, err
	}
	if st.BaselineHash != baseline.Baseline() {
		return st, errors.New("the repository changed since this cycle started; reject it and plan the ticket again")
	}
	original, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(store, st.Cycle, st.Orchestration.ConfigOriginal.ID)
	if err != nil {
		return st, err
	}
	if string(original) != string(liveConfig) {
		return st, fmt.Errorf("%s changed since this cycle started; reject it and plan again so the proposal is reviewed against the live configuration", config.FileName)
	}
	var parent config.Orchestrator
	if err = json.Unmarshal(st.Orchestration.ParentSettings, &parent); err != nil {
		return st, err
	}
	pc, err := r.planningContext(st, documents, target, liveConfig, parent)
	if err != nil {
		return st, err
	}
	initial, err := orchestrator.PlanningMessage(pc, ws.GeneratedPromptsDir+"/"+ticket.Slug+"/")
	if err != nil {
		return st, err
	}
	if len(initial) > MaxContextBytes {
		return st, errors.New("planning context too large; narrow repo.allowed_paths or shorten the project documents")
	}

	ctx, finish, err := r.beginWork(ctx, st.Cycle, workflow.BucketParent, parent.Budget.Policy())
	if err != nil {
		return st, err
	}
	defer func() {
		if e := finish(); err == nil {
			err = e
		}
	}()
	inputHash := pc.InputHash()
	runKey := "plan-" + shortHash(inputHash)
	var validated orchestrator.Validated
	var proposal orchestrator.Proposal
	readBytes := 0
	run := agentRun{
		Key: runKey, Role: parentRole, Bucket: workflow.BucketParent, Model: parent.Model, Temperature: parent.Temperature,
		MaxTokens: parent.MaxTokens, MaxSteps: parent.MaxSteps, Policy: parent.Budget.Policy(),
		System: orchestrator.PlanningSystem(), Initial: initial, Tools: orchestrator.PlanningTools(), Final: "submit_proposal", Retry: retry,
		Execute: func(ctx context.Context, step workflow.AgentStep, mark func(any) error) (toolOutcome, error) {
			switch step.ToolName {
			case "read_file":
				paths, err := orchestrator.ReadPaths(string(step.Arguments))
				if err != nil {
					return toolError("%v", err), nil
				}
				// Each file succeeds or fails on its own; one bad path does not
				// cost the turn that read the others.
				files := make([]map[string]any, 0, len(paths))
				for _, path := range paths {
					data, err := target.ReadFile(path, r.Cfg.Repo.MaxBytesFile+1)
					switch {
					case err != nil:
						files = append(files, map[string]any{"path": path, "error": err.Error()})
					case len(data) > r.Cfg.Repo.MaxBytesFile:
						files = append(files, map[string]any{"path": path, "error": fmt.Sprintf("larger than repo.max_file_bytes (%d); planning reads are bounded", r.Cfg.Repo.MaxBytesFile)})
					case readBytes+len(data) > maxParentReadBytes:
						files = append(files, map[string]any{"path": path, "error": fmt.Sprintf("the planning read budget of %d bytes is used up; submit the proposal", maxParentReadBytes)})
					default:
						readBytes += len(data)
						files = append(files, map[string]any{"path": path, "content": string(data)})
					}
				}
				return toolOutcome{Result: map[string]any{"files": files}}, nil
			case "submit_proposal":
				p, err := orchestrator.DecodeProposal(string(step.Arguments))
				if err != nil {
					return toolError("%v", err), nil
				}
				id := "proposal-" + shortHash(workflow.Digest(string(step.Arguments)))
				payload, _ := json.Marshal(p)
				if len(p.Observations) > 0 {
					for _, o := range p.Observations {
						if err := o.Validate(); err != nil {
							return toolError("%v", err), nil
						}
					}
					proposal = p
					return toolOutcome{Result: map[string]any{"status": "paused for human responses to the observations"}, Terminal: true}, nil
				}
				v, verr := orchestrator.Validate(p, pc, r.Workspace.Root, func(repo config.Repo) (orchestrator.Checker, error) {
					return repository.Open(repo, r.Workspace.Root)
				})
				if verr == nil {
					verr = r.checkWorkerBudget(st.Cycle, v.Config.Execution)
				}
				record := workflow.ProposalRecord{ID: id, Cycle: st.Cycle, InputHash: inputHash, Worker: p.Worker.ID, State: workflow.ProposalValid, Payload: payload}
				if verr != nil {
					record.State, record.Validation = workflow.ProposalInvalid, verr.Error()
				}
				if err := r.Workspace.Store.SaveProposal(record); err != nil {
					return toolOutcome{}, err
				}
				if verr != nil {
					return toolError("the proposal was rejected: %v. Fix it and call submit_proposal again.", verr), nil
				}
				proposal, validated = p, v
				return toolOutcome{Result: map[string]any{"status": "accepted for human review", "proposal": id}, Terminal: true}, nil
			}
			return toolError("unknown tool %s", step.ToolName), nil
		},
	}
	result, err := r.runLoop(ctx, run)
	if err != nil {
		return st, err
	}
	if result.Step.ToolName != "submit_proposal" || result.Step.IsError {
		return st, errors.New("the parent finished without an accepted proposal")
	}
	if proposal.Summary == "" {
		// Replayed terminal step: decode the recorded proposal again.
		if proposal, err = orchestrator.DecodeProposal(string(result.Step.Arguments)); err != nil {
			return st, err
		}
		if len(proposal.Observations) == 0 {
			if validated, err = orchestrator.Validate(proposal, pc, r.Workspace.Root, func(repo config.Repo) (orchestrator.Checker, error) {
				return repository.Open(repo, r.Workspace.Root)
			}); err != nil {
				return st, err
			}
		}
	}
	if len(proposal.Observations) > 0 {
		if err = r.Workspace.Store.RecordObservations(st.Cycle, parentRole, runKey, proposal.Observations); err != nil {
			return st, err
		}
		st, err = r.Workspace.LoadState()
		if err != nil {
			return st, err
		}
		return st, r.Workspace.Store.RequireResolved(st.Cycle)
	}
	current, err := target.Snapshot()
	if err != nil {
		return st, err
	}
	if current.Baseline() != st.BaselineHash {
		return st, errors.New("repository changed during planning")
	}
	return r.acceptProposal(st, proposal, validated, pc, "proposal-"+shortHash(workflow.Digest(string(result.Step.Arguments))))
}

func (r *Runner) newCycle(raw string, ticket workflow.MarkdownTicket, liveConfig []byte, baseline workflow.RepositoryState) (*ws.State, error) {
	store := r.Workspace.Store
	st, err := r.Workspace.NewCycle(workflow.MarkdownOrigin)
	if err != nil {
		return nil, err
	}
	st.SchemaVersion = "2"
	st.Markdown = &ticket
	st.BaseCommit = baseline.Head
	st.BaselineHash = baseline.Baseline()
	st.Phase = ws.PhaseAnalyzed
	source, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Publish(store, st.Cycle, workflow.ArtifactRef{ID: "ticket-source", Path: fmt.Sprintf("cycles/%03d/inputs/ticket.md", st.Cycle), Version: ticket.Revision}, []byte(raw))
	if err != nil {
		return st, err
	}
	st.Markdown.Source = source
	cfgRef, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Publish(store, st.Cycle, workflow.ArtifactRef{ID: "config-original", Path: fmt.Sprintf("cycles/%03d/inputs/yanai.config.json", st.Cycle), Version: workflow.Digest(string(liveConfig)), Media: "application/json"}, liveConfig)
	if err != nil {
		return st, err
	}
	parent, _ := json.Marshal(r.Cfg.Orchestrator)
	st.Orchestration = &ws.OrchestrationState{Protocol: orchestrator.Protocol, ParentSettings: parent, ConfigOriginal: cfgRef, RepoBaseline: baseline.Baseline(), RepoRoot: baseline.Root, TicketBranch: ticket.TicketBranch()}
	st.Log("ticket received; the parent will propose one worker", "", ticket.TicketBranch())
	if err = r.Workspace.SaveState(st, workflow.ActorEngine); err != nil {
		return st, err
	}
	if err = store.EnsureParentBudget(st.Cycle, r.Cfg.Orchestrator.Budget); err != nil {
		return st, err
	}
	return st, nil
}

func (r *Runner) planningContext(st *ws.State, documents []ws.Document, target *repository.Target, liveConfig []byte, parent config.Orchestrator) (orchestrator.Context, error) {
	var pc orchestrator.Context
	source, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(r.Workspace.Store, st.Cycle, st.Markdown.Source.ID)
	if err != nil {
		return pc, err
	}
	redacted, err := workflow.RedactCheckInputs(string(source))
	if err != nil {
		return pc, err
	}
	inputs, err := workflow.ParseCheckInputs(string(source))
	if err != nil {
		return pc, err
	}
	for _, v := range inputs {
		r.Secrets = append(r.Secrets, v)
	}
	base, err := r.Workspace.BasePrompts()
	if err != nil {
		return pc, err
	}
	var instructions []ws.Document
	if data, err := target.ReadFile("AGENTS.md", MaxContextBytes); err == nil {
		instructions = append(instructions, ws.Document{Path: "AGENTS.md", Content: string(data), SHA256: workflow.Digest(string(data))})
	}
	index, err := repoctx.Index(r.Cfg.Repo)
	if err != nil {
		return pc, err
	}
	observations, err := r.Workspace.Store.Observations(st.Cycle)
	if err != nil {
		return pc, err
	}
	return orchestrator.Context{
		Ticket: *st.Markdown, RedactedTicket: redacted, Documents: documents, BasePrompts: base, Instructions: instructions,
		Index: index, Config: r.Cfg, ConfigRaw: liveConfig, Parent: parent, Observations: observations,
	}, nil
}

// checkWorkerBudget rejects a proposed budget below spending the worker
// bucket already recorded; spending is never reset by a new policy.
func (r *Runner) checkWorkerBudget(cycle int, p workflow.ExecutionPolicy) error {
	b, err := r.Workspace.Store.Budget(cycle)
	if err != nil {
		return nil // no worker spending recorded yet
	}
	if p.MaxTokens < b.Tokens || p.MaxCostUSD < b.Cost || p.MaxCalls < b.Calls || p.MaxRepairs < b.Repairs || p.MaxActiveSeconds*1000 < b.ActiveMS {
		return errors.New("the proposed execution budget is below spending already recorded for this cycle")
	}
	return nil
}

// acceptProposal writes the generated prompt and records the reviewable plan.
// Nothing in the target repository changes, and the live configuration is
// untouched until a human approves.
func (r *Runner) acceptProposal(st *ws.State, p orchestrator.Proposal, v orchestrator.Validated, pc orchestrator.Context, id string) (*ws.State, error) {
	store := r.Workspace.Store
	artifacts := workflow.ArtifactStore{Root: r.Workspace.Root}
	promptRef, err := artifacts.Publish(store, st.Cycle, workflow.ArtifactRef{ID: "proposal-prompt", Path: fmt.Sprintf("cycles/%03d/proposal/%s.md", st.Cycle, v.Worker.ID), Version: workflow.Digest(p.Worker.Prompt), Media: "text/markdown"}, []byte(p.Worker.Prompt))
	if err != nil {
		return st, err
	}
	configRef, err := artifacts.Publish(store, st.Cycle, workflow.ArtifactRef{ID: "proposal-config", Path: fmt.Sprintf("cycles/%03d/proposal/yanai.config.json", st.Cycle), Version: workflow.Digest(string(v.ConfigBytes)), Media: "application/json"}, v.ConfigBytes)
	if err != nil {
		return st, err
	}
	if err = r.writeGeneratedPrompt(st.Cycle, v.PromptPath, p.Worker.Prompt); err != nil {
		return st, err
	}
	task := v.Task
	task.BaseCommit = st.BaseCommit
	plan := workflow.Proposal{SchemaVersion: "1", ID: "ticket-plan", Outcome: workflow.OutcomeProposeChange, Origin: workflow.MarkdownOrigin, Summary: p.Summary, Inputs: []workflow.ArtifactRef{st.Markdown.Source}, Tickets: []workflow.Ticket{task}}
	if _, err = store.SaveTicket(st.Cycle, task); err != nil {
		return st, err
	}
	st.Plan = &plan
	st.PlanHash, _ = workflow.Hash(st.Plan)
	st.ScopeHash, _ = workflow.Hash(st.Markdown)
	st.Tasks = tasksForProposal(plan)
	st.Verdict = string(plan.Outcome)
	o := st.Orchestration
	o.Proposal, o.ProposalInput, o.Worker, o.InputHashes = id, pc.InputHash(), v.Worker.ID, pc.Hashes()
	o.ConfigProposed, o.Prompt = &configRef, &promptRef
	o.CommitScope, o.CommitModule, o.BasePrompt, o.Summary = p.CommitScope.Scope, p.CommitScope.Module, p.Worker.BasePrompt, p.Summary
	o.ModelReason = p.Worker.ModelReason
	o.TaskComplexity = p.Worker.TaskComplexity
	o.ComplexityReason = p.Worker.ComplexityReason
	if _, err = r.Workspace.WriteDocument(st.Cycle, "04-plan.md", renderProposal(p, v)); err != nil {
		return st, err
	}
	st.Phase = ws.PhaseWaiting
	st.Log("parent proposed one worker", parentRole, v.Worker.ID)
	return st, r.Workspace.SaveState(st, workflow.ActorEngine)
}

// writeGeneratedPrompt places the worker prompt at its generated path. It
// never overwrites the prompt of another cycle that is still active.
func (r *Runner) writeGeneratedPrompt(cycle int, rel, content string) error {
	safe, err := workflow.SafeRelativePath(r.Workspace.Root, rel)
	if err != nil {
		return err
	}
	path := filepath.Join(r.Workspace.Root, filepath.FromSlash(safe))
	if existing, err := os.ReadFile(path); err == nil && string(existing) != content {
		for n := 1; n < cycle; n++ {
			other, err := r.Workspace.LoadCycleState(n)
			if err != nil || other.Orchestration == nil || other.Orchestration.Prompt == nil {
				continue
			}
			switch other.Phase {
			case ws.PhaseRejected, ws.PhaseCompleted, ws.PhaseNoChange:
				continue
			}
			if other.Markdown != nil && ws.GeneratedPromptPath(other.Markdown.Slug, other.Orchestration.Worker) == safe {
				return fmt.Errorf("%s belongs to active cycle %03d; use a distinct ticket title or finish that cycle first", safe, n)
			}
		}
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prompt-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(content); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func renderProposal(p orchestrator.Proposal, v orchestrator.Validated) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Parent agent proposal\n\n%s\n\n## Worker: %s (%s)\n\n%s\n\nDifficulty: %s — %s\n\nModel: %s (category %s) · temperature %.2f · max_tokens %d · max_steps %d\nModel rationale: %s\n\nCategory options:\n", p.Summary, v.Worker.ID, v.Worker.Name, v.Worker.Purpose, p.Worker.TaskComplexity, p.Worker.ComplexityReason, v.Worker.Model, v.Worker.ModelCategory, v.Worker.Temperature, v.Worker.MaxTokens, v.Worker.MaxSteps, p.Worker.ModelReason)
	for _, o := range v.Config.Models[v.Worker.ModelCategory].Options {
		price := v.Config.Execution.Prices[o.Model]
		fmt.Fprintf(&b, "- %s — covers: %s — $%.3f / $%.3f per million — %s\n", o.Model, strings.Join(o.Difficulty, ", "), price.Input, price.Output, o.Strengths)
	}
	b.WriteString("\n")
	if p.Worker.BasePrompt != "" {
		fmt.Fprintf(&b, "Base prompt: %s\n", p.Worker.BasePrompt)
	}
	fmt.Fprintf(&b, "Generated prompt: %s\n\n## Task %s — %s\n\n%s\n\nCriteria:\n", v.PromptPath, v.Task.ID, v.Task.Title, v.Task.Description)
	for _, c := range v.Task.Criteria {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	fmt.Fprintf(&b, "\nOwned files:\n")
	for _, o := range v.Task.Outputs {
		fmt.Fprintf(&b, "- %s\n", o)
	}
	fmt.Fprintf(&b, "\nChecks: %s\nCommit scope: %s (%s)\n", strings.Join(v.Task.Evidence, ", "), p.CommitScope.Scope, p.CommitScope.Module)
	return b.String()
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
