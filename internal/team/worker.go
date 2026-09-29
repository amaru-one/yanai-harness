package team

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/openrouter"
	"github.com/yanai/yanai-harness/internal/orchestrator"
	"github.com/yanai/yanai-harness/internal/repoctx"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// maxFailureBytes bounds check output returned to the model; the complete
// output stays in the recorded evidence.
const maxFailureBytes = 12 << 10

const workerProtocol = `HARNESS PROTOCOL (level 0; mandatory, takes precedence over other instructions):
- Write prompts, explanations, observations, commit messages, and Markdown documents in English. Preserve literal paths, identifiers, and schema values.
- You work on ticket branch %s in the real repository. You may read, create, modify, or delete any repository file except protected ones (.git, .env files, keys, credentials, ignored files). The expected files listed in your first message are the plan; when the task needs other files, change them and say why in finish.
- Exactly one tool per turn. If the harness says only finish is available, call it with what you have.
- Keep your reasoning brief and make one change per turn: your output limit covers reasoning and the tool call together, and a turn cut off at the limit is wasted.
- Files under "Files already loaded" in your first message are current, with their sha256; do not read them again. To find code, use grep (regular expression over file contents) or list_files (paths and sizes) first, then read only what you need. read_file takes up to 10 paths and returns each file and its sha256 (start_line/end_line read only a range); every turn resends the whole conversation, so request all the files you need in one call.
- To change an existing file, prefer edit_file: it replaces one exact piece of text that occurs exactly once (pass the file's current sha256). write_file writes the COMPLETE content of a file (never fragments or "the rest stays the same"); use it to create files (expected_absent=true) or to rewrite most of one (expected_sha256); delete=true deletes it. Every write returns the new sha256 for your next edit.
- run_command asks the human to run any command (docker compose, npm, curl, bash -c for pipelines, ...). Every command needs their approval, so the run pauses until they decide, and they may deny it: prefer the built-in tools and approved checks, and give a clear reason. Files the command changes become part of your work.
- run_check executes an approved check by id. Before each commit, all required checks (%s) must pass against the current state; any edit invalidates previous results.
- commit commits ALL your pending changes. Use a Conventional Commits message: first line "%s(%s): <summary>", optional body, and the exact trailers "Yanai-Ticket: %s" and "Yanai-Agent: %s" at the end. The harness adds its own operation trailer; do not write other Yanai- trailers.
- Call finish when done: result="completed" (commits exist, no uncommitted changes remain, and all approved checks pass against the final state; the harness reruns them), "no_change" (the task needs no changes: no commits or edits; explain why), or "blocked" with observations for the human (for example, when you need a variable, tool, secret, or configuration change). Human responses do not expand the approved contract.
- If a check fails because of the environment (missing variable, tool, or library), report an observation; do not change code to hide the failure.
- Do not claim success without evidence: the harness verifies commits and checks.`

func workerTools() []openrouter.Tool {
	return []openrouter.Tool{
		orchestrator.ReadFileTool(),
		orchestrator.ListFilesTool(),
		orchestrator.GrepTool(),
		orchestrator.RunCommandTool(),
		{Type: "function", Function: openrouter.ToolFunction{Name: "write_file", Description: "Write, create or delete one repository file with complete content.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"expected_sha256":{"type":"string"},"expected_absent":{"type":"boolean"},"delete":{"type":"boolean"}},"required":["path"],"additionalProperties":false}`)}},
		{Type: "function", Function: openrouter.ToolFunction{Name: "edit_file", Description: "Replace one exact, unique piece of text in an existing repository file. Cheaper than rewriting the whole file.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string","description":"exact text to replace; must occur exactly once"},"new":{"type":"string"},"expected_sha256":{"type":"string"}},"required":["path","old","new","expected_sha256"],"additionalProperties":false}`)}},
		{Type: "function", Function: openrouter.ToolFunction{Name: "run_check", Description: "Run one approved check by id against the current checkout.", Parameters: json.RawMessage(`{"type":"object","properties":{"check_id":{"type":"string"}},"required":["check_id"],"additionalProperties":false}`)}},
		{Type: "function", Function: openrouter.ToolFunction{Name: "commit", Description: "Commit every pending change on the ticket branch with a Conventional Commits message.", Parameters: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"],"additionalProperties":false}`)}},
		{Type: "function", Function: openrouter.ToolFunction{Name: "finish", Description: "End the run: completed, no_change, or blocked with observations.", Parameters: json.RawMessage(`{"type":"object","properties":{"result":{"type":"string","enum":["completed","no_change","blocked"]},"explanation":{"type":"string"},"observations":{"type":"array","items":{"type":"object","properties":{"description":{"type":"string"},"requirement":{"type":"string"},"question":{"type":"string"}},"required":["description","requirement","question"]}}},"required":["result","explanation"],"additionalProperties":false}`)}},
	}
}

// workerState is what earlier steps established, rebuilt from their
// recorded results on every run so a restart sees the same facts.
type workerState struct {
	passed      map[string]string // check ID → repository state it passed against
	failedSince bool              // a check failed since the last write phase began
	phases      int               // write phases started (1 + repairs)
}

func (s *workerState) observe(tool string, result map[string]any) {
	switch tool {
	case "write_file", "edit_file":
		if ok, _ := result["ok"].(bool); ok {
			if s.phases == 0 || s.failedSince {
				s.phases++
				s.failedSince = false
			}
			s.passed = map[string]string{}
		}
	case "run_command":
		// Files a command changed invalidate earlier check results like a write.
		if changed, _ := result["changed_files"].([]any); len(changed) > 0 {
			if s.phases == 0 || s.failedSince {
				s.phases++
				s.failedSince = false
			}
			s.passed = map[string]string{}
		}
	case "run_check":
		id, _ := result["check_id"].(string)
		tested, _ := result["tested_state"].(string)
		if passed, _ := result["passed"].(bool); passed {
			s.passed[id] = tested
		} else if id != "" {
			delete(s.passed, id)
			s.failedSince = true
		}
	case "commit":
		if ok, _ := result["ok"].(bool); ok {
			s.passed = map[string]string{}
		}
	}
}

// finishArgs is the worker's terminal tool call.
type finishArgs struct {
	Result       string                       `json:"result"`
	Explanation  string                       `json:"explanation"`
	Observations []workflow.ObservationDetail `json:"observations"`
}

// Execute runs the approved worker in the approved checkout, on its ticket
// branch. Everything it does is recorded; rerunning resumes where it stopped
// and never repeats a confirmed write, check or commit.
func (r *Runner) Execute(ctx context.Context, retry bool) (result *ws.State, err error) {
	if os.Getenv("YANAI_NO_REPO") == "1" {
		return nil, fmt.Errorf("run requires repository context; unset YANAI_NO_REPO")
	}
	if err = r.checkUnresolvedAttempts(retry); err != nil {
		return nil, err
	}
	store := r.Workspace.Store
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	if st.Orchestration == nil {
		return st, errors.New("this cycle predates the parent-led workflow and cannot run under it; invalidate it and plan the ticket again")
	}
	switch st.Phase {
	case ws.PhaseAwaitingReview:
		// Implementation is recorded; only the closing proposal may be missing.
		return r.close(ctx, st, retry)
	case ws.PhaseCompleted, ws.PhaseNoChange:
		return st, nil
	case ws.PhaseApproved, ws.PhaseAwaitingExecution:
	default:
		return st, fmt.Errorf("cycle %03d is in phase %q: nothing runs without the human's approval ('yanai review', 'yanai approve')", st.Cycle, st.Phase)
	}
	if st.Approval == nil || st.Approval.ContractHash == "" {
		return st, errors.New("approval is stale or incomplete; review and approve again")
	}
	contract, err := store.Contract(st.Cycle, st.Approval.ContractHash)
	if err != nil {
		return st, fmt.Errorf("approved contract is not recorded; review and approve again: %w", err)
	}
	if contract.Level0 == nil || contract.Version != workflow.ContractVersion {
		return st, errors.New("this approval predates branch-and-commit execution; invalidate it and plan the ticket again")
	}
	if err = r.checkApproval(st, contract); err != nil {
		return st, err
	}
	inputs, err := r.approvedCheckInputs(st)
	if err != nil {
		return st, err
	}
	for _, v := range inputs {
		r.Secrets = append(r.Secrets, v)
	}
	if issue := executor.Preflight(r.Cfg.Repo, r.Workspace.Root, contract.Policy, inputs); issue != nil {
		return st, r.recordCheckBlocker(st, issue)
	}
	options, err := executor.Defaults(executor.Options{Repo: r.Cfg.Repo, Store: store, Artifacts: workflow.ArtifactStore{Root: r.Workspace.Root}, Cycle: st.Cycle, Contract: st.Approval.ContractHash, CheckInputs: inputs})
	if err != nil {
		return st, r.recordCheckBlocker(st, err)
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
	r.guard = func() error { return r.checkApproval(st, contract) }
	defer func() { r.guard = nil }()

	session, err := backend.StartTicketBranch()
	if err != nil {
		return st, err
	}
	if session.State == workflow.GitSessionReturned {
		// The branch work finished and the checkout went back; only the
		// phase change was interrupted.
		if len(session.Commits) == 0 {
			return r.recordNoChange(st, contract, "the worker reported that no change was required")
		}
		return r.completeImplementation(ctx, st, contract, backend, retry)
	}
	if st.Phase == ws.PhaseAwaitingExecution {
		st.Phase = ws.PhaseApproved
		st.Log("execution resumed after human responses", "", "")
		if err = r.Workspace.SaveState(st, workflow.ActorEngine); err != nil {
			return st, err
		}
	}
	fmt.Fprintf(os.Stderr, "Working on %s in %s\n", session.TicketBranch, session.Root)

	outcome, err := r.runWorker(ctx, st, contract, backend, retry)
	if err != nil {
		return st, err
	}
	switch outcome.Result {
	case "completed":
		if _, err = backend.ReturnToOriginal(); err != nil {
			return st, err
		}
		return r.completeImplementation(ctx, st, contract, backend, retry)
	case "no_change":
		if _, err = backend.ReturnToOriginal(); err != nil {
			return st, err
		}
		return r.recordNoChange(st, contract, outcome.Explanation)
	default:
		if err = store.RecordObservations(st.Cycle, contract.Level0.Worker.ID, r.workerRunKey(st, contract), outcome.Observations); err != nil {
			return st, err
		}
		if st, err = r.Workspace.LoadState(); err != nil {
			return st, err
		}
		st.Phase = ws.PhaseAwaitingExecution
		st.Orchestration.Outcome, st.Orchestration.Explanation = "blocked", outcome.Explanation
		setTaskStatus(st, "blocked")
		st.Log("the worker paused for human responses; its branch and files are left as they are", contract.Level0.Worker.ID, outcome.Explanation)
		if err = r.Workspace.SaveState(st, workflow.ActorEngine); err != nil {
			return st, err
		}
		return st, fmt.Errorf("the worker needs human responses: inspect 'yanai status', answer with 'yanai resolve', then 'yanai review', 'yanai approve' and 'yanai run' to continue on %s", contract.Level0.TicketBranch)
	}
}

// recordNoChange ends the cycle with the existing no-change finding. It is
// not a successful implementation: nothing was committed.
func (r *Runner) recordNoChange(st *ws.State, c workflow.ExecutionContract, explanation string) (*ws.State, error) {
	st.Phase = ws.PhaseNoChange
	st.Orchestration.Outcome, st.Orchestration.Explanation = "no_change", explanation
	setTaskStatus(st, workflow.TicketNoChangeReported)
	st.Log("the worker found nothing to change; the checkout is back on its original branch", c.Level0.Worker.ID, explanation)
	return st, r.Workspace.SaveState(st, workflow.ActorEngine)
}

func setTaskStatus(st *ws.State, status string) {
	for i := range st.Tasks {
		st.Tasks[i].Status = status
	}
}

// workerRunKey names the worker run. A continuation after human responses
// is a new run on the same branch; the same responses resume the same run.
func (r *Runner) workerRunKey(st *ws.State, c workflow.ExecutionContract) string {
	observations, _ := r.Workspace.Store.Observations(st.Cycle)
	var resolved []string
	for _, o := range observations {
		if o.Resolution != "" {
			resolved = append(resolved, o.ID+"="+o.Resolution)
		}
	}
	sort.Strings(resolved)
	h, _ := workflow.Hash(resolved)
	return "work-" + shortHash(st.Approval.ContractHash)[:12] + "-" + shortHash(h)[:8]
}

func (r *Runner) runWorker(ctx context.Context, st *ws.State, c workflow.ExecutionContract, backend *executor.Native, retry bool) (finishArgs, error) {
	store := r.Workspace.Store
	terms := c.Level0
	task := c.Plan.Tickets[0]
	runKey := r.workerRunKey(st, c)
	system, err := r.Workspace.ReadWorkspaceFile(terms.Worker.Prompt)
	if err != nil {
		return finishArgs{}, err
	}
	initial, err := r.workerInput(st, c, backend, runKey)
	if err != nil {
		return finishArgs{}, err
	}

	// Rebuild what earlier steps established.
	state := &workerState{passed: map[string]string{}}
	steps, err := store.AgentSteps(st.Cycle, runKey)
	if err != nil {
		return finishArgs{}, err
	}
	for _, step := range steps {
		if step.State != workflow.StepDone || step.Result == nil {
			continue
		}
		raw, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(store, st.Cycle, step.Result.ID)
		if err != nil {
			return finishArgs{}, err
		}
		var result map[string]any
		if err = json.Unmarshal(raw, &result); err != nil {
			return finishArgs{}, err
		}
		state.observe(step.ToolName, result)
	}

	ctx, finish, err := r.beginWork(ctx, st.Cycle, workflow.BucketWorker, c.Policy)
	if err != nil {
		return finishArgs{}, err
	}
	defer func() {
		if e := finish(); err == nil {
			err = e
		}
	}()
	r.ticket = task.ID
	var final finishArgs
	run := agentRun{
		Key: runKey, Role: terms.Worker.ID, Bucket: workflow.BucketWorker, Model: terms.Worker.Model, Temperature: terms.Worker.Temperature,
		MaxTokens: terms.Worker.MaxTokens, MaxSteps: terms.Worker.MaxSteps, Policy: c.Policy,
		System:  system.Content + "\n\n" + fmt.Sprintf(workerProtocol, terms.TicketBranch, strings.Join(task.Evidence, ", "), terms.TicketType, terms.CommitScope, terms.Slug, terms.Worker.ID),
		Initial: initial, Tools: workerTools(), Final: "finish", Reasoning: approvedReasoningCap(terms), Retry: retry,
		Execute: func(ctx context.Context, step workflow.AgentStep, mark func(any) error) (toolOutcome, error) {
			if step.ToolName == "run_command" {
				return r.workerCommand(ctx, st.Cycle, agentRun{Key: runKey, Role: terms.Worker.ID}, backend, state, step, mark)
			}
			outcome, err := r.workerTool(ctx, st, c, backend, state, step, mark)
			if err == nil {
				state.observe(step.ToolName, encodeResult(outcome))
				if outcome.Terminal {
					_ = json.Unmarshal(step.Arguments, &final)
				}
			}
			return outcome, err
		},
	}
	result, err := r.runLoop(ctx, run)
	if err != nil {
		return finishArgs{}, err
	}
	if final.Result == "" {
		if err = json.Unmarshal(result.Step.Arguments, &final); err != nil {
			return finishArgs{}, err
		}
	}
	return final, nil
}

// workerCommand runs a run_command step once the human approved it.
func (r *Runner) workerCommand(ctx context.Context, cycle int, run agentRun, backend *executor.Native, state *workerState, step workflow.AgentStep, mark func(any) error) (toolOutcome, error) {
	req, outcome, err := r.commandGate(cycle, run, step, "yanai run --ws "+r.Workspace.Root)
	if err != nil || outcome != nil {
		if outcome != nil {
			return *outcome, nil
		}
		return toolOutcome{}, err
	}
	id := workflow.CommandID(run.Key, step.Seq)
	if err := mark(map[string]any{"command": id}); err != nil {
		return toolOutcome{}, err
	}
	fmt.Fprintf(os.Stderr, "    running approved command %s: %s\n", id, quoteArgs(req.Args))
	res, err := backend.RunCommand(ctx, id, req.Args, req.Dir, time.Duration(req.TimeoutSeconds)*time.Second)
	if err != nil && isGuardFailure(err) {
		return toolOutcome{}, err
	}
	if err != nil {
		return toolError("%v", err), nil
	}
	result := commandOutcome(res)
	state.observe(step.ToolName, encodeResult(result))
	return result, nil
}

func encodeResult(o toolOutcome) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(encodeOutcome(o), &m)
	return m
}

func (r *Runner) workerInput(st *ws.State, c workflow.ExecutionContract, backend *executor.Native, runKey string) (string, error) {
	terms := c.Level0
	task := c.Plan.Tickets[0]
	var b strings.Builder
	b.WriteString("# Project scope and state\n")
	docs := make([]string, 0, len(terms.ProjectDocuments))
	for path := range terms.ProjectDocuments {
		docs = append(docs, path)
	}
	sort.Strings(docs)
	for _, path := range docs {
		d, err := r.Workspace.ReadWorkspaceFile(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", path, d.Content)
	}
	source, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(r.Workspace.Store, st.Cycle, st.Markdown.Source.ID)
	if err != nil {
		return "", err
	}
	redacted, err := workflow.RedactCheckInputs(string(source))
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\n# Ticket\n\n%s\n", redacted)
	fmt.Fprintf(&b, "\n# Your approved task: %s — %s\n\n%s\n\nCriteria:\n", task.ID, task.Title, task.Description)
	for _, cr := range task.Criteria {
		fmt.Fprintf(&b, "- %s\n", cr)
	}
	b.WriteString("\nExpected files (the plan; other repository files may be changed when needed):\n")
	for _, out := range task.Outputs {
		fmt.Fprintf(&b, "- %s\n", out)
	}
	b.WriteString("\nApproved checks:\n")
	for _, check := range c.Policy.Checks {
		required := ""
		for _, id := range task.Evidence {
			if id == check.ID {
				required = " (required before each commit)"
			}
		}
		fmt.Fprintf(&b, "- %s: %s in %s%s\n", check.ID, strings.Join(check.Args, " "), check.Dir, required)
	}
	b.WriteString("\n# Repository instructions\n")
	for _, path := range governingPaths(task.Outputs) {
		// A convention path (AGENTS.md at the repo root, or up the output
		// tree) may sit outside repo.allowed_paths; that is a configuration
		// boundary, not a missing-file error, so it is skipped like one.
		if !backend.InAllowedPaths(path) {
			continue
		}
		f, err := backend.Read(path)
		if err != nil {
			return "", fmt.Errorf("repository instructions %s: %w", path, err)
		}
		if f != nil {
			fmt.Fprintf(&b, "\n## %s\n\n%s\n", path, f.Data)
		}
	}
	index, err := repoctx.Index(r.Cfg.Repo)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\n# Repository index\n\n%s\n", index)
	preloaded, err := r.preloadedFiles(st, task, backend, runKey)
	if err != nil {
		return "", err
	}
	b.WriteString(preloaded)
	if session, found, err := backend.GitSession(); err == nil && found && len(session.Commits) > 0 {
		b.WriteString("\n# Existing commits on the ticket branch\n")
		for _, commit := range session.Commits {
			fmt.Fprintf(&b, "- %s %s (%s)\n", commit.SHA[:12], commit.Subject, strings.Join(commit.Paths, ", "))
		}
	}
	if changed, err := backend.ChangedPaths(); err == nil && len(changed) > 0 {
		fmt.Fprintf(&b, "\n# Pending uncommitted changes (from an earlier run)\n%s\n", strings.Join(changed, "\n"))
	}
	observations, err := r.Workspace.Store.Observations(st.Cycle)
	if err != nil {
		return "", err
	}
	if len(observations) > 0 {
		raw, _ := json.MarshalIndent(observations, "", "  ")
		fmt.Fprintf(&b, "\n# Observations and human responses (do not expand the contract)\n\n%s\n", raw)
	}
	p := c.Policy
	fmt.Fprintf(&b, "\n# Limits\nSteps: %d · tokens %d · maximum cost $%.4f · calls %d · repairs %d · task attempts %d\n", terms.Worker.MaxSteps, p.MaxTokens, p.MaxCostUSD, p.MaxCalls, p.MaxRepairs, task.MaxAttempts)
	input, err := json.Marshal(openrouter.MockInput{Kind: "worker", TicketType: terms.TicketType, Slug: terms.Slug, Title: st.Markdown.Title, Task: task.Description, Role: terms.Worker.ID, Outputs: task.Outputs, Checks: task.Evidence, CommitScope: terms.CommitScope})
	if err != nil {
		return "", err
	}
	b.WriteString(openrouter.InputMarker)
	b.Write(input)
	return b.String(), nil
}

// governingPaths lists the repository instruction documents that govern the
// owned outputs: the root AGENTS.md and every AGENTS.md or SPEC.md on the way.
func governingPaths(outputs []string) []string {
	seen := map[string]bool{"AGENTS.md": true}
	paths := []string{"AGENTS.md"}
	for _, output := range outputs {
		for dir := filepath.ToSlash(filepath.Dir(output)); dir != "." && dir != "/"; dir = filepath.ToSlash(filepath.Dir(dir)) {
			for _, name := range []string{"AGENTS.md", "SPEC.md"} {
				p := dir + "/" + name
				if !seen[p] {
					seen[p] = true
					paths = append(paths, p)
				}
			}
		}
	}
	sort.Strings(paths)
	return paths
}

func fileSHA(f *executor.File) string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(f.Data))
}

func (r *Runner) workerTool(ctx context.Context, st *ws.State, c workflow.ExecutionContract, backend *executor.Native, state *workerState, step workflow.AgentStep, mark func(any) error) (toolOutcome, error) {
	task := c.Plan.Tickets[0]
	expected := func(path string) bool {
		for _, out := range task.Outputs {
			if out == path {
				return true
			}
		}
		return false
	}
	switch step.ToolName {
	case "read_file":
		req, err := orchestrator.ParseRead(string(step.Arguments))
		if err != nil {
			return toolError("%v", err), nil
		}
		files := make([]map[string]any, 0, len(req.Paths))
		for _, path := range req.Paths {
			f, err := backend.Read(path)
			switch {
			case err != nil && isGuardFailure(err):
				return toolOutcome{}, err
			case err != nil:
				files = append(files, map[string]any{"path": path, "error": err.Error()})
			case f == nil:
				files = append(files, map[string]any{"path": path, "exists": false, "expected_output": expected(path)})
			case req.Ranged():
				text, err := repository.Lines(f.Data, req.StartLine, req.EndLine)
				if err != nil {
					files = append(files, map[string]any{"path": path, "error": err.Error()})
					continue
				}
				// The sha256 is the whole file's, as edit_file and write_file expect.
				files = append(files, map[string]any{"path": path, "exists": true, "expected_output": expected(path), "mode": fmt.Sprintf("%o", f.Mode), "sha256": fileSHA(f), "start_line": max(req.StartLine, 1), "total_lines": repository.LineCount(f.Data), "content": text})
			default:
				files = append(files, map[string]any{"path": path, "exists": true, "expected_output": expected(path), "mode": fmt.Sprintf("%o", f.Mode), "sha256": fileSHA(f), "content": string(f.Data)})
			}
		}
		return toolOutcome{Result: map[string]any{"files": files}}, nil

	case "list_files", "grep":
		result, err := searchRepository(backend, step.ToolName, string(step.Arguments))
		if err != nil && isGuardFailure(err) {
			return toolOutcome{}, err
		}
		if err != nil {
			return toolError("%v", err), nil
		}
		return toolOutcome{Result: result}, nil

	case "write_file":
		var args struct {
			Path           string  `json:"path"`
			Content        *string `json:"content"`
			ExpectedSHA256 string  `json:"expected_sha256"`
			ExpectedAbsent bool    `json:"expected_absent"`
			Delete         bool    `json:"delete"`
		}
		if err := workflow.DecodeStrict(string(step.Arguments), &args); err != nil {
			return toolError("write_file arguments: %v", err), nil
		}
		if !task.Allows(args.Path) {
			return toolError("%s is outside the approved ticket; if the task needs it, finish with result=blocked and an observation", args.Path), nil
		}
		if args.Delete == (args.Content != nil) {
			return toolError("pass either content or delete=true"), nil
		}
		if args.ExpectedAbsent == (args.ExpectedSHA256 != "") {
			return toolError("pass exactly one of expected_sha256 (from read_file) or expected_absent=true"), nil
		}
		current, err := backend.Read(args.Path)
		if err != nil {
			if isGuardFailure(err) {
				return toolOutcome{}, err
			}
			return toolError("%v", err), nil
		}
		var next *executor.File
		if !args.Delete {
			mode := uint32(0o644)
			if current != nil {
				mode = current.Mode
			}
			next = &executor.File{Data: []byte(*args.Content), Mode: mode}
		}
		if step.State == workflow.StepIntent && fileSHA(current) == fileSHA(next) && (current == nil) == (next == nil) {
			// Applied before an interruption; the executor confirmed it.
			return toolOutcome{Result: map[string]any{"path": args.Path, "sha256": fileSHA(next), "deleted": next == nil, "note": "already applied before a restart"}}, nil
		}
		if args.ExpectedAbsent && current != nil {
			return toolError("%s already exists (sha256 %s); read it first", args.Path, fileSHA(current)), nil
		}
		if args.ExpectedSHA256 != "" && (current == nil || fileSHA(current) != args.ExpectedSHA256) {
			return toolError("%s does not match expected_sha256; read it again", args.Path), nil
		}
		if args.Delete && current == nil {
			return toolError("%s does not exist", args.Path), nil
		}
		return r.applyOwned(st, task, state, backend, mark, args.Path, current, next)

	case "edit_file":
		var args struct {
			Path           string `json:"path"`
			Old            string `json:"old"`
			New            string `json:"new"`
			ExpectedSHA256 string `json:"expected_sha256"`
		}
		if err := workflow.DecodeStrict(string(step.Arguments), &args); err != nil {
			return toolError("edit_file arguments: %v", err), nil
		}
		if !task.Allows(args.Path) {
			return toolError("%s is outside the approved ticket; if the task needs it, finish with result=blocked and an observation", args.Path), nil
		}
		if args.Old == "" || args.Old == args.New {
			return toolError("old must be nonempty text that differs from new"), nil
		}
		current, err := backend.Read(args.Path)
		if err != nil {
			if isGuardFailure(err) {
				return toolOutcome{}, err
			}
			return toolError("%v", err), nil
		}
		if current == nil {
			return toolError("%s does not exist; create it with write_file and expected_absent=true", args.Path), nil
		}
		if step.State == workflow.StepIntent {
			var intent struct {
				After string `json:"after"`
			}
			if json.Unmarshal(step.Intent, &intent) == nil && intent.After == fileSHA(current) {
				// Applied before an interruption; the executor confirmed it.
				return toolOutcome{Result: map[string]any{"path": args.Path, "sha256": fileSHA(current), "note": "already applied before a restart"}}, nil
			}
		}
		if fileSHA(current) != args.ExpectedSHA256 {
			return toolError("%s does not match expected_sha256 (it is %s now); read it again", args.Path, fileSHA(current)), nil
		}
		edited, err := replaceOnce(string(current.Data), args.Old, args.New)
		if err != nil {
			return toolError("%s: %v", args.Path, err), nil
		}
		next := &executor.File{Data: []byte(edited), Mode: current.Mode}
		return r.applyOwned(st, task, state, backend, mark, args.Path, current, next)

	case "run_check":
		var args struct {
			CheckID string `json:"check_id"`
		}
		if err := workflow.DecodeStrict(string(step.Arguments), &args); err != nil {
			return toolError("run_check arguments: %v", err), nil
		}
		var check *workflow.Check
		for i := range c.Policy.Checks {
			if c.Policy.Checks[i].ID == args.CheckID {
				check = &c.Policy.Checks[i]
			}
		}
		if check == nil {
			return toolError("check %q is not approved", args.CheckID), nil
		}
		if err := mark(map[string]any{"check_id": args.CheckID}); err != nil {
			return toolOutcome{}, err
		}
		fmt.Fprintf(os.Stderr, "    check %s…\n", check.ID)
		result, runErr := backend.Check(ctx, check.ID)
		if result.ID == "" {
			return toolOutcome{}, r.recordCheckBlocker(st, fmt.Errorf("check %s could not run: %w", check.ID, runErr))
		}
		passed := runErr == nil && result.ExitCode == 0 && !result.TimedOut && !result.Truncated
		failure := ""
		if runErr != nil {
			failure = runErr.Error()
		}
		if passed {
			if f := verifyTestEvidence(*check, result); f != "" {
				passed, failure = false, f
			}
		}
		return toolOutcome{Result: map[string]any{"check_id": check.ID, "passed": passed, "exit_code": result.ExitCode, "timed_out": result.TimedOut, "failure": failure, "tested_state": result.Before, "evidence": result.Evidence.ID, "output_tail": boundedTail(result.Output, maxFailureBytes)}}, nil

	case "commit":
		var args struct {
			Message string `json:"message"`
		}
		if err := workflow.DecodeStrict(string(step.Arguments), &args); err != nil {
			return toolError("commit arguments: %v", err), nil
		}
		session, _, err := backend.GitSession()
		if err != nil {
			return toolOutcome{}, err
		}
		if step.State == workflow.StepIntent {
			var intent struct {
				Parent string `json:"parent"`
			}
			_ = json.Unmarshal(step.Intent, &intent)
			for _, commit := range session.Commits {
				if commit.Parent == intent.Parent && intent.Parent != "" {
					return toolOutcome{Result: map[string]any{"commit": commit.SHA, "subject": commit.Subject, "paths": commit.Paths, "note": "committed before a restart"}}, nil
				}
			}
		}
		if err := validateCommitMessage(args.Message, c.Level0); err != nil {
			return toolError("%v", err), nil
		}
		current, err := backend.State()
		if err != nil {
			return toolOutcome{}, err
		}
		var missing []string
		for _, id := range task.Evidence {
			if state.passed[id] != current.Baseline() {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			return toolError("required checks have not passed against the current files: %s; run them first", strings.Join(missing, ", ")), nil
		}
		if err := mark(map[string]any{"parent": session.ExpectedSHA}); err != nil {
			return toolOutcome{}, err
		}
		record, err := backend.Commit(task.ID, args.Message)
		if err != nil {
			if isGuardFailure(err) {
				return toolOutcome{}, err
			}
			return toolError("commit refused: %v", err), nil
		}
		fmt.Fprintf(os.Stderr, "    committed %s %s\n", record.SHA[:12], record.Subject)
		return toolOutcome{Result: map[string]any{"commit": record.SHA, "subject": record.Subject, "paths": record.Paths}}, nil

	case "finish":
		var args finishArgs
		if err := workflow.DecodeStrict(string(step.Arguments), &args); err != nil {
			return toolError("finish arguments: %v", err), nil
		}
		if strings.TrimSpace(args.Explanation) == "" {
			return toolError("explanation is required"), nil
		}
		session, _, err := backend.GitSession()
		if err != nil {
			return toolOutcome{}, err
		}
		changed, err := backend.ChangedPaths()
		if err != nil {
			return toolOutcome{}, err
		}
		switch args.Result {
		case "completed":
			if len(session.Commits) == 0 {
				return toolError("completed requires at least one commit on the ticket branch"), nil
			}
			if len(changed) > 0 {
				return toolError("uncommitted changes remain: %s", strings.Join(changed, ", ")), nil
			}
			records, failure, err := r.finalChecks(ctx, st, c, backend)
			if err != nil {
				return toolOutcome{}, err
			}
			if failure != "" {
				return toolError("final checks against the committed state failed: %s", failure), nil
			}
			return toolOutcome{Result: map[string]any{"result": "completed", "final_checks": records}, Terminal: true}, nil
		case "no_change":
			if len(session.Commits) > 0 || len(changed) > 0 {
				return toolError("no_change requires no commits and no pending edits"), nil
			}
			return toolOutcome{Result: map[string]any{"result": "no_change"}, Terminal: true}, nil
		case "blocked":
			if len(args.Observations) == 0 {
				return toolError("blocked requires at least one observation for the human"), nil
			}
			for _, o := range args.Observations {
				if err := o.Validate(); err != nil {
					return toolError("%v", err), nil
				}
			}
			return toolOutcome{Result: map[string]any{"result": "blocked"}, Terminal: true}, nil
		}
		return toolError("result must be completed, no_change or blocked"), nil
	}
	return toolError("unknown tool %s", step.ToolName), nil
}

// replaceOnce replaces old with new in content, requiring exactly one match
// so an edit never lands somewhere the model did not intend.
func replaceOnce(content, old, new string) (string, error) {
	switch n := strings.Count(content, old); n {
	case 0:
		return "", errors.New("old text was not found; copy it exactly, including whitespace")
	case 1:
		return strings.Replace(content, old, new, 1), nil
	default:
		return "", fmt.Errorf("old text occurs %d times; include more surrounding lines so it matches exactly once", n)
	}
}

// Bounds on the files preloaded into a worker's first message.
const (
	maxPreloadFiles = 12
	maxPreloadBytes = 160 << 10
)

// preloadedFiles gives the worker, up front, its existing owned files and the
// files the parent read for the accepted proposal, each read from the
// approved checkout with its sha256, so it can edit without read turns. The
// section is pinned as an artifact when the run starts, so a resumed run
// keeps the first message its later steps were based on.
func (r *Runner) preloadedFiles(st *ws.State, task workflow.Ticket, backend *executor.Native, runKey string) (string, error) {
	store := r.Workspace.Store
	artifacts := workflow.ArtifactStore{Root: r.Workspace.Root}
	ref := workflow.ArtifactRef{ID: "worker-context-" + runKey, Path: fmt.Sprintf("cycles/%03d/agents/%s/preloaded.md", st.Cycle, runKey), Version: "1", Media: "text/markdown"}
	if _, err := store.GetArtifact(st.Cycle, ref.ID); err == nil {
		data, err := artifacts.Read(store, st.Cycle, ref.ID)
		return string(data), err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if steps, err := store.AgentSteps(st.Cycle, runKey); err != nil {
		return "", err
	} else if len(steps) > 0 {
		return "", nil // a run that started without a preload keeps its first message
	}
	parentReads, err := r.planningReadPaths(st.Cycle, "", true)
	if err != nil {
		return "", err
	}
	candidates := append(append([]string(nil), task.Outputs...), parentReads...)
	seen := map[string]bool{}
	var b strings.Builder
	files, size := 0, 0
	for _, path := range candidates {
		if seen[path] || files >= maxPreloadFiles || !backend.InAllowedPaths(path) {
			continue
		}
		seen[path] = true
		f, err := backend.Read(path)
		if err != nil {
			if isGuardFailure(err) {
				return "", err
			}
			continue
		}
		if f == nil || size+len(f.Data) > maxPreloadBytes {
			continue
		}
		if files == 0 {
			b.WriteString("\n# Files already loaded (current content and sha256; do not read them again)\n")
		}
		files++
		size += len(f.Data)
		fmt.Fprintf(&b, "\n## %s\nsha256: %s\n\n```\n%s\n```\n", path, fileSHA(f), f.Data)
	}
	if _, err := artifacts.Publish(store, st.Cycle, ref, []byte(b.String())); err != nil {
		return "", err
	}
	return b.String(), nil
}

// approvedReasoningCap is the reasoning cap the approved catalog declares for
// the worker's model, or 0 when it declares none.
func approvedReasoningCap(terms *workflow.LevelZeroTerms) int {
	for _, o := range terms.Worker.ModelOptions {
		if o.Model == terms.Worker.Model {
			return o.ReasoningMaxTokens
		}
	}
	return 0
}

// applyOwned writes one owned file through the executor, after recording the
// intent, within the task's repair-attempt allowance.
func (r *Runner) applyOwned(st *ws.State, task workflow.Ticket, state *workerState, backend *executor.Native, mark func(any) error, path string, current, next *executor.File) (toolOutcome, error) {
	phase := state.phases
	if phase == 0 || state.failedSince {
		phase++
	}
	if err := r.Workspace.Store.EnsureRepairAttempts(st.Cycle, task.ID, phase, task.MaxAttempts); err != nil {
		return toolOutcome{}, fmt.Errorf("%w; the branch and its work are preserved", err)
	}
	if err := mark(map[string]any{"path": path, "before": fileSHA(current), "after": fileSHA(next)}); err != nil {
		return toolOutcome{}, err
	}
	if _, err := backend.Apply(task.ID, []executor.Edit{{Path: path, Before: current, After: next}}); err != nil {
		if isGuardFailure(err) {
			return toolOutcome{}, err
		}
		return toolError("write refused: %v", err), nil
	}
	return toolOutcome{Result: map[string]any{"path": path, "sha256": fileSHA(next), "deleted": next == nil}}, nil
}

// isGuardFailure distinguishes "the checkout or approval is not what the
// engine recorded" (stop everything) from a request the model can correct.
func isGuardFailure(err error) bool {
	msg := err.Error()
	for _, s := range []string{"unexpected repository state", "unexpected branch", "no active approval", "observations require", "executor closed", "pending patch", "unfinished check", "changed after approval", "approval is stale"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

var commitSubject = regexp.MustCompile(`^([a-z]+)\(([a-z0-9-]+)\): (\S.*)$`)

// validateCommitMessage enforces the approved Conventional Commits shape and
// the required trailers; forged harness trailers are refused.
func validateCommitMessage(message string, terms *workflow.LevelZeroTerms) error {
	message = strings.TrimRight(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	if strings.ContainsRune(message, 0) || len(message) > 16<<10 {
		return errors.New("commit message must be text of at most 16 KiB")
	}
	lines := strings.Split(message, "\n")
	m := commitSubject.FindStringSubmatch(lines[0])
	if m == nil {
		return fmt.Errorf("first line must be %s(%s): <summary>", terms.TicketType, terms.CommitScope)
	}
	if m[1] != terms.TicketType {
		return fmt.Errorf("commit type must be the ticket type %q, not %q", terms.TicketType, m[1])
	}
	if m[2] != terms.CommitScope {
		return fmt.Errorf("commit scope must be the approved scope %q, not %q", terms.CommitScope, m[2])
	}
	if len(lines[0]) > 100 {
		return errors.New("the first line must be at most 100 characters")
	}
	if len(lines) > 1 && strings.TrimSpace(lines[1]) != "" {
		return errors.New("leave a blank line after the first line")
	}
	// The trailers are the final paragraph.
	start := len(lines)
	for start > 1 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}
	if start <= 1 {
		return errors.New("end the message with a blank line and the Yanai-Ticket and Yanai-Agent trailers")
	}
	trailers := map[string]int{}
	values := map[string]string{}
	for _, line := range lines[start:] {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || strings.ContainsAny(key, " \t") {
			return fmt.Errorf("the final paragraph must contain only trailers, found %q", line)
		}
		trailers[key]++
		values[key] = strings.TrimSpace(value)
	}
	for key := range trailers {
		if strings.HasPrefix(key, "Yanai-") && key != "Yanai-Ticket" && key != "Yanai-Agent" {
			return fmt.Errorf("trailer %s is reserved for the harness", key)
		}
	}
	for _, key := range []string{"Yanai-Ticket", "Yanai-Agent"} {
		if trailers[key] != 1 {
			return fmt.Errorf("exactly one %s trailer is required", key)
		}
	}
	if values["Yanai-Ticket"] != terms.Slug {
		return fmt.Errorf("Yanai-Ticket must be %q", terms.Slug)
	}
	if values["Yanai-Agent"] != terms.Worker.ID {
		return fmt.Errorf("Yanai-Agent must be %q", terms.Worker.ID)
	}
	return nil
}

// finalChecks runs every approved check against the final committed state.
func (r *Runner) finalChecks(ctx context.Context, st *ws.State, c workflow.ExecutionContract, backend *executor.Native) ([]workflow.CheckRecord, string, error) {
	final, err := backend.State()
	if err != nil {
		return nil, "", err
	}
	var records []workflow.CheckRecord
	for _, check := range c.Policy.Checks {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		fmt.Fprintf(os.Stderr, "    final check %s…\n", check.ID)
		result, runErr := backend.Check(ctx, check.ID)
		if result.ID == "" {
			return nil, "", r.recordCheckBlocker(st, fmt.Errorf("final check %s could not run: %w", check.ID, runErr))
		}
		record := workflow.CheckRecord{CheckID: check.ID, RunID: result.ID, Evidence: result.Evidence, ExitCode: result.ExitCode, Tested: result.Before, Passed: runErr == nil && result.ExitCode == 0 && !result.TimedOut && !result.Truncated}
		if runErr != nil {
			record.Failure = runErr.Error()
		}
		if record.Passed {
			if f := verifyTestEvidence(check, result); f != "" {
				record.Passed, record.Failure = false, f
			}
		}
		if record.Tested != final.Baseline() {
			record.Passed, record.Failure = false, "the check did not run against the final committed state"
		}
		records = append(records, record)
		if !record.Passed {
			return records, fmt.Sprintf("%s: %s\n%s", check.ID, record.Failure, boundedTail(result.Output, maxFailureBytes)), nil
		}
	}
	if len(records) == 0 {
		return nil, "", errors.New("the approved contract has no checks; an unchecked result cannot await review")
	}
	return records, "", nil
}

// verifyTestEvidence applies approved generic output rules and retains the Go
// adapter's test-result checks. Output matching relies on trusted check programs.
func verifyTestEvidence(c workflow.Check, result executor.CheckResult) string {
	if failure := workflow.CheckOutputFailure(c, result.Output); failure != "" {
		return failure
	}
	if len(c.Args) < 2 || c.Args[0] != "go" || c.Args[1] != "test" {
		return ""
	}
	if c.PostgresURLVar != "" && !result.DatabaseEnabled {
		return "test check ran without a disposable PostgreSQL URL; skipped database tests cannot prove acceptance"
	}
	packages, skips := 0, 0
	for _, line := range strings.Split(result.Output, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "ok "), strings.HasPrefix(trimmed, "--- PASS"), trimmed == "PASS":
			packages++
		case strings.HasPrefix(trimmed, "--- SKIP"), strings.HasPrefix(trimmed, "=== SKIP"):
			skips++
		case strings.Contains(trimmed, "DATABASE TESTS SKIPPED"):
			skips++
		case strings.HasPrefix(trimmed, "FAIL"), strings.HasPrefix(trimmed, "--- FAIL"):
			return "test output reports a failure despite a zero exit code"
		}
	}
	if skips > 0 {
		return fmt.Sprintf("%d skipped test(s) in the approved check; a skipped test is not a passing test", skips)
	}
	if packages == 0 {
		return "the approved test check executed no tests"
	}
	return ""
}

// boundedTail keeps the last n bytes of check output and says how much was
// dropped rather than silently shortening it.
func boundedTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	dropped := len(s) - n
	tail := s[dropped:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < 200 {
		tail = tail[i+1:]
	}
	return fmt.Sprintf("[%d earlier bytes of output omitted]\n%s", dropped, tail)
}
