package main

import (
	"encoding/json"
	"flag"
	"fmt"
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
	b, err := r.Workspace.Store.Budget(st.Cycle)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(b, "", "  ")
	fmt.Println(string(data))
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
	fmt.Println("Approval invalidated. Run discuss to prepare a new contract; spending is preserved.")
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
