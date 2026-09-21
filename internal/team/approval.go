package team

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

func (r *Runner) executionContract(st *ws.State) (workflow.ExecutionContract, error) {
	var c workflow.ExecutionContract
	if err := r.validateCurrentPlan(st); err != nil {
		return c, err
	}
	if r.Workspace.Store == nil {
		return c, fmt.Errorf("approval requires SQLite authority")
	}
	if err := r.Workspace.Store.EnsureBudget(st.Cycle, r.Cfg.Execution); err != nil {
		return c, err
	}
	if len(r.Cfg.Execution.Checks) == 0 {
		return c, fmt.Errorf("configure execution.checks: at least one required project check must be reviewed")
	}
	target, err := repository.Open(r.Cfg.Repo, r.Workspace.Root)
	if err != nil {
		return c, err
	}
	snapshot, err := target.Snapshot()
	if err != nil {
		return c, err
	}
	if snapshot.Baseline() != st.BaselineHash {
		if st.Approval == nil || st.Approval.ContractHash == "" {
			return c, fmt.Errorf("approval is stale: repository baseline changed; invalidate and discuss again")
		}
		expected, _, err := r.Workspace.Store.PatchState(st.Cycle, st.Approval.ContractHash)
		if err != nil || expected.Baseline() != snapshot.Baseline() {
			return c, fmt.Errorf("approval is stale: unexpected external repository edits")
		}
		original, err := r.Workspace.Store.Contract(st.Cycle, st.Approval.ContractHash)
		if err != nil {
			return c, err
		}
		snapshot = repository.Snapshot{}
		if err = json.Unmarshal(original.Repository, &snapshot); err != nil {
			return c, err
		}
	}
	inputs := map[string]string{}
	for role, a := range r.Cfg.Agents {
		if _, err := r.Cfg.Execution.ReserveCost(a.Model, 1, int64(a.MaxTokens)); err != nil {
			return c, err
		}
		safe, err := workflow.SafeRelativePath(r.Workspace.Root, filepath.ToSlash(a.Prompt))
		if err != nil {
			return c, err
		}
		b, err := os.ReadFile(filepath.Join(r.Workspace.Root, safe))
		if err != nil {
			return c, err
		}
		inputs["prompt:"+role] = workflow.Digest(string(b))
	}
	discussion := r.Workspace.ReadDocument(st.Cycle, "03-discusion.md")
	if discussion == "" {
		return c, fmt.Errorf("missing discussion input")
	}
	inputs["discussion"] = workflow.Digest(discussion)
	inputs["source"] = st.Intake.Revision
	inputs["source_provenance"], err = workflow.Hash(st.Intake)
	if err != nil {
		return c, err
	}
	// Ticket rows, not the JSON projection, are canonical executable contracts.
	for _, ticket := range st.Plan.Tickets {
		rec, err := r.Workspace.Store.GetTicket(st.Cycle, ticket.ID)
		if err != nil {
			return c, err
		}
		var stored workflow.Ticket
		if err = json.Unmarshal([]byte(rec.Payload), &stored); err != nil {
			return c, err
		}
		if !reflect.DeepEqual(stored, ticket) {
			return c, fmt.Errorf("stored ticket %s differs from reviewed plan", ticket.ID)
		}
	}
	settings, _ := json.Marshal(struct {
		Agents   map[string]config.Agent
		Repo     config.Repo
		Provider config.OpenRouter
		Order    []string
	}{r.Cfg.Agents, r.Cfg.Repo, r.Cfg.OpenRouter, r.Cfg.DiscussionOrder})
	repo, _ := json.Marshal(snapshot)
	c = workflow.ExecutionContract{Version: 1, Plan: *st.Plan, ContextHash: st.ScopeHash, Baseline: snapshot.Baseline(), Repository: repo, Inputs: inputs, Settings: settings, Policy: r.Cfg.Execution}
	return c, nil
}
func (r *Runner) Review() (string, error) {
	st, err := r.Workspace.LoadState()
	if err != nil {
		return "", err
	}
	c, err := r.executionContract(st)
	if err != nil {
		return "", err
	}
	hash, err := r.Workspace.Store.SaveContract(st.Cycle, c)
	if err != nil {
		return "", err
	}
	b, err := r.Workspace.Store.Budget(st.Cycle)
	if err != nil {
		return "", err
	}
	raw, _ := json.MarshalIndent(c, "", "  ")
	budget, _ := json.MarshalIndent(b, "", "  ")
	return fmt.Sprintf("Contract: %s\n%s\n\nBudget (cycle-wide; all checks apply to every ticket):\n%s\n", hash, raw, budget), nil
}
func (r *Runner) Approve(note, reviewedHash string) (*ws.State, error) {
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	c, err := r.executionContract(st)
	if err != nil {
		return st, err
	}
	hash, err := r.Workspace.Store.SaveContract(st.Cycle, c)
	if err != nil {
		return st, err
	}
	if reviewedHash == "" || reviewedHash != hash {
		return st, fmt.Errorf("reviewed contract changed before approval; review again")
	}
	if st.Phase == ws.PhaseApproved || st.Phase == ws.PhaseAwaitingExecution {
		return st, r.Workspace.Store.HasApproval(st.Cycle, hash)
	}
	if st.Phase != ws.PhaseWaiting {
		return st, fmt.Errorf("only awaiting_approval plans can be approved")
	}
	artifact := workflow.ArtifactStore{Root: r.Workspace.Root}
	discussion := r.Workspace.ReadDocument(st.Cycle, "03-discusion.md")
	_, err = artifact.Publish(r.Workspace.Store, st.Cycle, workflow.ArtifactRef{ID: "discussion-" + hash, Path: fmt.Sprintf("cycles/%03d/inputs/%s/discussion.md", st.Cycle, hash), Version: hash}, []byte(discussion))
	if err != nil {
		return st, err
	}
	at := time.Now().UTC()
	st.Approval = &ws.ApprovalBinding{Actor: workflow.ActorHuman, PlanHash: st.PlanHash, ScopeHash: st.ScopeHash, BaselineHash: st.BaselineHash, ContractHash: hash, ApprovedAt: at}
	st.Phase = ws.PhaseApproved
	st.Log("contract approved", workflow.ActorHuman, note)
	payload, _ := json.Marshal(st)
	a := workflow.Approval{ID: fmt.Sprintf("A-%03d-%d", st.Cycle, at.UnixNano()), Cycle: st.Cycle, Actor: workflow.ActorHuman, PlanHash: st.PlanHash, ScopeHash: st.ScopeHash, Baseline: st.BaselineHash, ContractHash: hash, ApprovedAt: at}
	if err = r.Workspace.Store.ApproveContract(a, st.StateVersion, string(payload)); err != nil {
		return st, err
	}
	st, err = r.Workspace.LoadState()
	if err != nil {
		return st, err
	}
	return st, r.Workspace.RefreshProjection(st)
}
func (r *Runner) checkApproval(st *ws.State) error {
	// Re-read config and state on every boundary; a long-running process must
	// not keep authorizing work from a stale in-memory copy.
	cfg, err := config.Load(r.Workspace.Root)
	if err != nil {
		return err
	}
	target, err := repository.Open(cfg.Repo, r.Workspace.Root)
	if err != nil {
		return err
	}
	cfg.Repo.Path = target.Root
	before, _ := json.Marshal(r.Cfg)
	after, _ := json.Marshal(cfg)
	if string(before) != string(after) {
		return fmt.Errorf("configuration changed during work")
	}
	current, err := r.Workspace.LoadState()
	if err != nil {
		return err
	}
	c, err := r.executionContract(current)
	if err != nil {
		return err
	}
	hash, err := workflow.Hash(c)
	if err != nil {
		return err
	}
	if st.Approval == nil || st.Approval.ContractHash != hash {
		return fmt.Errorf("approval is stale or incomplete; review and approve again")
	}
	if err = r.Workspace.Store.HasApproval(st.Cycle, hash); err != nil {
		return err
	}
	_, err = (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(r.Workspace.Store, st.Cycle, "discussion-"+hash)
	if err != nil {
		return err
	}
	_, err = r.candidateContext(st)
	return err
}
func (r *Runner) publishCandidate(cycle int, id, path, text string) error {
	_, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Publish(r.Workspace.Store, cycle, workflow.ArtifactRef{ID: id, Path: fmt.Sprintf("cycles/%03d/%s", cycle, filepath.ToSlash(path)), Version: workflow.Digest(text)}, []byte(text))
	return err
}
func (r *Runner) candidateContext(st *ws.State) (string, error) {
	var b strings.Builder
	tickets, err := r.Workspace.Store.ListTickets(st.Cycle)
	if err != nil {
		return "", err
	}
	for _, t := range tickets {
		if t.Status != workflow.TicketCandidateReady {
			continue
		}
		id, err := r.Workspace.Store.CandidateRef(st.Cycle, t.ID)
		if err != nil {
			return "", err
		}
		data, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(r.Workspace.Store, st.Cycle, id)
		if err != nil {
			return "", err
		}
		record, err := r.Workspace.Store.GetArtifact(st.Cycle, id)
		if err != nil {
			return "", err
		}
		ids, err := r.Workspace.Store.ArtifactIDs(st.Cycle, filepath.ToSlash(filepath.Dir(record.Path))+"/")
		if err != nil {
			return "", err
		}
		for _, fileID := range ids {
			if _, err := (workflow.ArtifactStore{Root: r.Workspace.Root}).Read(r.Workspace.Store, st.Cycle, fileID); err != nil {
				return "", err
			}
		}
		fmt.Fprintf(&b, "\n## %s\n%s\n", t.ID, data)
	}
	return b.String(), nil
}
