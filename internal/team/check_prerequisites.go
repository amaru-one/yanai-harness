package team

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

func (r *Runner) approvedCheckInputs(st *ws.State) (map[string]string, error) {
	if st.Markdown == nil {
		return nil, errors.New("cycle has no Markdown ticket")
	}
	raw, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(r.Workspace.Store, st.Cycle, st.Markdown.Source.ID)
	if err != nil {
		return nil, err
	}
	parsed, err := workflow.ParseMarkdownTicket(string(raw))
	if err != nil {
		return nil, err
	}
	if parsed.Revision != st.Markdown.Revision || !reflect.DeepEqual(parsed.CheckInputNames, st.Markdown.CheckInputNames) {
		return nil, errors.New("check input source differs from approved ticket")
	}
	return workflow.ParseCheckInputs(string(raw))
}

func (r *Runner) recordCheckBlocker(st *ws.State, issue error) error {
	if issue == nil {
		return nil
	}
	detail := workflow.ObservationDetail{
		Description: issue.Error(),
		Requirement: "approved check prerequisites",
		Question:    "Provide the named input or tool, or revise the ticket/check configuration, then review again.",
	}
	if err := r.Workspace.Store.RecordObservations(st.Cycle, "engine", "check-prerequisite-"+workflow.Digest(issue.Error()), []workflow.ObservationDetail{detail}); err != nil {
		return err
	}
	return fmt.Errorf("check prerequisites are not ready: %w", issue)
}

func (r *Runner) preflightChecks(st *ws.State, inputs map[string]string) error {
	if err := executor.Preflight(r.Cfg.Repo, r.Workspace.Root, r.Cfg.Execution, inputs); err != nil {
		return r.recordCheckBlocker(st, err)
	}
	return nil
}
