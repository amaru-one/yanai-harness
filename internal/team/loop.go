package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/yanai/yanai-harness/internal/openrouter"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// MaxContextBytes bounds one model request's complete input. Exceeding it
// stops the run with a clear reason; nothing is silently summarized or cut.
const MaxContextBytes = 4 << 20

// probeThreshold is the share of a bucket's token budget after which the
// next request's prompt is counted exactly by a paid one-token probe.
const probeThreshold = 0.80

// maxForcedTurns bounds a run's final turns: the first, and one correction
// when the harness rejected its answer, so a fixable mistake (an extra field
// in a proposal) does not strand the cycle.
const maxForcedTurns = 2

// forcedNotice tells the model that only the run's terminal tool remains.
func forcedNotice(final string) string {
	text := fmt.Sprintf("HARNESS: the budget or step limit allows only one more turn. Call %s now with what you have; no other tool is available.", final)
	if final == "finish" {
		text += ` If there are no verified commits, use result "blocked" with one observation explaining what is missing.`
	}
	return text
}

func toolsNamed(tools []openrouter.Tool, name string) []openrouter.Tool {
	for _, t := range tools {
		if t.Function.Name == name {
			return []openrouter.Tool{t}
		}
	}
	return nil
}

// toolOutcome is what one tool call produced. Result is shown to the model;
// Terminal ends the run.
type toolOutcome struct {
	Result   map[string]any
	IsError  bool
	Terminal bool
}

func toolError(format string, args ...any) toolOutcome {
	return toolOutcome{Result: map[string]any{"error": fmt.Sprintf(format, args...)}, IsError: true}
}

// agentRun describes one saved tool loop: a planning run, a worker run or a
// closing run of one cycle.
type agentRun struct {
	Key         string
	Role        string
	Bucket      string
	Model       string
	Temperature float64
	MaxTokens   int
	MaxSteps    int
	Policy      workflow.ExecutionPolicy
	System      string
	Initial     string
	Tools       []openrouter.Tool
	// Final is the run's terminal tool. When the budget or the step limit
	// leaves room for only one more turn, it is the only tool offered.
	Final string
	Retry bool
	// Execute runs one validated tool call. A step replayed after a restart
	// arrives with State responded or intent; intent means a side effect may
	// already have happened and Execute must recognize it rather than repeat
	// it. mark records intent before a side effect. An error stops the run;
	// a problem the model can correct belongs in toolOutcome.IsError.
	Execute func(ctx context.Context, step workflow.AgentStep, mark func(intent any) error) (toolOutcome, error)
}

type loopResult struct {
	Step   workflow.AgentStep
	Result json.RawMessage
}

func (r *Runner) artifactFor(run agentRun, seq int, name string) workflow.ArtifactRef {
	return workflow.ArtifactRef{
		ID:      fmt.Sprintf("agent-%s-%03d-%s", run.Key, seq, name),
		Path:    fmt.Sprintf("cycles/%03d/agents/%s/%03d-%s.json", r.cycle, run.Key, seq, name),
		Version: "1",
		Media:   "application/json",
	}
}

func (r *Runner) readArtifact(id string) ([]byte, error) {
	return (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(r.Workspace.Store, r.cycle, id)
}

func (r *Runner) publish(ref workflow.ArtifactRef, data []byte) (workflow.ArtifactRef, error) {
	return (workflow.ArtifactStore{Root: r.Workspace.Root}).Publish(r.Workspace.Store, r.cycle, ref, data)
}

// callID gives every tool call a stable identity that is unique within the
// run, even when a provider reuses or omits IDs.
func callID(id string, seq int, used map[string]bool) string {
	if id == "" || used[id] {
		id = fmt.Sprintf("%s#%d", strings.TrimSpace(id), seq)
	}
	used[id] = true
	return id
}

// resultMessages renders a recorded tool result back into the transcript.
func resultMessages(assistant openrouter.Message, content string) []openrouter.Message {
	if len(assistant.ToolCalls) == 0 {
		return []openrouter.Message{{Role: "user", Content: content}}
	}
	var out []openrouter.Message
	for _, c := range assistant.ToolCalls {
		out = append(out, openrouter.Message{Role: "tool", ToolCallID: c.ID, Content: content})
	}
	return out
}

func encodeOutcome(o toolOutcome) []byte {
	result := map[string]any{}
	for k, v := range o.Result {
		result[k] = v
	}
	result["ok"] = !o.IsError
	raw, _ := json.Marshal(result)
	return raw
}

// runLoop drives a saved tool loop to its terminal tool call. It rebuilds
// the transcript from recorded steps, finishes a step a dead process left
// between answer and result, and only then asks the model for another turn.
func (r *Runner) runLoop(ctx context.Context, run agentRun) (loopResult, error) {
	store := r.Workspace.Store
	steps, err := store.AgentSteps(r.cycle, run.Key)
	if err != nil {
		return loopResult{}, err
	}
	known := map[string]bool{}
	for _, t := range run.Tools {
		known[t.Function.Name] = true
	}
	used := map[string]bool{}
	messages := []openrouter.Message{{Role: "system", Content: r.redact(run.System)}, {Role: "user", Content: r.redact(run.Initial)}}

	finish := func(step workflow.AgentStep, assistant openrouter.Message) (loopResult, bool, error) {
		outcome := toolOutcome{}
		var err error
		switch {
		case step.Forced && (len(assistant.ToolCalls) != 1 || step.ToolName != run.Final):
			outcome = toolError("only %s was available in this final turn", run.Final)
		case len(assistant.ToolCalls) == 0 && assistant.Content == openrouter.TruncatedAnswer:
			outcome = toolError("your previous answer reached the %d-token output limit (reasoning included) before a complete tool call; think briefly and act in smaller steps, for example one file per write", run.MaxTokens)
		case len(assistant.ToolCalls) == 0:
			outcome = toolError("answer with exactly one tool call; plain text is not an action")
		case len(assistant.ToolCalls) > 1:
			outcome = toolError("exactly one tool call per turn is allowed; none of the %d calls was executed", len(assistant.ToolCalls))
		case !known[step.ToolName]:
			outcome = toolError("unknown tool %q", step.ToolName)
		case !json.Valid(step.Arguments) || !strings.HasPrefix(strings.TrimSpace(string(step.Arguments)), "{"):
			outcome = toolError("tool arguments must be one JSON object")
		default:
			mark := func(intent any) error {
				step.State, step.Intent = workflow.StepIntent, mustJSON(intent)
				return store.SaveAgentStep(r.cycle, step)
			}
			outcome, err = run.Execute(ctx, step, mark)
			if err != nil {
				return loopResult{}, false, err
			}
		}
		raw := encodeOutcome(outcome)
		ref, err := r.publish(r.artifactFor(run, step.Seq, "result"), raw)
		if err != nil {
			return loopResult{}, false, err
		}
		step.Result, step.IsError, step.Terminal = &ref, outcome.IsError, outcome.Terminal
		step.State = workflow.StepDone
		if err = store.SaveAgentStep(r.cycle, step); err != nil {
			return loopResult{}, false, err
		}
		messages = append(messages, resultMessages(assistant, r.redact(string(raw)))...)
		return loopResult{Step: step, Result: raw}, outcome.Terminal, nil
	}

	// forcedTurns counts final turns taken; once maxForcedTurns were rejected
	// the run stops rather than spend more on corrections.
	forcedTurns := 0
	stopForced := func(raw []byte) error {
		if forcedTurns < maxForcedTurns {
			return nil
		}
		return fmt.Errorf("%s's %d final turns (only %s was offered) did not end the run; last result: %s; the recorded work is preserved", run.Role, forcedTurns, run.Final, string(raw))
	}

	// Replay what is already durable.
	next := 1
	var last *workflow.AgentStep
	// pending is a step whose request was recorded but never answered; a
	// retry of it must send the same request, so it keeps its decision.
	var pending *workflow.AgentStep
	for i, step := range steps {
		if step.Response == nil {
			if i != len(steps)-1 {
				return loopResult{}, fmt.Errorf("agent run %s has a gap at step %d", run.Key, step.Seq)
			}
			unanswered := step
			pending = &unanswered
			break
		}
		data, err := r.readArtifact(step.Response.ID)
		if err != nil {
			return loopResult{}, err
		}
		var assistant openrouter.Message
		if err = json.Unmarshal(data, &assistant); err != nil {
			return loopResult{}, err
		}
		for j := range assistant.ToolCalls {
			assistant.ToolCalls[j].ID = callID(assistant.ToolCalls[j].ID, step.Seq, used)
		}
		if step.Forced {
			forcedTurns++
			messages = append(messages, openrouter.Message{Role: "user", Content: forcedNotice(run.Final)})
		}
		messages = append(messages, assistant)
		next = step.Seq + 1
		recorded := step
		last = &recorded
		if step.State == workflow.StepDone {
			raw, err := r.readArtifact(step.Result.ID)
			if err != nil {
				return loopResult{}, err
			}
			messages = append(messages, resultMessages(assistant, r.redact(string(raw)))...)
			if step.Terminal {
				return loopResult{Step: step, Result: raw}, nil
			}
			if step.Forced {
				if err := stopForced(raw); err != nil {
					return loopResult{}, err
				}
			}
			continue
		}
		if i != len(steps)-1 {
			return loopResult{}, fmt.Errorf("agent run %s step %d is unfinished but later steps exist", run.Key, step.Seq)
		}
		fmt.Fprintf(os.Stderr, "  resuming %s step %d (%s) from its recorded answer\n", run.Role, step.Seq, step.ToolName)
		result, terminal, err := finish(step, assistant)
		if err != nil || terminal {
			return result, err
		}
		if step.Forced {
			if err := stopForced(result.Result); err != nil {
				return loopResult{}, err
			}
		}
		recorded.IsError = result.Step.IsError
	}

	for seq := next; ; seq++ {
		// A rejected final turn at the step limit may take its one correction.
		correcting := last != nil && last.Forced && last.IsError && forcedTurns < maxForcedTurns
		if seq > run.MaxSteps && !correcting {
			return loopResult{}, fmt.Errorf("%s reached its limit of %d steps without finishing; the recorded work is preserved", run.Role, run.MaxSteps)
		}
		if err := ctx.Err(); err != nil {
			return loopResult{}, err
		}
		if r.guard != nil {
			if err := r.guard(); err != nil {
				return loopResult{}, err
			}
		}
		estimate, err := r.turnEstimate(run, messages, last)
		if err != nil {
			return loopResult{}, err
		}
		b, err := store.BucketBudget(r.cycle, run.Bucket)
		if err != nil {
			return loopResult{}, err
		}
		if b.Policy.MaxTokens > 0 && float64(b.Tokens) > probeThreshold*float64(b.Policy.MaxTokens) {
			r.inputEstimate = estimate
			usage, err := r.Client.ProbePromptTokens(ctx, run.Model, messages, run.Tools, "", run.Temperature, r.observer(ctx, run.Role, run.Model, "token_probe", 1, run.Bucket, run.Policy))
			r.inputEstimate = 0
			if err != nil {
				return loopResult{}, fmt.Errorf("token probe: %w", err)
			}
			if usage.PromptTokens > 0 {
				estimate = int64(usage.PromptTokens) + 1024
			}
			if b, err = store.BucketBudget(r.cycle, run.Bucket); err != nil {
				return loopResult{}, err
			}
		}
		// A turn is the final one when this turn and one more of the same size
		// would not both fit, so the run always keeps room to finish.
		perTurn := estimate + int64(run.MaxTokens)
		perTurnCost, err := run.Policy.ReserveCost(run.Model, estimate, int64(run.MaxTokens))
		if err != nil {
			return loopResult{}, err
		}
		forced := run.Final != "" && (correcting || seq >= run.MaxSteps || b.RemainingTokens < 2*perTurn || b.RemainingCostUSD < 2*perTurnCost || b.RemainingCalls < 2)
		if pending != nil && pending.Seq == seq {
			forced = pending.Forced
		}
		tools, force := run.Tools, ""
		if forced {
			messages = append(messages, openrouter.Message{Role: "user", Content: forcedNotice(run.Final)})
			tools, force = toolsNamed(run.Tools, run.Final), run.Final
			fmt.Fprintf(os.Stderr, "  %s step %d: budget or step limit reached; only %s is offered\n", run.Role, seq, run.Final)
		}
		body, _ := json.Marshal(messages)
		if len(body) > MaxContextBytes {
			return loopResult{}, fmt.Errorf("%s's context is %d bytes, over the %d-byte limit; automatic summarization is not part of level 0, so the run stops here with its work preserved", run.Role, len(body), MaxContextBytes)
		}
		requestRef, err := r.publish(r.artifactFor(run, seq, "request"), mustJSON(map[string]any{"model": run.Model, "messages": len(messages), "bytes": len(body), "sha256": workflow.Digest(string(body)), "tools": toolNames(tools)}))
		if err != nil {
			return loopResult{}, err
		}
		step := workflow.AgentStep{Run: run.Key, Seq: seq, Role: run.Role, State: workflow.StepRequested, Request: &requestRef, Forced: forced}
		if err = store.SaveAgentStep(r.cycle, step); err != nil {
			return loopResult{}, err
		}
		r.inputEstimate = estimate
		assistant, usage, responseRef, err := r.agentCall(ctx, run, seq, messages, tools, force)
		r.inputEstimate = 0
		if err != nil {
			return loopResult{}, err
		}
		step.PromptTokens, step.CompletionTokens = usage.PromptTokens, usage.CompletionTokens
		for j := range assistant.ToolCalls {
			assistant.ToolCalls[j].ID = callID(assistant.ToolCalls[j].ID, seq, used)
		}
		step.Response, step.State = &responseRef, workflow.StepResponded
		if len(assistant.ToolCalls) == 1 {
			step.ToolCallID = assistant.ToolCalls[0].ID
			step.ToolName = assistant.ToolCalls[0].Function.Name
			step.Arguments = json.RawMessage(strings.TrimSpace(assistant.ToolCalls[0].Function.Arguments))
			if !json.Valid(step.Arguments) {
				step.Arguments = mustJSON(assistant.ToolCalls[0].Function.Arguments)
			}
		}
		if err = store.SaveAgentStep(r.cycle, step); err != nil {
			return loopResult{}, err
		}
		messages = append(messages, assistant)
		fmt.Fprintf(os.Stderr, "  %s step %d: %s\n", run.Role, seq, describeCall(assistant))
		result, terminal, err := finish(step, assistant)
		if err != nil || terminal {
			return result, err
		}
		recorded := result.Step
		last = &recorded
		if forced {
			forcedTurns++
			if err := stopForced(result.Result); err != nil {
				return loopResult{}, err
			}
			fmt.Fprintf(os.Stderr, "  %s step %d: the final answer was rejected; one correction turn remains\n", run.Role, seq)
		}
	}
}

// turnEstimate bounds the next request's input tokens: the provider's native
// count for the previous turn (its prompt and its answer) plus a byte bound
// on every message added since. The first turn has no native count yet, so
// the whole request is bounded by its bytes.
func (r *Runner) turnEstimate(run agentRun, messages []openrouter.Message, last *workflow.AgentStep) (int64, error) {
	answer := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			answer = i
			break
		}
	}
	if last != nil && answer >= 0 {
		prompt, completion := last.PromptTokens, last.CompletionTokens
		if prompt == 0 {
			// Steps recorded before native counts were kept.
			p, c, ok, err := r.Workspace.Store.LastTurnUsage(r.cycle, run.Bucket, run.Role)
			if err != nil {
				return 0, err
			}
			if ok {
				prompt, completion = p, c
			}
		}
		if prompt > 0 {
			added, _ := json.Marshal(messages[answer+1:])
			return int64(prompt+completion) + int64(len(added)) + 1024, nil
		}
	}
	body, _ := json.Marshal(struct {
		Messages []openrouter.Message
		Tools    []openrouter.Tool
	}{messages, run.Tools})
	return int64(len(body)) + 1024, nil
}

// agentCall makes (or reuses) the model call for one step. The response is
// published by the accounting hook before this returns, so a crash after
// the provider answered never pays for the same turn twice.
func (r *Runner) agentCall(ctx context.Context, run agentRun, seq int, messages []openrouter.Message, tools []openrouter.Tool, force string) (openrouter.Message, openrouter.Usage, workflow.ArtifactRef, error) {
	store := r.Workspace.Store
	ref := r.artifactFor(run, seq, "response")
	if rec, err := store.GetArtifact(r.cycle, ref.ID); err == nil {
		data, err := r.readArtifact(ref.ID)
		if err != nil {
			return openrouter.Message{}, openrouter.Usage{}, ref, err
		}
		ref.SHA256 = rec.SHA256
		var m openrouter.Message
		return m, openrouter.Usage{}, ref, json.Unmarshal(data, &m)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return openrouter.Message{}, openrouter.Usage{}, ref, err
	}
	intent := fmt.Sprintf("agent-call/%s/%d/%s/%d", store.Project(), r.cycle, run.Key, seq)
	if _, found, err := store.CheckCommand(intent); err != nil {
		return openrouter.Message{}, openrouter.Usage{}, ref, err
	} else if found && !run.Retry {
		return openrouter.Message{}, openrouter.Usage{}, ref, errors.New("a model call was interrupted before its answer was recorded; reconcile attempts (yanai status --attempts) and rerun with --retry-unresolved")
	} else if !found {
		if err = store.RecordCommand(intent, "agent-call", "started"); err != nil {
			return openrouter.Message{}, openrouter.Usage{}, ref, err
		}
	}
	r.captureResponse = func(raw string) error {
		published, err := r.publish(ref, []byte(raw))
		ref = published
		return err
	}
	defer func() { r.captureResponse = nil }()
	fmt.Fprintf(os.Stderr, "→ %s (%s) thinking…\n", run.Role, run.Model)
	m, usage, err := r.Client.ChatTools(ctx, run.Model, messages, tools, force, run.Temperature, run.MaxTokens, r.observer(ctx, run.Role, run.Model, "agent_turn", run.MaxTokens, run.Bucket, run.Policy))
	return m, usage, ref, err
}

func describeCall(m openrouter.Message) string {
	if len(m.ToolCalls) != 1 {
		return fmt.Sprintf("%d tool calls (invalid)", len(m.ToolCalls))
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(m.ToolCalls[0].Function.Arguments), &args)
	if paths, ok := args["paths"].([]any); ok {
		names := make([]string, 0, len(paths))
		for _, p := range paths {
			if s, ok := p.(string); ok {
				names = append(names, s)
			}
		}
		return fmt.Sprintf("%s %s", m.ToolCalls[0].Function.Name, strings.Join(names, ", "))
	}
	for _, key := range []string{"path", "check_id", "result"} {
		if v, ok := args[key].(string); ok {
			return fmt.Sprintf("%s %s", m.ToolCalls[0].Function.Name, v)
		}
	}
	return m.ToolCalls[0].Function.Name
}

func toolNames(tools []openrouter.Tool) []string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Function.Name)
	}
	return out
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
