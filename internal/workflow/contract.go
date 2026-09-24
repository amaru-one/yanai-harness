package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ContractVersion is the execution-contract revision this binary executes.
// Revision 1 predates native application: an approval recorded against it
// authorized staging a candidate, never writing to the application checkout,
// so it is refused rather than reinterpreted. Revision 2 names the execution
// backend, the candidate wire format, and the exact checkout/branch identity
// the human saw, so none of those can change between review and execution.
const ContractVersion = 3

// ExecutionBackendNative and CandidateSchemaVersion are the only values this
// binary can execute. They are part of the contract (and therefore of its
// hash) so that swapping either one invalidates the approval.
const (
	ExecutionBackendNative = "native"
	CandidateSchemaVersion = "1"
)

// ExecutionIdentity pins what the approval authorized execution *against*:
// which backend applies the change, which candidate format the model must
// emit, and which checkout, worktree and branch the diff lands in. Root and
// CommonDir together distinguish two clones at the same commit; Branch is
// recorded because a checkout can move between branches without its HEAD
// commit or working tree changing at all.
type ExecutionIdentity struct {
	Backend         string `json:"backend"`
	CandidateSchema string `json:"candidate_schema"`
	Root            string `json:"root"`
	CommonDir       string `json:"common_dir"`
	Head            string `json:"head"`
	Branch          string `json:"branch"`
}

type ExecutionContract struct {
	Version     int               `json:"version"`
	Execution   ExecutionIdentity `json:"execution"`
	Plan        Proposal          `json:"plan"`
	ContextHash string            `json:"context_hash"`
	Baseline    string            `json:"baseline"`
	Repository  json.RawMessage   `json:"repository"`
	Inputs      map[string]string `json:"inputs"`
	Settings    json.RawMessage   `json:"settings"`
	Policy      ExecutionPolicy   `json:"policy"`
}

// ValidateExecutable refuses an older or foreign contract before any model
// call or repository write. state is the repository the harness is bound to
// now and branch is its current branch; both must be exactly what the human
// approved. It deliberately reports the remedy, because the only valid answer
// to a stale contract is a fresh review and approval, never a silent upgrade.
func (c ExecutionContract) ValidateExecutable(state RepositoryState, branch string) error {
	if c.Version != ContractVersion {
		return fmt.Errorf("execution contract revision %d is not supported by this workflow (this binary executes revision %d); run 'yanai review' and 'yanai approve' again", c.Version, ContractVersion)
	}
	e := c.Execution
	if e.Backend != ExecutionBackendNative {
		return fmt.Errorf("execution backend %q is not available; approve a contract for the %q backend", e.Backend, ExecutionBackendNative)
	}
	if e.CandidateSchema != CandidateSchemaVersion {
		return fmt.Errorf("approved candidate format %q is not the one this binary validates (%q)", e.CandidateSchema, CandidateSchemaVersion)
	}
	if strings.TrimSpace(e.Branch) == "" || strings.TrimSpace(e.Head) == "" {
		return errors.New("execution contract is missing the approved checkout identity")
	}
	if e.Root != state.Root || e.CommonDir != state.CommonDir || e.Head != state.Head {
		return errors.New("approved checkout identity differs from the bound repository; review and approve against the checkout you intend to change")
	}
	if e.Branch != branch {
		return fmt.Errorf("approved branch %q is not the checked-out branch %q; review and approve again", e.Branch, branch)
	}
	return c.Policy.Validate()
}

func (s *Store) SaveContract(cycle int, c ExecutionContract) (string, error) {
	hash, err := Hash(c)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`INSERT INTO workflow_contracts(project,cycle,hash,payload) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, s.project, cycle, hash, string(data))
	return hash, err
}

// ApproveContract is the only live human gate: approval and transition commit
// together, and a conditional version prevents stale approval from winning.
func (s *Store) ApproveContract(a Approval, expected int64, payload string) error {
	if a.Actor != ActorHuman || a.ContractHash == "" {
		return errors.New("human approval requires a complete contract")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stored string
	if err = tx.QueryRow(`SELECT payload FROM workflow_contracts WHERE project=? AND cycle=? AND hash=?`, s.project, a.Cycle, a.ContractHash).Scan(&stored); err != nil {
		return err
	}
	var c ExecutionContract
	if err = json.Unmarshal([]byte(stored), &c); err != nil {
		return err
	}
	ph, _ := Hash(c.Plan)
	if !ApprovalValid(a, ph, c.ContextHash, c.Baseline, a.ContractHash) {
		return errors.New("approval does not match contract")
	}
	var unresolved int
	if err = tx.QueryRow(`SELECT count(*) FROM workflow_observations WHERE project=? AND cycle=? AND resolution=''`, s.project, a.Cycle).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved != 0 {
		return errors.New("observations require a human response before approval")
	}
	if _, err = tx.Exec(`UPDATE workflow_observations SET approved_contract=? WHERE project=? AND cycle=?`, a.ContractHash, s.project, a.Cycle); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE workflow_cycles SET phase='approved',active_contract=?,payload=?,state_version=state_version+1,updated_at=? WHERE project=? AND cycle=? AND phase='awaiting_approval' AND state_version=?`, a.ContractHash, payload, now(), s.project, a.Cycle, expected)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return &ErrStaleVersion{Kind: "cycle", Expected: expected}
	}
	_, err = tx.Exec(`INSERT INTO workflow_approvals(id,project,cycle,actor,plan_hash,scope_hash,baseline_hash,contract_hash,approved_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(project,cycle,contract_hash) DO NOTHING`, a.ID, s.project, a.Cycle, a.Actor, a.PlanHash, a.ScopeHash, a.Baseline, a.ContractHash, a.ApprovedAt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO workflow_patch_states(project,cycle,contract_hash,current_hash,current_json) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, s.project, a.Cycle, a.ContractHash, a.Baseline, string(c.Repository))
	if err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: a.Cycle, Actor: ActorHuman, Type: "contract.approved", Payload: a.ContractHash}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) HasApproval(cycle int, hash string) error {
	var phase, active string
	if err := s.db.QueryRow(`SELECT phase,active_contract FROM workflow_cycles WHERE project=? AND cycle=?`, s.project, cycle).Scan(&phase, &active); err != nil {
		return err
	}
	// awaiting_review is still under this approval: the change is applied but
	// nobody has reviewed or revoked it, so its evidence stays readable and a
	// rerun stays idempotent. Only a human rejection ends it.
	if active != hash || (phase != PhaseApproved && phase != PhaseAwaitingExecution && phase != PhaseAwaitingReview) {
		return errors.New("cycle has no active approval")
	}
	var blocked int
	if err := s.db.QueryRow(`SELECT count(*) FROM workflow_observations WHERE project=? AND cycle=? AND (resolution='' OR approved_contract<>?)`, s.project, cycle, hash).Scan(&blocked); err != nil {
		return err
	}
	if blocked > 0 {
		return errors.New("observations require human resolution and fresh review/approval before execution")
	}
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM workflow_approvals WHERE project=? AND cycle=? AND actor='human' AND contract_hash=? AND contract_hash<>''`, s.project, cycle, hash).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("approval missing or stale; review and approve the current contract")
	}
	return nil
}
func (s *Store) Invalidate(cycle int, version int64, note string) error {
	if note == "" {
		return errors.New("invalidation requires --note")
	}
	_, err := s.ApplyCyclePhase(cycle, PhaseRejected, ActorHuman, version, CycleFields{}, Event{Type: "approval.invalidated", Payload: note})
	return err
}
func (s *Store) Contract(cycle int, hash string) (ExecutionContract, error) {
	var raw string
	var c ExecutionContract
	err := s.db.QueryRow(`SELECT payload FROM workflow_contracts WHERE project=? AND cycle=? AND hash=?`, s.project, cycle, hash).Scan(&raw)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(raw), &c)
	return c, err
}

// RepositoryState includes the working tree and index independently of HEAD.
type RepositoryState struct {
	Root       string
	CommonDir  string
	Head       string
	Dirty      bool
	Content    map[string]string
	IndexHash  string
	StatusHash string
}

func (s RepositoryState) Baseline() string { h, _ := Hash(s); return h }
func (s RepositoryState) Label() string {
	state := "clean"
	if s.Dirty {
		state = "DIRTY"
	}
	return fmt.Sprintf("Repository: %s\nHEAD: %s\nWorking tree: %s\n", s.Root, s.Head, state)
}
func (s *Store) PatchState(cycle int, hash string) (RepositoryState, int64, error) {
	var raw, post string
	var version int64
	var state RepositoryState
	err := s.db.QueryRow(`SELECT current_json,post_hash,revision FROM workflow_patch_states WHERE project=? AND cycle=? AND contract_hash=?`, s.project, cycle, hash).Scan(&raw, &post, &version)
	if err != nil {
		return state, 0, err
	}
	if post != "" {
		return state, 0, errors.New("pending patch: reconcile before further execution")
	}
	err = json.Unmarshal([]byte(raw), &state)
	return state, version, err
}

// PendingPatch exposes recorded intent to the executor, without authorizing a
// new mutation. Revision is the revision AFTER PreparePatch committed.
func (s *Store) PendingPatch(cycle int, hash string) (before, after RepositoryState, revision int64, pending bool, err error) {
	var preJSON, postJSON, postHash string
	err = s.db.QueryRow(`SELECT current_json,post_json,post_hash,revision FROM workflow_patch_states WHERE project=? AND cycle=? AND contract_hash=?`, s.project, cycle, hash).Scan(&preJSON, &postJSON, &postHash, &revision)
	if err != nil || postHash == "" {
		return
	}
	pending = true
	if err = json.Unmarshal([]byte(preJSON), &before); err == nil {
		err = json.Unmarshal([]byte(postJSON), &after)
	}
	return
}

// PreparePatch records the exact intended successor; only declared outputs of
// this ticket may change. There is no CLI or model route to this engine API.
func (s *Store) PreparePatch(cycle int, hash, ticket string, next RepositoryState, version int64) error {
	if err := s.HasApproval(cycle, hash); err != nil {
		return err
	}
	c, err := s.Contract(cycle, hash)
	if err != nil {
		return err
	}
	prior, actualVersion, err := s.PatchState(cycle, hash)
	if err != nil {
		return err
	}
	if actualVersion != version {
		return errors.New("stale patch revision")
	}
	if prior.Root != next.Root || prior.CommonDir != next.CommonDir || prior.Head != next.Head || prior.IndexHash != next.IndexHash {
		return errors.New("patch cannot change repository identity, HEAD, or index")
	}
	var approved *Ticket
	for i := range c.Plan.Tickets {
		if c.Plan.Tickets[i].ID == ticket {
			approved = &c.Plan.Tickets[i]
		}
	}
	if approved == nil {
		return errors.New("unknown patch ticket")
	}
	paths := map[string]bool{}
	for p := range prior.Content {
		paths[p] = true
	}
	for p := range next.Content {
		paths[p] = true
	}
	delta := map[string][2]string{}
	for p := range paths {
		before, bok := prior.Content[p]
		after, aok := next.Content[p]
		if bok == aok && before == after {
			continue
		}
		allowed := false
		for _, out := range approved.Outputs {
			if p == out {
				allowed = true
			}
		}
		within := false
		for _, prefix := range approved.AllowedPaths {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				within = true
			}
		}
		if !allowed || !within {
			return fmt.Errorf("patch path outside approved ticket: %s", p)
		}
		delta[p] = [2]string{before, after}
	}
	if len(delta) == 0 {
		return errors.New("patch has no content changes")
	}
	raw, _ := json.Marshal(next)
	changes, _ := json.Marshal(delta)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE workflow_patch_states SET pre_hash=?,post_hash=?,post_json=?,delta=?,revision=revision+1 WHERE project=? AND cycle=? AND contract_hash=? AND revision=? AND post_hash=''`, prior.Baseline(), next.Baseline(), string(raw), string(changes), s.project, cycle, hash, version)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("stale or pending patch state")
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, TicketID: ticket, Actor: ActorEngine, Type: "patch.prepared", Payload: string(changes)}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ReconcilePatch(cycle int, hash string, actual RepositoryState) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pre, post, current, postJSON string
	if err = tx.QueryRow(`SELECT current_hash,pre_hash,post_hash,post_json FROM workflow_patch_states WHERE project=? AND cycle=? AND contract_hash=?`, s.project, cycle, hash).Scan(&current, &pre, &post, &postJSON); err != nil {
		return err
	}
	digest := actual.Baseline()
	if post == "" {
		if current == digest {
			return nil
		}
		return errors.New("unexpected external repository edit")
	}
	if digest != pre && digest != post {
		return errors.New("repository matches neither intended patch state; inspect manually")
	}
	if digest == post {
		var intended RepositoryState
		if err = json.Unmarshal([]byte(postJSON), &intended); err != nil {
			return err
		}
		if !reflect.DeepEqual(intended, actual) {
			return errors.New("patch content differs")
		}
	}
	raw, _ := json.Marshal(actual)
	_, err = tx.Exec(`UPDATE workflow_patch_states SET current_hash=?,current_json=?,pre_hash='',post_hash='',post_json='',delta='',revision=revision+1 WHERE project=? AND cycle=? AND contract_hash=?`, digest, string(raw), s.project, cycle, hash)
	if err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "patch.reconciled", Payload: digest}); err != nil {
		return err
	}
	return tx.Commit()
}
