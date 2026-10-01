package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/team"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

func cmdPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	path := wsPath(fs)
	retry := fs.Bool("retry-unresolved", false, "explicitly acknowledge interrupted model dispatch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: yanai plan [--ws PATH] <ticket.md>")
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	if _, err = workflow.ParseMarkdownTicket(string(raw)); err != nil {
		return err
	}
	r, cleanup, err := openWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := withContext()
	defer cancel()
	st, err := r.Plan(ctx, string(raw), *retry)
	if st != nil {
		fmt.Printf("Cycle %03d: %s\n", st.Cycle, st.Phase)
		printTasks(st)
	}
	if err == nil {
		fmt.Println("Next: yanai review, then yanai approve and yanai run")
	}
	return err
}

func cmdResolve(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	path := wsPath(fs)
	id := fs.String("observation", "", "observation ID")
	note := fs.String("note", "", "human response; revise ticket if requirements changed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("resolve requires --observation ID --note ...")
	}
	r, cleanup, err := bindWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	st, err := r.Workspace.LoadState()
	if err != nil {
		return err
	}
	if err = r.Workspace.Store.ResolveObservation(st.Cycle, *id, *note); err != nil {
		return err
	}
	fmt.Println("Human response recorded. Resume planning, or review and approve again before execution.")
	return nil
}

func cmdCommand(args []string) error {
	fs := flag.NewFlagSet("command", flag.ContinueOnError)
	path := wsPath(fs)
	id := fs.String("id", "", "command request ID (C-...)")
	approve := fs.Bool("approve", false, "run the command")
	deny := fs.Bool("deny", false, "refuse the command; --note tells the agent why")
	always := fs.Bool("always", false, "with --approve: also run this exact command (same arguments and directory) without asking for the rest of the cycle")
	note := fs.String("note", "", "a note for the agent (required with --deny)")
	run := fs.Bool("run", false, "after deciding a worker's request, continue the run at once (like 'yanai run')")
	show := fs.Bool("show", false, "with --id: show the request and, once it ran, its exit code and output")
	tail := fs.Int("tail", 0, "with --show: only the last N lines of output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, cleanup, err := bindWorkspace(*path)
	if err != nil {
		return err
	}
	released := false
	release := func() {
		if !released {
			released = true
			cleanup()
		}
	}
	defer release()
	st, err := r.Workspace.LoadState()
	if err != nil {
		return err
	}
	if *id == "" {
		if *approve || *deny {
			return fmt.Errorf("command --approve/--deny requires --id C-...")
		}
		all, err := r.Workspace.Store.Commands(st.Cycle)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			fmt.Printf("Cycle %03d has no command requests.\n", st.Cycle)
			return nil
		}
		for _, c := range all {
			printCommandRequest(c)
		}
		fmt.Println("\nOutput of a command that ran: yanai command --id C-... --show [--tail N]")
		return nil
	}
	if *show {
		return showCommand(r.Workspace, st.Cycle, *id, *tail)
	}
	if *approve == *deny {
		return fmt.Errorf("choose exactly one of --approve or --deny")
	}
	request, err := r.Workspace.Store.Command(st.Cycle, *id)
	if err != nil {
		return fmt.Errorf("command request %s: %w", *id, err)
	}
	if err = r.Workspace.Store.DecideCommand(st.Cycle, *id, *approve, *always, *note); err != nil {
		return err
	}
	if *run {
		if request.Role == "parent" {
			fmt.Println("Decision recorded. This request came from planning: continue with 'yanai plan <ticket.md>'.")
			return nil
		}
		fmt.Println("Decision recorded; continuing the run.")
		release() // the run takes the workspace lock itself
		return runCycle(*path, false)
	}
	if *approve {
		fmt.Println("Command approved. Continue with 'yanai run' (worker) or 'yanai plan <ticket.md>' (parent); the agent resumes at the same step.")
	} else {
		fmt.Println("Command denied. Continue with 'yanai run' or 'yanai plan <ticket.md>'; the agent reads your note.")
	}
	return nil
}

func printCommandRequest(c workflow.CommandRequest) {
	status := c.Status
	if c.Always {
		status += " (always)"
	}
	timeout := "timeout not recorded"
	if c.Timeout > 0 {
		timeout = fmt.Sprintf("timeout %ds", c.Timeout)
	}
	fmt.Printf("Command %s [%s] by %s in %s, %s\n  %s\n  Reason: %s\n", c.ID, status, c.Role, c.Dir, timeout, strings.Join(c.Args, " "), c.Reason)
	if c.Note != "" {
		fmt.Printf("  Note: %s\n", c.Note)
	}
}

// repeated collects a flag given several times.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func cmdAmend(args []string) error {
	fs := flag.NewFlagSet("amend", flag.ContinueOnError)
	path := wsPath(fs)
	var drop repeated
	fs.Var(&drop, "drop-output", "remove an expected file from the task (repeatable)")
	promptFile := fs.String("prompt", "", "file whose content replaces the worker prompt")
	note := fs.String("note", "", "why the proposal is amended")
	if err := fs.Parse(args); err != nil {
		return err
	}
	amendment := team.Amendment{DropOutputs: drop, Note: *note}
	if *promptFile != "" {
		data, err := os.ReadFile(*promptFile)
		if err != nil {
			return err
		}
		amendment.Prompt = string(data)
	}
	r, cleanup, err := bindWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	st, err := r.Amend(amendment)
	if err != nil {
		return err
	}
	fmt.Printf("Cycle %03d: proposal amended; it needs a new review and approval.\nNext: yanai review, then yanai approve --contract TOKEN\n", st.Cycle)
	return nil
}

// showCommand prints one request and, when it ran, its evidence: the worker's
// executor artifact or the parent's agent-step artifact.
func showCommand(w *ws.Workspace, cycle int, id string, tail int) error {
	c, err := w.Store.Command(cycle, id)
	if err != nil {
		return fmt.Errorf("command request %s: %w", id, err)
	}
	printCommandRequest(c)
	artifacts := workflow.ArtifactStore{Root: w.Root}
	evidence := "command-" + id
	if c.Role == "parent" {
		evidence = fmt.Sprintf("agent-%s-%03d-command", c.Run, c.Seq)
	}
	raw, err := artifacts.Read(w.Store, cycle, evidence)
	if err != nil {
		if c.Status == workflow.CommandApproved {
			fmt.Println("  Result: not recorded yet (the run has not reached it, or it was interrupted)")
		}
		return nil
	}
	var res struct {
		ExitCode   int       `json:"exit_code"`
		TimedOut   bool      `json:"timed_out"`
		Truncated  bool      `json:"truncated"`
		Error      string    `json:"error"`
		Changed    []string  `json:"changed"`
		Output     string    `json:"output"`
		StartedAt  time.Time `json:"started_at"`
		FinishedAt time.Time `json:"finished_at"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	fmt.Printf("  Result: exit %d in %s", res.ExitCode, res.FinishedAt.Sub(res.StartedAt).Round(100*time.Millisecond))
	if res.TimedOut {
		fmt.Print(", TIMED OUT")
	}
	if res.Truncated {
		fmt.Print(", output truncated")
	}
	fmt.Println()
	if res.Error != "" {
		fmt.Printf("  Error: %s\n", res.Error)
	}
	if len(res.Changed) > 0 {
		fmt.Printf("  Changed files: %s\n", strings.Join(res.Changed, ", "))
	}
	output := strings.TrimRight(res.Output, "\n")
	if tail > 0 {
		lines := strings.Split(output, "\n")
		if len(lines) > tail {
			output = strings.Join(lines[len(lines)-tail:], "\n")
		}
	}
	fmt.Printf("  Output:\n%s\n", output)
	return nil
}
