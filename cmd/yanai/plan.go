package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/yanai/yanai-harness/internal/workflow"
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
