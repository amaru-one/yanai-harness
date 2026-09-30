package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// proposalFiles are the parent's reviewed artifacts for a cycle.
type proposalFiles struct {
	Original    []byte
	Proposed    []byte
	Config      *config.Config
	OriginalCfg *config.Config
	Prompt      []byte
}

func (r *Runner) proposalFiles(st *ws.State) (proposalFiles, error) {
	var f proposalFiles
	o := st.Orchestration
	if o == nil || o.ConfigProposed == nil || o.Prompt == nil {
		return f, errors.New("this cycle has no parent proposal; run 'yanai plan <ticket.md>'")
	}
	artifacts := workflow.ArtifactStore{Root: r.Workspace.Root}
	var err error
	if f.Original, err = artifacts.Read(r.Workspace.Store, st.Cycle, o.ConfigOriginal.ID); err != nil {
		return f, err
	}
	if f.Proposed, err = artifacts.Read(r.Workspace.Store, st.Cycle, o.ConfigProposed.ID); err != nil {
		return f, err
	}
	if f.Prompt, err = artifacts.Read(r.Workspace.Store, st.Cycle, o.Prompt.ID); err != nil {
		return f, err
	}
	if f.Config, err = config.Parse(f.Proposed, r.Workspace.Root); err != nil {
		return f, fmt.Errorf("recorded proposed config: %w", err)
	}
	if f.OriginalCfg, err = config.Parse(f.Original, r.Workspace.Root); err != nil {
		return f, fmt.Errorf("recorded original config: %w", err)
	}
	return f, nil
}

// levelZeroContract builds the executable contract for the cycle's proposal
// and verifies every reviewed input is still exactly what the parent saw.
func (r *Runner) levelZeroContract(st *ws.State) (workflow.ExecutionContract, proposalFiles, error) {
	var c workflow.ExecutionContract
	store := r.Workspace.Store
	if st.Markdown == nil || st.Plan == nil || len(st.Plan.Tickets) != 1 || st.Orchestration == nil {
		return c, proposalFiles{}, errors.New("this cycle has no single-worker plan to review")
	}
	if err := store.RequireResolved(st.Cycle); err != nil {
		return c, proposalFiles{}, err
	}
	files, err := r.proposalFiles(st)
	if err != nil {
		return c, files, err
	}
	o := st.Orchestration
	// The live configuration must still be the one the parent read; the
	// proposal replaces it only on approval.
	live, err := os.ReadFile(filepath.Join(r.Workspace.Root, config.FileName))
	if err != nil {
		return c, files, err
	}
	if string(live) != string(files.Original) {
		return c, files, fmt.Errorf("%s changed since the parent's proposal; reject this cycle and plan again", config.FileName)
	}
	agent := files.Config.Agents[o.Worker]
	prompt, err := r.Workspace.ReadWorkspaceFile(agent.Prompt)
	if err != nil {
		return c, files, fmt.Errorf("generated prompt %s: %w", agent.Prompt, err)
	}
	if prompt.Content != string(files.Prompt) {
		return c, files, fmt.Errorf("generated prompt %s was edited after the proposal; reject and plan again", agent.Prompt)
	}
	documents := map[string]string{}
	docs, err := r.Workspace.ProjectDocuments()
	if err != nil {
		return c, files, err
	}
	for _, d := range docs {
		documents[d.Path] = d.SHA256
		if o.InputHashes["doc:"+d.Path] != d.SHA256 {
			return c, files, fmt.Errorf("%s changed since the parent's proposal; reject this cycle and plan again", d.Path)
		}
	}
	base := map[string]string{}
	prompts, err := r.Workspace.BasePrompts()
	if err != nil {
		return c, files, err
	}
	for _, d := range prompts {
		base[d.Path] = d.SHA256
	}
	for key, sha := range o.InputHashes {
		if name, ok := strings.CutPrefix(key, "base:"); ok && base[name] != sha {
			return c, files, fmt.Errorf("base prompt %s changed since the parent's proposal; reject and plan again", name)
		}
	}
	for name := range base {
		if _, ok := o.InputHashes["base:"+name]; !ok {
			return c, files, fmt.Errorf("base prompt %s was added after the parent's proposal; reject and plan again", name)
		}
	}
	record, err := store.Proposal(st.Cycle, o.Proposal)
	if err != nil {
		return c, files, err
	}
	if record.State != workflow.ProposalValid || record.InputHash != o.ProposalInput {
		return c, files, errors.New("the recorded proposal is not a valid answer to this cycle's inputs")
	}

	// The target the proposal names, validated freshly.
	target, err := repository.Open(files.Config.Repo, r.Workspace.Root)
	if err != nil {
		return c, files, err
	}
	snapshot, err := target.Snapshot()
	if err != nil {
		return c, files, err
	}
	if snapshot.Dirty {
		return c, files, errors.New("the target checkout has uncommitted or untracked changes; execution needs a clean committed baseline")
	}
	if snapshot.Root == st.Orchestration.RepoRoot && snapshot.Baseline() != st.BaselineHash {
		return c, files, errors.New("the repository changed since planning; reject this cycle and plan again")
	}
	branch, err := target.Branch()
	if err != nil {
		return c, files, err
	}
	if strings.HasPrefix(branch, "DETACHED@") {
		return c, files, errors.New("the checkout has a detached HEAD; check out the branch the ticket starts from")
	}
	ticketBranch := st.Markdown.TicketBranch()
	if err = target.ValidBranchName(ticketBranch); err != nil {
		return c, files, err
	}
	if tip, err := target.BranchTip(ticketBranch); err != nil {
		return c, files, err
	} else if tip != "" {
		return c, files, fmt.Errorf("branch %s already exists; use a distinct ticket title (existing branches are never reused or reset)", ticketBranch)
	}
	if _, _, err = target.Identity(); err != nil {
		return c, files, err
	}
	inputs, err := r.approvedCheckInputs(st)
	if err != nil {
		return c, files, err
	}
	if issue := executor.Preflight(files.Config.Repo, r.Workspace.Root, files.Config.Execution, inputs); issue != nil {
		return c, files, r.recordCheckBlocker(st, issue)
	}
	if err = r.checkWorkerBudget(st.Cycle, files.Config.Execution); err != nil {
		return c, files, err
	}
	for _, t := range st.Plan.Tickets {
		rec, err := store.GetTicket(st.Cycle, t.ID)
		if err != nil {
			return c, files, err
		}
		var stored workflow.Ticket
		if err = json.Unmarshal([]byte(rec.Payload), &stored); err != nil {
			return c, files, err
		}
		if workflow.Digest(rec.Payload) != workflow.Digest(string(mustJSON(t))) {
			return c, files, fmt.Errorf("stored ticket %s differs from the reviewed plan", t.ID)
		}
	}
	hash, _ := workflow.Hash(st.Plan)
	scope, _ := workflow.Hash(st.Markdown)
	if hash != st.PlanHash || scope != st.ScopeHash {
		return c, files, errors.New("the reviewed plan changed")
	}

	provider, _ := json.Marshal(files.Config.OpenRouter)
	if !files.Config.CategoryOf(agent.ModelCategory, agent.Model) {
		return c, files, fmt.Errorf("worker model %s is not an option of catalog category %q", agent.Model, agent.ModelCategory)
	}
	var options []workflow.ModelOptionTerms
	for _, opt := range files.Config.Models[agent.ModelCategory].Options {
		price := files.Config.Execution.Prices[opt.Model]
		options = append(options, workflow.ModelOptionTerms{Model: opt.Model, Difficulty: append([]string(nil), opt.Difficulty...), Strengths: opt.Strengths, Weaknesses: opt.Weaknesses, ContextTokens: opt.ContextTokens, ReasoningMaxTokens: opt.ReasoningMaxTokens, InputUSD: price.Input, OutputUSD: price.Output})
	}
	worker := workflow.WorkerTerms{ID: agent.ID, Name: agent.Name, Purpose: agent.Purpose, Model: agent.Model, Temperature: agent.Temperature, MaxTokens: agent.MaxTokens, MaxSteps: agent.MaxSteps, Prompt: agent.Prompt, PromptSHA256: prompt.SHA256,
		ModelCategory: agent.ModelCategory, ModelOptions: options, ModelReason: o.ModelReason, TaskComplexity: o.TaskComplexity, ComplexityReason: o.ComplexityReason}
	terms := &workflow.LevelZeroTerms{
		Proposal: o.Proposal, TicketType: st.Markdown.Type, Slug: st.Markdown.Slug, TicketBranch: ticketBranch,
		OriginalBranch: branch, BaseSHA: snapshot.Head, CommitScope: o.CommitScope, CommitModule: o.CommitModule,
		Worker: worker, ToolProtocol: workflow.ToolProtocolVersion,
		ConfigOriginalSHA: workflow.Digest(string(files.Original)), ConfigProposedSHA: workflow.Digest(string(files.Proposed)),
		ConfigProposed: *o.ConfigProposed, ProjectDocuments: documents, BasePrompts: base, Provider: provider,
	}
	contractInputs := map[string]string{"source": st.Markdown.Revision, "prompt:" + agent.ID: prompt.SHA256, "config:original": terms.ConfigOriginalSHA, "config:proposed": terms.ConfigProposedSHA, "proposal": o.Proposal}
	for path, sha := range documents {
		contractInputs["doc:"+path] = sha
	}
	for path, sha := range base {
		contractInputs["base:"+path] = sha
	}
	settings, _ := json.Marshal(struct {
		Worker   config.Agent
		Repo     config.Repo
		Provider config.OpenRouter
	}{agent, files.Config.Repo, files.Config.OpenRouter})
	repo, _ := json.Marshal(snapshot)
	identity := workflow.ExecutionIdentity{Backend: workflow.ExecutionBackendNative, CandidateSchema: workflow.CandidateSchemaVersion, Root: snapshot.Root, CommonDir: snapshot.CommonDir, Head: snapshot.Head, Branch: branch}
	c = workflow.ExecutionContract{Version: workflow.ContractVersion, Execution: identity, Plan: *st.Plan, ContextHash: st.ScopeHash, Baseline: snapshot.Baseline(), Repository: repo, Inputs: contractInputs, Settings: settings, Policy: files.Config.Execution, Level0: terms}
	if err = c.ValidateExecutable(snapshot, branch); err != nil {
		return c, files, err
	}
	return c, files, nil
}

// Review shows what the human is asked to approve, or, after execution, the
// final report and the proposed project-state update. It never calls a model.
func (r *Runner) Review() (string, error) {
	st, err := r.Workspace.LoadState()
	if err != nil {
		return "", err
	}
	if st.Orchestration == nil {
		return "", errors.New("this cycle predates the parent-led workflow; invalidate it and plan the ticket again")
	}
	switch st.Phase {
	case ws.PhaseAwaitingReview, ws.PhaseCompleted:
		return r.closingReview(st)
	case ws.PhaseApproved, ws.PhaseAwaitingExecution:
		c, err := r.Workspace.Store.Contract(st.Cycle, st.Approval.ContractHash)
		if err != nil {
			return "", err
		}
		token, err := r.Workspace.Store.ObservationReviewHash(st.Cycle, st.Approval.ContractHash)
		if err != nil {
			return "", err
		}
		observations, err := r.Workspace.Store.Observations(st.Cycle)
		if err != nil {
			return "", err
		}
		raw, _ := json.MarshalIndent(observations, "", "  ")
		return fmt.Sprintf("Contract: %s\nCycle %03d is already approved (contract %s).\nApproving again only confirms the human responses below under the same contract; it cannot change scope.\n\nObservations:\n%s\n", token, st.Cycle, shortHash(st.Approval.ContractHash), raw), c.Policy.Validate()
	case ws.PhaseWaiting:
	default:
		return "", fmt.Errorf("cycle %03d is in phase %q; there is nothing to review", st.Cycle, st.Phase)
	}
	c, files, err := r.levelZeroContract(st)
	if err != nil {
		return "", err
	}
	hash, err := r.Workspace.Store.SaveContract(st.Cycle, c)
	if err != nil {
		return "", err
	}
	token, err := r.Workspace.Store.ObservationReviewHash(st.Cycle, hash)
	if err != nil {
		return "", err
	}
	return r.renderReview(st, c, files, token)
}

func (r *Runner) renderReview(st *ws.State, c workflow.ExecutionContract, files proposalFiles, token string) (string, error) {
	var b strings.Builder
	l := c.Level0
	t := c.Plan.Tickets[0]
	fmt.Fprintf(&b, "Contract: %s\n", token)
	fmt.Fprintf(&b, "Cycle %03d · %s (%s) · awaiting your approval\n\n%s\n", st.Cycle, st.Markdown.Title, st.Markdown.Type, st.Orchestration.Summary)
	fmt.Fprintf(&b, "\n== Worker created by the parent ==\n%s — %s\nPurpose: %s\nTask difficulty: %s — %s\nModel: %s · category %s\n", l.Worker.ID, l.Worker.Name, l.Worker.Purpose, l.Worker.TaskComplexity, l.Worker.ComplexityReason, l.Worker.Model, l.Worker.ModelCategory)
	b.WriteString(renderModelOptions(l.Worker))
	fmt.Fprintf(&b, "Parent's reason: %s\n", l.Worker.ModelReason)
	if cheaper, ok := cheaperOption(l.Worker); ok {
		fmt.Fprintf(&b, "WARNING: a cheaper option also covers %q: %s ($%.3f in / $%.3f out per million)\n", l.Worker.TaskComplexity, cheaper.Model, cheaper.InputUSD, cheaper.OutputUSD)
	}
	fmt.Fprintf(&b, "Settings: temperature %.2f · max_tokens %d · max_steps %d\n", l.Worker.Temperature, l.Worker.MaxTokens, l.Worker.MaxSteps)
	if st.Orchestration.BasePrompt != "" {
		fmt.Fprintf(&b, "Base prompt: %s\n", st.Orchestration.BasePrompt)
	}
	fmt.Fprintf(&b, "Generated prompt: %s (sha256 %s)\n\n%s\n", l.Worker.Prompt, shortHash(l.Worker.PromptSHA256), indent(string(files.Prompt)))
	fmt.Fprintf(&b, "\n== The one task ==\n%s — %s\n%s\nCriteria:\n", t.ID, t.Title, t.Description)
	for _, cr := range t.Criteria {
		fmt.Fprintf(&b, "  - %s\n", cr)
	}
	fmt.Fprintf(&b, "Expected files (the plan; the worker may change other repository files when needed):\n")
	for _, p := range t.Outputs {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	fmt.Fprintf(&b, "Required checks before every commit: %s (max attempts %d)\n", strings.Join(t.Evidence, ", "), t.MaxAttempts)
	fmt.Fprintf(&b, "\n== Checks (commands run without a shell) ==\n")
	for _, check := range c.Policy.Checks {
		fmt.Fprintf(&b, "  %s: %s  (dir %s, timeout %ds, evidence %s)\n", check.ID, strings.Join(check.Args, " "), check.Dir, check.TimeoutSeconds, orDefault(check.Evidence, "go adapter"))
	}
	fmt.Fprintf(&b, "\n== Git ==\nCheckout: %s\nOriginal branch: %s at %s (stays unchanged; the checkout returns to it at the end)\nTicket branch to create: %s\nCommit format: %s(%s): <summary> + Yanai-Ticket: %s / Yanai-Agent: %s trailers\nModule for scope %q: %s\nNo merge, push, rebase, deploy or destructive database action is possible.\n",
		c.Execution.Root, l.OriginalBranch, shortHash(l.BaseSHA), l.TicketBranch, l.TicketType, l.CommitScope, l.Slug, l.Worker.ID, l.CommitScope, l.CommitModule)
	p := c.Policy
	fmt.Fprintf(&b, "\n== Budgets ==\nWorker: max_tokens %d · max_cost_usd %.4f · max_calls %d · max_active_seconds %d · max_repairs %d\n", p.MaxTokens, p.MaxCostUSD, p.MaxCalls, p.MaxActiveSeconds, p.MaxRepairs)
	if parent, err := r.Workspace.Store.ParentBudget(st.Cycle); err == nil {
		fmt.Fprintf(&b, "Parent (spent so far): %d tokens · $%.4f · %d calls of %d\n", parent.Tokens, parent.Cost, parent.Calls, parent.Policy.MaxCalls)
	}
	if notes := configHighlights(files.OriginalCfg, files.Config); len(notes) > 0 {
		b.WriteString("\n== Attention ==\n")
		for _, n := range notes {
			fmt.Fprintf(&b, "  ! %s\n", n)
		}
	}
	fmt.Fprintf(&b, "\n== Configuration diff (keys sorted; applied only when you approve) ==\n%s", lineDiff(canonicalJSON(files.Original), canonicalJSON(files.Proposed), config.FileName, config.FileName+" (proposed)"))
	observations, err := r.Workspace.Store.Observations(st.Cycle)
	if err != nil {
		return "", err
	}
	if len(observations) > 0 {
		raw, _ := json.MarshalIndent(observations, "", "  ")
		fmt.Fprintf(&b, "\n== Observations and your responses (included in the token) ==\n%s\n", raw)
	}
	fmt.Fprintf(&b, "\nApprove with: yanai approve --contract %s\n", token)
	return b.String(), nil
}

func indent(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("    │ ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// configHighlights names the changes a reviewer must not miss.
func configHighlights(before, after *config.Config) []string {
	var notes []string
	within := func(path string, allowed []string) bool {
		for _, a := range allowed {
			if a == "." || path == a || strings.HasPrefix(path, a+"/") {
				return true
			}
		}
		return false
	}
	for _, p := range after.Repo.AllowedPaths {
		if !within(p, before.Repo.AllowedPaths) {
			notes = append(notes, fmt.Sprintf("allowed path widened: %s", p))
		}
	}
	if before.Repo.Path != after.Repo.Path {
		notes = append(notes, fmt.Sprintf("target repository changes: %s → %s", before.Repo.Path, after.Repo.Path))
	}
	x, y := before.Execution, after.Execution
	if y.MaxTokens > x.MaxTokens || y.MaxCostUSD > x.MaxCostUSD || y.MaxCalls > x.MaxCalls || y.MaxActiveSeconds > x.MaxActiveSeconds || y.MaxRepairs > x.MaxRepairs {
		notes = append(notes, "worker execution limits are higher than the current configuration")
	}
	if !x.Commit && y.Commit {
		notes = append(notes, "commit permission is enabled (ticket branch only)")
	}
	oldChecks, newChecks := map[string]string{}, map[string]string{}
	for _, c := range x.Checks {
		oldChecks[c.ID] = string(mustJSON(c))
	}
	for _, c := range y.Checks {
		newChecks[c.ID] = string(mustJSON(c))
	}
	var changed []string
	for id, v := range newChecks {
		if oldChecks[id] != v {
			changed = append(changed, id)
		}
	}
	for id := range oldChecks {
		if _, ok := newChecks[id]; !ok {
			changed = append(changed, id+" (removed)")
		}
	}
	sort.Strings(changed)
	if len(changed) > 0 {
		notes = append(notes, "checks changed: "+strings.Join(changed, ", "))
	}
	if string(mustJSON(x.Tools)) != string(mustJSON(y.Tools)) {
		notes = append(notes, "check tool binaries changed")
	}
	if string(mustJSON(x.Prices)) != string(mustJSON(y.Prices)) {
		notes = append(notes, "model price bounds changed")
	}
	if string(mustJSON(before.Models)) != string(mustJSON(after.Models)) {
		notes = append(notes, "the model catalog changes (the parent is not allowed to change it)")
	}
	if string(mustJSON(before.OpenRouter)) != string(mustJSON(after.OpenRouter)) {
		notes = append(notes, "provider settings change (the worker uses them after approval)")
	}
	if string(mustJSON(before.Orchestrator)) != string(mustJSON(after.Orchestrator)) {
		notes = append(notes, "parent settings change (they apply from the next cycle)")
	}
	return notes
}

// Approve records the single human execution approval and activates the
// approved configuration. For an already approved cycle it only confirms
// human responses to execution observations under the same contract.
func (r *Runner) Approve(note, reviewedHash string) (*ws.State, error) {
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	store := r.Workspace.Store
	if st.Phase == ws.PhaseApproved || st.Phase == ws.PhaseAwaitingExecution {
		expected, err := store.ObservationReviewHash(st.Cycle, st.Approval.ContractHash)
		if err != nil {
			return st, err
		}
		if reviewedHash == "" || reviewedHash != expected {
			return st, errors.New("reviewed contract changed before approval; review again")
		}
		if err = store.ApproveObservations(st.Cycle, st.StateVersion, st.Approval.ContractHash, reviewedHash); err != nil {
			return st, err
		}
		st, err = r.Workspace.LoadState()
		if err != nil {
			return st, err
		}
		return st, store.HasApproval(st.Cycle, st.Approval.ContractHash)
	}
	if st.Phase != ws.PhaseWaiting {
		return st, fmt.Errorf("cycle %03d is in phase %q; only a plan awaiting approval can be approved", st.Cycle, st.Phase)
	}
	c, files, err := r.levelZeroContract(st)
	if err != nil {
		return st, err
	}
	hash, err := store.SaveContract(st.Cycle, c)
	if err != nil {
		return st, err
	}
	expected, err := store.ObservationReviewHash(st.Cycle, hash)
	if err != nil {
		return st, err
	}
	if reviewedHash == "" || reviewedHash != expected {
		return st, errors.New("reviewed contract changed before approval; review again")
	}
	if err = store.EnsureBudget(st.Cycle, c.Policy); err != nil {
		return st, err
	}
	at := time.Now().UTC()
	st.Approval = &ws.ApprovalBinding{Actor: workflow.ActorHuman, PlanHash: st.PlanHash, ScopeHash: st.ScopeHash, BaselineHash: c.Baseline, ContractHash: hash, ApprovedAt: at}
	st.Phase = ws.PhaseApproved
	st.Log("contract approved; configuration activation recorded", workflow.ActorHuman, note)
	payload, _ := json.Marshal(st)
	a := workflow.Approval{ID: fmt.Sprintf("A-%03d-%d", st.Cycle, at.UnixNano()), Cycle: st.Cycle, Actor: workflow.ActorHuman, PlanHash: st.PlanHash, ScopeHash: st.ScopeHash, Baseline: c.Baseline, ContractHash: hash, ApprovedAt: at}
	activation := &workflow.ConfigActivation{OriginalSHA: c.Level0.ConfigOriginalSHA, ProposedSHA: c.Level0.ConfigProposedSHA, Proposed: c.Level0.ConfigProposed}
	if err = store.ApproveContractWithActivation(a, st.StateVersion, string(payload), activation); err != nil {
		return st, err
	}
	if err = ActivateConfig(r.Workspace, st.Cycle, hash, files.Proposed); err != nil {
		return st, fmt.Errorf("approval recorded but the configuration was not activated (rerun any command to finish): %w", err)
	}
	if st, err = r.Workspace.LoadState(); err != nil {
		return st, err
	}
	return st, r.Workspace.RefreshProjection(st)
}

// ActivateConfig replaces the live configuration with the approved one and
// marks the activation done. It is safe to repeat: the live file must be
// either the original or the approved version; anything else is an operator
// edit, which is never overwritten.
func ActivateConfig(w *ws.Workspace, cycle int, contract string, proposed []byte) error {
	a, found, err := w.Store.ConfigActivation(cycle, contract)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("no configuration activation recorded")
	}
	if a.State == workflow.ActivationDone {
		return nil
	}
	if proposed == nil {
		if proposed, err = (workflow.ArtifactStore{Root: w.Root}).Read(w.Store, cycle, a.Proposed.ID); err != nil {
			return err
		}
	}
	if workflow.Digest(string(proposed)) != a.ProposedSHA {
		return errors.New("approved configuration artifact does not match its recorded hash")
	}
	path := filepath.Join(w.Root, config.FileName)
	live, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	switch workflow.Digest(string(live)) {
	case a.ProposedSHA:
	case a.OriginalSHA:
		tmp, err := os.CreateTemp(w.Root, ".config-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err = tmp.Write(proposed); err == nil {
			err = tmp.Sync()
		}
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Chmod(tmp.Name(), 0o600)
		}
		if err != nil {
			return err
		}
		if err = os.Rename(tmp.Name(), path); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s was edited while the approved configuration was being activated; restore either the original or the approved version (cycles/%03d/proposal/yanai.config.json)", config.FileName, cycle)
	}
	return w.Store.CompleteConfigActivation(cycle, contract)
}

// RecoverConfigActivations finishes any activation an interrupted approval
// left pending. Commands run it before trusting the live configuration.
func RecoverConfigActivations(w *ws.Workspace) (bool, error) {
	pending, cycles, err := w.Store.PendingConfigActivations()
	if err != nil {
		return false, err
	}
	for i, a := range pending {
		if err = ActivateConfig(w, cycles[i], a.Contract, nil); err != nil {
			return false, err
		}
	}
	return len(pending) > 0, nil
}

// checkApproval re-reads everything the approval bound before each model
// call and each repository action of the worker.
func (r *Runner) checkApproval(st *ws.State, c workflow.ExecutionContract) error {
	store := r.Workspace.Store
	if err := store.HasApproval(st.Cycle, st.Approval.ContractHash); err != nil {
		return err
	}
	a, found, err := store.ConfigActivation(st.Cycle, st.Approval.ContractHash)
	if err != nil {
		return err
	}
	if !found || a.State != workflow.ActivationDone {
		return errors.New("the approved configuration is not active yet")
	}
	live, err := os.ReadFile(filepath.Join(r.Workspace.Root, config.FileName))
	if err != nil {
		return err
	}
	if workflow.Digest(string(live)) != c.Level0.ConfigProposedSHA {
		return fmt.Errorf("%s changed after approval; restore the approved version or invalidate the cycle", config.FileName)
	}
	prompt, err := r.Workspace.ReadWorkspaceFile(c.Level0.Worker.Prompt)
	if err != nil {
		return err
	}
	if prompt.SHA256 != c.Level0.Worker.PromptSHA256 {
		return fmt.Errorf("the approved worker prompt %s changed; restore it or invalidate the cycle", c.Level0.Worker.Prompt)
	}
	for path, sha := range c.Level0.ProjectDocuments {
		d, err := r.Workspace.ReadWorkspaceFile(path)
		if err != nil {
			return err
		}
		if d.SHA256 != sha {
			return fmt.Errorf("%s changed after approval; the worker's approved inputs must not change during execution", path)
		}
	}
	return nil
}

// canonicalJSON re-renders a JSON document with sorted keys so a review diff
// shows changed values, not reordered ones.
func canonicalJSON(data []byte) string {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(data)
	}
	return string(out) + "\n"
}

// renderModelOptions lists the category's options with what each covers.
func renderModelOptions(w workflow.WorkerTerms) string {
	var b strings.Builder
	b.WriteString("Options:\n")
	for _, o := range w.ModelOptions {
		mark := " "
		if o.Model == w.Model {
			mark = "*"
		}
		fmt.Fprintf(&b, "  %s %s — covers %s — context %d tokens — $%.3f in / $%.3f out per million — %s\n", mark, o.Model, strings.Join(o.Difficulty, ", "), o.ContextTokens, o.InputUSD, o.OutputUSD, o.Strengths)
	}
	return b.String()
}

// cheaperOption returns the cheapest option, cheaper than the chosen one, that
// also covers the task's difficulty. Cost is input plus output price.
func cheaperOption(w workflow.WorkerTerms) (workflow.ModelOptionTerms, bool) {
	cost := func(o workflow.ModelOptionTerms) float64 { return o.InputUSD + o.OutputUSD }
	var chosen, best workflow.ModelOptionTerms
	found := false
	for _, o := range w.ModelOptions {
		if o.Model == w.Model {
			chosen = o
		}
	}
	for _, o := range w.ModelOptions {
		if o.Model == w.Model || !covers(o, w.TaskComplexity) || cost(o) >= cost(chosen) {
			continue
		}
		if !found || cost(o) < cost(best) {
			best, found = o, true
		}
	}
	return best, found
}

func covers(o workflow.ModelOptionTerms, difficulty string) bool {
	for _, d := range o.Difficulty {
		if d == difficulty {
			return true
		}
	}
	return false
}
