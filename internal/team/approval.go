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
			return c, fmt.Errorf("approval is stale: repository baseline changed; invalidate and plan again")
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
	agents := r.Cfg.Agents
	if st.Markdown != nil {
		agents = r.activeAgents()
	}
	for role, a := range agents {
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
	if st.Markdown != nil {
		inputs["source"] = st.Markdown.Revision
		if st.Planning != nil {
			for _, ref := range st.Planning.Turns {
				inputs[ref.ID] = ref.SHA256
			}
		}
	} else {
		return c, fmt.Errorf("cycle has no Markdown ticket source")
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
	}{agents, r.Cfg.Repo, r.Cfg.OpenRouter, r.Cfg.SpecialistOrder})
	repo, _ := json.Marshal(snapshot)
	// The approval binds the execution backend, the candidate wire format and
	// the exact checkout, worktree and branch the diff will land in. All four
	// are inside the hash, so changing any of them invalidates the approval
	// instead of silently redirecting the change somewhere else.
	branch, err := target.Branch()
	if err != nil {
		return c, err
	}
	identity := workflow.ExecutionIdentity{
		Backend:         workflow.ExecutionBackendNative,
		CandidateSchema: workflow.CandidateSchemaVersion,
		Root:            snapshot.Root,
		CommonDir:       snapshot.CommonDir,
		Head:            snapshot.Head,
		Branch:          branch,
	}
	c = workflow.ExecutionContract{Version: workflow.ContractVersion, Execution: identity, Plan: *st.Plan, ContextHash: st.ScopeHash, Baseline: snapshot.Baseline(), Repository: repo, Inputs: inputs, Settings: settings, Policy: r.Cfg.Execution}
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
	reviewHash, err := r.Workspace.Store.ObservationReviewHash(st.Cycle, hash)
	if err != nil {
		return "", err
	}
	observations, err := r.Workspace.Store.Observations(st.Cycle)
	if err != nil {
		return "", err
	}
	observationJSON, _ := json.MarshalIndent(observations, "", "  ")
	raw, _ := json.MarshalIndent(c, "", "  ")
	budget, _ := json.MarshalIndent(b, "", "  ")
	return fmt.Sprintf("Contract: %s\n%s\n\nBudget (cycle-wide; all checks apply to every ticket):\n%s\n", reviewHash, raw, budget) + "\nObservations (included in review token):\n" + string(observationJSON) + "\n", nil
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
	expectedReview, err := r.Workspace.Store.ObservationReviewHash(st.Cycle, hash)
	if err != nil {
		return st, err
	}
	if reviewedHash == "" || reviewedHash != expectedReview {
		return st, fmt.Errorf("reviewed contract changed before approval; review again")
	}
	if st.Phase == ws.PhaseApproved || st.Phase == ws.PhaseAwaitingExecution || st.Phase == ws.PhaseAwaitingReview {
		if err = r.Workspace.Store.ApproveObservations(st.Cycle, st.StateVersion, hash, reviewedHash); err != nil {
			return st, err
		}
		st, err = r.Workspace.LoadState()
		if err != nil {
			return st, err
		}
		return st, r.Workspace.Store.HasApproval(st.Cycle, hash)
	}
	if st.Phase != ws.PhaseWaiting {
		return st, fmt.Errorf("only awaiting_approval plans can be approved")
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
	if _, err = r.candidateContext(st); err != nil {
		return err
	}
	return r.verifyExecutionEvidence(st)
}

// verifyExecutionEvidence re-reads every artifact this contract's execution
// rounds point at. ArtifactStore.Read checks each file against the hash the
// store recorded, so a rewritten candidate, manifest, diff or check result is
// caught here — before the next model call, patch or check is allowed to
// build on it. A ticket that has moved past candidate_ready is covered too,
// which candidateContext alone would not be.
func (r *Runner) verifyExecutionEvidence(st *ws.State) error {
	if st.Approval == nil || st.Approval.ContractHash == "" {
		return nil
	}
	rounds, err := r.Workspace.Store.Rounds(st.Cycle)
	if err != nil {
		return err
	}
	artifacts := workflow.ArtifactStore{Root: r.Workspace.Root}
	for _, round := range rounds {
		if round.Contract != st.Approval.ContractHash {
			continue
		}
		refs := append([]workflow.ArtifactRef{}, round.Inputs...)
		for _, ref := range []*workflow.ArtifactRef{round.Response, round.Candidate, round.Patch, round.Diff, round.Manifest} {
			if ref != nil {
				refs = append(refs, *ref)
			}
		}
		for _, c := range round.Checks {
			if c.Evidence.ID != "" {
				refs = append(refs, c.Evidence)
			}
		}
		for _, ref := range refs {
			if _, err := artifacts.Read(r.Workspace.Store, st.Cycle, ref.ID); err != nil {
				return fmt.Errorf("execution evidence %s for %s round %d: %w", ref.ID, round.Ticket, round.Round, err)
			}
		}
	}
	return nil
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
