package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/orchestrator"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// Amendment is a human correction to a proposal awaiting approval.
type Amendment struct {
	// DropOutputs removes expected files from the task.
	DropOutputs []string
	// Prompt, when set, replaces the worker prompt.
	Prompt string
	// Note records why; it is required.
	Note string
}

// Amend applies a human correction to the cycle's proposal without asking
// the parent again. The corrected proposal must pass the same validation
// against the same inputs the parent planned with; it is published as a new
// revision beside the earlier ones and needs a fresh review and approval.
func (r *Runner) Amend(a Amendment) (*ws.State, error) {
	if strings.TrimSpace(a.Note) == "" {
		return nil, errors.New("amend requires --note explaining the correction")
	}
	if len(a.DropOutputs) == 0 && a.Prompt == "" {
		return nil, errors.New("nothing to amend: pass --drop-output and/or --prompt")
	}
	store := r.Workspace.Store
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	o := st.Orchestration
	if st.Phase != ws.PhaseWaiting || o == nil || o.Proposal == "" || st.Plan == nil || len(st.Plan.Tickets) != 1 {
		return st, fmt.Errorf("cycle %03d is in phase %q: only a proposal awaiting approval can be amended", st.Cycle, st.Phase)
	}
	record, err := store.Proposal(st.Cycle, o.Proposal)
	if err != nil {
		return st, err
	}
	var p orchestrator.Proposal
	if err = json.Unmarshal(record.Payload, &p); err != nil {
		return st, fmt.Errorf("recorded proposal: %w", err)
	}

	// Apply the correction.
	var changes []string
	for _, drop := range a.DropOutputs {
		kept := p.Task.Outputs[:0:0]
		for _, out := range p.Task.Outputs {
			if out != drop {
				kept = append(kept, out)
			}
		}
		if len(kept) == len(p.Task.Outputs) {
			return st, fmt.Errorf("%s is not one of the proposal's expected files", drop)
		}
		p.Task.Outputs = kept
		changes = append(changes, "dropped expected file "+drop)
	}
	if a.Prompt != "" {
		if a.Prompt == p.Worker.Prompt {
			return st, errors.New("the new worker prompt is identical to the current one")
		}
		p.Worker.Prompt = a.Prompt
		changes = append(changes, "replaced the worker prompt")
	}

	// Rebuild the parent's planning inputs and require them unchanged.
	documents, err := r.Workspace.ProjectDocuments()
	if err != nil {
		return st, err
	}
	target, err := repository.Open(r.Cfg.Repo, r.Workspace.Root)
	if err != nil {
		return st, err
	}
	baseline, err := target.Snapshot()
	if err != nil {
		return st, err
	}
	if baseline.Baseline() != st.BaselineHash {
		return st, errors.New("the repository changed since the parent planned; reject this cycle and plan again")
	}
	live, err := os.ReadFile(filepath.Join(r.Workspace.Root, config.FileName))
	if err != nil {
		return st, err
	}
	original, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(store, st.Cycle, o.ConfigOriginal.ID)
	if err != nil {
		return st, err
	}
	if string(original) != string(live) {
		return st, fmt.Errorf("%s changed since the parent planned; reject this cycle and plan again", config.FileName)
	}
	var parent config.Orchestrator
	if err = json.Unmarshal(o.ParentSettings, &parent); err != nil {
		return st, err
	}
	pc, err := r.planningContext(st, documents, target, live, parent)
	if err != nil {
		return st, err
	}
	if pc.InputHash() != o.ProposalInput {
		return st, errors.New("the planning inputs changed since the parent's proposal; reject this cycle and plan again")
	}
	v, err := orchestrator.Validate(p, pc, r.Workspace.Root, func(repo config.Repo) (orchestrator.Checker, error) {
		return repository.Open(repo, r.Workspace.Root)
	})
	if err == nil {
		err = r.checkWorkerBudget(st.Cycle, v.Config.Execution)
	}
	if err != nil {
		return st, fmt.Errorf("the amended proposal is not valid:\n%w", err)
	}

	payload, _ := json.Marshal(p)
	id := "proposal-" + shortHash(workflow.Digest("amended\x00"+string(payload)))
	if err = store.SaveProposal(workflow.ProposalRecord{ID: id, Cycle: st.Cycle, InputHash: o.ProposalInput, Worker: p.Worker.ID, State: workflow.ProposalValid, Validation: "amended by the human from " + o.Proposal + ": " + a.Note, Payload: payload}); err != nil {
		return st, err
	}
	revision := st.Plan.Tickets[0].Revision + 1
	if revision < 2 {
		revision = 2
	}
	return r.recordProposal(st, p, v, pc, id, revision, fmt.Sprintf("the human amended the proposal (%s): %s", strings.Join(changes, "; "), a.Note), string(workflow.ActorHuman))
}
