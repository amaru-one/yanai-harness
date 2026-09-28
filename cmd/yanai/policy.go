package main

import (
	"encoding/json"
	"flag"
	"fmt"

	"github.com/yanai/yanai-harness/internal/workflow"
)

func cmdReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	path := wsPath(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, cleanup, err := bindWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	review, err := r.Review()
	if err != nil {
		return err
	}
	fmt.Print(review)
	return nil
}
func cmdPolicy(args []string) error {
	fs := flag.NewFlagSet("policy", flag.ContinueOnError)
	path := wsPath(fs)
	note := fs.String("note", "", "reason for adopting the configured execution policy")
	if err := fs.Parse(args); err != nil {
		return err
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
	if *note != "" && st.Orchestration != nil {
		return fmt.Errorf("level 0 budgets are part of the approved configuration; to change them, invalidate the cycle and plan again")
	}
	if *note != "" {
		if err = r.Workspace.Store.RevisePolicy(st.Cycle, r.Cfg.Execution, *note); err != nil {
			return err
		}
		st, err = r.Workspace.LoadState()
		if err != nil {
			return err
		}
		if err = r.Workspace.RefreshProjection(st); err != nil {
			return err
		}
	}
	for _, bucket := range []string{workflow.BucketParent, workflow.BucketWorker} {
		b, err := r.Workspace.Store.BucketBudget(st.Cycle, bucket)
		if err != nil {
			fmt.Printf("%s: no budget recorded yet\n", bucket)
			continue
		}
		data, _ := json.MarshalIndent(b, "", "  ")
		fmt.Printf("%s:\n%s\n", bucket, data)
	}
	return nil
}
func cmdInvalidate(args []string) error {
	fs := flag.NewFlagSet("invalidate", flag.ContinueOnError)
	path := wsPath(fs)
	note := fs.String("note", "", "reason for invalidating the plan")
	if err := fs.Parse(args); err != nil {
		return err
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
	if err = r.Workspace.Store.Invalidate(st.Cycle, st.StateVersion, *note); err != nil {
		return err
	}
	st, err = r.Workspace.LoadState()
	if err != nil {
		return err
	}
	fmt.Println("Approval invalidated. Use plan <ticket.md> to prepare a new cycle; prior spending and evidence remain recorded.")
	return r.Workspace.RefreshProjection(st)
}
func cmdReconcileAttempt(args []string) error {
	fs := flag.NewFlagSet("reconcile-attempt", flag.ContinueOnError)
	path := wsPath(fs)
	id := fs.String("id", "", "attempt ID")
	cost := fs.Float64("cost-usd", -1, "actual billed USD, including an explicitly confirmed zero")
	tokens := fs.Int64("tokens", -1, "actual total tokens")
	reference := fs.String("reference", "", "billing source/reference")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, cleanup, err := bindWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	if err = r.Workspace.Store.ReconcileBilling(*id, *cost, *tokens, *reference); err != nil {
		return err
	}
	fmt.Println("Billing recorded. An uncertain call still requires --retry-unresolved before retry.")
	return nil
}
