package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// MaxCommandSeconds bounds one agent command.
const MaxCommandSeconds = 1800

// Process is the outcome of one program run.
type Process struct {
	ExitCode  int
	Output    string
	Truncated bool
	TimedOut  bool
	Err       error
}

// runProcess runs binary in dir without a shell, with stdout and stderr
// combined and bounded, and kills its whole process group when it ends or
// times out.
func runProcess(ctx context.Context, binary string, args []string, dir string, env []string, timeout time.Duration) Process {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	p := Process{ExitCode: -1}
	p.Err = cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if cmd.ProcessState != nil {
		p.ExitCode = cmd.ProcessState.ExitCode()
	}
	p.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	p.Truncated = output.truncated
	p.Output = string(output.data)
	return p
}

// CommandResult is the evidence of one human-approved agent command.
type CommandResult struct {
	ID         string    `json:"id"`
	Args       []string  `json:"args"`
	Dir        string    `json:"dir"`
	Binary     string    `json:"binary"`
	ExitCode   int       `json:"exit_code"`
	Output     string    `json:"output"`
	Truncated  bool      `json:"truncated,omitempty"`
	TimedOut   bool      `json:"timed_out,omitempty"`
	Error      string    `json:"error,omitempty"`
	Changed    []string  `json:"changed,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// RunApproved runs a command a human approved, in dir inside the target
// checkout, with the operator's environment (see hostEnvironment). The
// program is a name found on PATH or a relative path to a repository
// program. scratch is an empty directory for TMPDIR. It does not look at
// what the command changed; callers compare repository snapshots.
func RunApproved(ctx context.Context, target *repository.Target, args []string, dir string, timeout time.Duration, scratch string) (CommandResult, error) {
	result := CommandResult{Args: args, Dir: dir, ExitCode: -1, StartedAt: time.Now().UTC()}
	if len(args) == 0 || args[0] == "" {
		return result, errors.New("the command is empty")
	}
	if err := target.CheckDirectory(dir); err != nil {
		return result, fmt.Errorf("command directory: %w", err)
	}
	if timeout <= 0 || timeout > MaxCommandSeconds*time.Second {
		return result, fmt.Errorf("command timeout must be between 1 and %d seconds", MaxCommandSeconds)
	}
	var err error
	if workflow.RepositoryProgram(args[0]) {
		if err = workflow.ValidateCheckArguments(args); err == nil {
			result.Binary, err = resolveRepositoryProgram(target.Root, dir, args[0])
		}
	} else {
		result.Binary, err = exec.LookPath(args[0])
	}
	if err != nil {
		return result, fmt.Errorf("program %s: %w", args[0], err)
	}
	p := runProcess(ctx, result.Binary, args[1:], filepath.Join(target.Root, filepath.FromSlash(dir)), hostEnvironment(scratch, nil), timeout)
	result.FinishedAt = time.Now().UTC()
	result.ExitCode, result.Output, result.Truncated, result.TimedOut = p.ExitCode, p.Output, p.Truncated, p.TimedOut
	if p.Err != nil {
		result.Error = p.Err.Error()
	}
	return result, nil
}

// ChangedPaths lists the repository files whose content differs between two
// snapshots.
func ChangedPaths(before, after workflow.RepositoryState) []string {
	var out []string
	for path, fingerprint := range after.Content {
		if before.Content[path] != fingerprint {
			out = append(out, path)
		}
	}
	for path := range before.Content {
		if _, ok := after.Content[path]; !ok {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// RunCommand runs a human-approved worker command on the ticket branch.
// Files it changes become part of the worker's recorded state, so the
// worker can commit them; a command that moves HEAD, changes the index or
// switches branch stops the run for a human to reconcile.
func (n *Native) RunCommand(ctx context.Context, id string, args []string, dir string, timeout time.Duration) (CommandResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.unfinishedChecks(); err != nil {
		return CommandResult{}, err
	}
	before, err := n.guard()
	if err != nil {
		return CommandResult{}, err
	}
	scratch, err := os.MkdirTemp(n.o.Artifacts.Root, ".command-")
	if err != nil {
		return CommandResult{}, err
	}
	defer os.RemoveAll(scratch)
	result, err := RunApproved(ctx, n.target, args, dir, timeout, scratch)
	result.ID = id
	if err != nil {
		return result, err
	}
	for _, value := range n.o.CheckInputs {
		if value != "" {
			result.Output = strings.ReplaceAll(result.Output, value, "[check input]")
		}
	}
	after, err := n.target.Snapshot()
	if err != nil {
		return result, err
	}
	if n.branch != "" {
		branch, err := n.target.Branch()
		if err != nil {
			return result, err
		}
		if branch != n.branch {
			return result, fmt.Errorf("unexpected branch: command %s moved the checkout to %q; nothing was reverted, reconcile manually", id, branch)
		}
	}
	if after.Baseline() != before.Baseline() {
		result.Changed = ChangedPaths(before, after)
		for _, p := range result.Changed {
			if !n.contract.Plan.Tickets[0].Allows(p) {
				return result, fmt.Errorf("unexpected repository state: command %s changed %s, outside the approved ticket; nothing was reverted, reconcile manually", id, p)
			}
		}
		if err := n.o.Store.AdoptCommandState(n.o.Cycle, n.o.Contract, before, after, id); err != nil {
			return result, fmt.Errorf("unexpected repository state: %w", err)
		}
	}
	_, err = n.publish("command-"+id, result)
	return result, err
}
