package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/orchestrator"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// maxCommandOutput bounds the command output an agent sees; the full output
// (up to the executor's limit) stays in the evidence artifact.
const maxCommandOutput = 20 << 10

// AwaitingCommand stops a run until a human decides a command request. The
// agent step stays answered but not done, so the next run replays it and
// finds the decision.
type AwaitingCommand struct {
	Request   workflow.CommandRequest
	Workspace string
	Resume    string
}

func (e *AwaitingCommand) Error() string {
	c := e.Request
	return fmt.Sprintf(`awaiting command approval %s
  agent:   %s
  dir:     %s
  command: %s
  reason:  %s
Approve: yanai command --ws %s --id %s --approve   (add --always to allow this exact command for the rest of the cycle)
Deny:    yanai command --ws %s --id %s --deny --note "why, and what to do instead"
Then continue with: %s`, c.ID, c.Role, c.Dir, quoteArgs(c.Args), c.Reason, e.Workspace, c.ID, e.Workspace, c.ID, e.Resume)
}

// quoteArgs renders a command for a human, quoting arguments with spaces.
func quoteArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$`\\|&;<>()*?") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

// commandGate records the step's command request and reports whether it may
// run now. It returns a tool outcome instead when the arguments are invalid,
// the human denied the command, or an earlier run was interrupted while the
// command ran; and an *AwaitingCommand error while the decision is pending.
func (r *Runner) commandGate(cycle int, run agentRun, step workflow.AgentStep, resume string) (orchestrator.CommandRequest, *toolOutcome, error) {
	req, err := orchestrator.ParseCommand(string(step.Arguments))
	if err != nil {
		outcome := toolError("%v", err)
		return req, &outcome, nil
	}
	if step.State == workflow.StepIntent {
		// The process died after the command started; its effect is unknown
		// and an approved command is never repeated behind the human's back.
		outcome := toolError("this command was started but the run was interrupted before it finished; its outcome is unknown. Inspect the repository with read_file/grep, or ask for the command again")
		return req, &outcome, nil
	}
	request, err := r.Workspace.Store.RequestCommand(cycle, run.Key, step.Seq, run.Role, req.Args, req.Dir, req.Reason)
	if err != nil {
		return req, nil, err
	}
	switch request.Status {
	case workflow.CommandApproved:
		return req, nil, nil
	case workflow.CommandDenied:
		outcome := toolError("the human denied this command: %s", request.Note)
		return req, &outcome, nil
	}
	return req, nil, &AwaitingCommand{Request: request, Workspace: r.Workspace.Root, Resume: resume}
}

// commandOutcome is what the agent sees of a finished command.
func commandOutcome(res executor.CommandResult) toolOutcome {
	output := res.Output
	if len(output) > maxCommandOutput {
		head, tail := output[:maxCommandOutput/5], output[len(output)-maxCommandOutput*4/5:]
		output = fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", head, len(res.Output)-len(head)-len(tail), tail)
	}
	result := map[string]any{"exit_code": res.ExitCode, "output": output}
	if res.TimedOut {
		result["timed_out"] = true
	}
	if res.Truncated {
		result["output_truncated"] = true
	}
	if len(res.Changed) > 0 {
		result["changed_files"] = res.Changed
	}
	if res.Error != "" && res.ExitCode == -1 {
		result["error"] = res.Error
	}
	return toolOutcome{Result: result, IsError: res.ExitCode != 0}
}

// parentCommand runs an approved planning command in the original checkout.
// Planning must not change the repository it plans against: a command that
// changes any file stops planning until the human restores it.
func (r *Runner) parentCommand(ctx context.Context, cycle int, run agentRun, target *repository.Target, step workflow.AgentStep, mark func(any) error, resume string) (toolOutcome, error) {
	req, outcome, err := r.commandGate(cycle, run, step, resume)
	if err != nil || outcome != nil {
		if outcome != nil {
			return *outcome, nil
		}
		return toolOutcome{}, err
	}
	id := workflow.CommandID(run.Key, step.Seq)
	before, err := target.Snapshot()
	if err != nil {
		return toolOutcome{}, err
	}
	if err := mark(map[string]any{"command": id}); err != nil {
		return toolOutcome{}, err
	}
	scratch, err := os.MkdirTemp(r.Workspace.Root, ".command-")
	if err != nil {
		return toolOutcome{}, err
	}
	defer os.RemoveAll(scratch)
	fmt.Fprintf(os.Stderr, "    running approved command %s: %s\n", id, quoteArgs(req.Args))
	res, err := executor.RunApproved(ctx, target, req.Args, req.Dir, time.Duration(req.TimeoutSeconds)*time.Second, scratch)
	res.ID = id
	if err != nil {
		return toolError("%v", err), nil
	}
	after, err := target.Snapshot()
	if err != nil {
		return toolOutcome{}, err
	}
	res.Changed = executor.ChangedPaths(before, after)
	evidence, _ := json.Marshal(res)
	if _, err := r.publish(r.artifactFor(run, step.Seq, "command"), evidence); err != nil {
		return toolOutcome{}, err
	}
	if after.Baseline() != before.Baseline() {
		return toolOutcome{}, fmt.Errorf("planning command %s changed the repository (%s); planning never modifies the checkout. Restore it, then run 'yanai plan' again", id, strings.Join(res.Changed, ", "))
	}
	return commandOutcome(res), nil
}
