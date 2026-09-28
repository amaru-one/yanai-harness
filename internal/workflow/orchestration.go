package workflow

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// ---- Orchestration proposals ----

// Proposal states. A proposal is saved whatever the validation said, so a
// human can see why a parent's answer was refused; only a valid one can be
// reviewed and approved.
const (
	ProposalValid   = "valid"
	ProposalInvalid = "invalid"
)

// ProposalRecord is one parent proposal with the exact inputs it answered.
// Payload is the caller-defined proposal body (see internal/orchestrator).
type ProposalRecord struct {
	ID         string          `json:"id"`
	Cycle      int             `json:"cycle"`
	InputHash  string          `json:"input_hash"`
	Worker     string          `json:"worker"`
	State      string          `json:"state"`
	Validation string          `json:"validation,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

// SaveProposal records a proposal once. Replaying the same proposal is a
// no-op; a different body under the same ID is refused.
func (s *Store) SaveProposal(r ProposalRecord) error {
	if r.ID == "" || r.InputHash == "" || (r.State != ProposalValid && r.State != ProposalInvalid) {
		return errors.New("proposal requires id, input hash and a valid state")
	}
	if existing, err := s.Proposal(r.Cycle, r.ID); err == nil {
		if existing.InputHash != r.InputHash || existing.State != r.State || string(existing.Payload) != string(r.Payload) {
			return fmt.Errorf("proposal %s already recorded with different content", r.ID)
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err = tx.Exec(`INSERT INTO workflow_orchestration_proposals(project,cycle,id,input_hash,worker,state,validation,payload,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		s.project, r.Cycle, r.ID, r.InputHash, r.Worker, r.State, r.Validation, string(r.Payload), now()); err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: r.Cycle, Actor: ActorEngine, Type: "proposal." + r.State, Payload: r.ID}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Proposal(cycle int, id string) (ProposalRecord, error) {
	r := ProposalRecord{Cycle: cycle, ID: id}
	var payload, created string
	err := s.db.QueryRow(`SELECT input_hash,worker,state,validation,payload,created_at FROM workflow_orchestration_proposals WHERE project=? AND cycle=? AND id=?`, s.project, cycle, id).
		Scan(&r.InputHash, &r.Worker, &r.State, &r.Validation, &payload, &created)
	r.Payload = json.RawMessage(payload)
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return r, err
}

// ---- Agent steps ----

// Agent step states, in order. A step is recorded before its model call so
// an interrupted dispatch is visible; the response is recorded before its
// tool runs; a side-effecting tool records its validated intent before it
// acts; done means the tool result is durable.
const (
	StepRequested = "requested"
	StepResponded = "responded"
	StepIntent    = "intent"
	StepDone      = "done"
)

// AgentStep is one model turn in a saved loop and the single tool call it
// made. Run names the loop (planning, execution or closing of one cycle).
type AgentStep struct {
	Run        string          `json:"run"`
	Seq        int             `json:"seq"`
	Role       string          `json:"role"`
	State      string          `json:"state"`
	Request    *ArtifactRef    `json:"request,omitempty"`
	Response   *ArtifactRef    `json:"response,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	// Intent is what a side-effecting tool recorded before acting, so a
	// restart can recognize whether the effect already happened.
	Intent   json.RawMessage `json:"intent,omitempty"`
	Result   *ArtifactRef    `json:"result,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
	Terminal bool            `json:"terminal,omitempty"`
	// Forced marks a final turn that was offered only the run's terminal
	// tool; replay reinserts the same notice so the transcript is identical.
	Forced bool `json:"forced,omitempty"`
	// The provider's native token counts for this step's model call.
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
}

var stepOrder = map[string]int{StepRequested: 0, StepResponded: 1, StepIntent: 2, StepDone: 3}

// SaveAgentStep inserts a step or advances it. A step never moves backwards,
// a done step never changes, and a tool call ID can belong to one step only.
func (s *Store) SaveAgentStep(cycle int, st AgentStep) error {
	if st.Run == "" || st.Seq < 1 || st.Role == "" {
		return errors.New("agent step requires run, positive sequence and role")
	}
	if _, ok := stepOrder[st.State]; !ok {
		return fmt.Errorf("unknown agent step state %q", st.State)
	}
	payload, err := json.Marshal(st)
	if err != nil {
		return err
	}
	var callID any
	if st.ToolCallID != "" {
		callID = st.ToolCallID
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var current, raw string
	err = tx.QueryRow(`SELECT state,payload FROM workflow_agent_steps WHERE project=? AND cycle=? AND run=? AND seq=?`, s.project, cycle, st.Run, st.Seq).Scan(&current, &raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if st.Seq > 1 {
			var previous string
			if err = tx.QueryRow(`SELECT state FROM workflow_agent_steps WHERE project=? AND cycle=? AND run=? AND seq=?`, s.project, cycle, st.Run, st.Seq-1).Scan(&previous); err != nil {
				return fmt.Errorf("agent step %d requires a recorded step %d: %w", st.Seq, st.Seq-1, err)
			}
			if previous != StepDone {
				return fmt.Errorf("agent step %d cannot start before step %d is done", st.Seq, st.Seq-1)
			}
		}
		if _, err = tx.Exec(`INSERT INTO workflow_agent_steps(project,cycle,run,seq,role,state,tool_call_id,tool_name,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			s.project, cycle, st.Run, st.Seq, st.Role, st.State, callID, st.ToolName, string(payload), now(), now()); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if raw == string(payload) {
			return nil
		}
		if current == StepDone {
			return fmt.Errorf("agent step %s/%d is done and cannot change", st.Run, st.Seq)
		}
		if stepOrder[st.State] < stepOrder[current] {
			return fmt.Errorf("agent step %s/%d cannot move from %s back to %s", st.Run, st.Seq, current, st.State)
		}
		if _, err = tx.Exec(`UPDATE workflow_agent_steps SET state=?,tool_call_id=?,tool_name=?,payload=?,updated_at=? WHERE project=? AND cycle=? AND run=? AND seq=?`,
			st.State, callID, st.ToolName, string(payload), now(), s.project, cycle, st.Run, st.Seq); err != nil {
			return err
		}
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "agent.step." + st.State, Payload: fmt.Sprintf("%s/%d %s", st.Run, st.Seq, st.ToolName)}); err != nil {
		return err
	}
	return tx.Commit()
}

// AgentSteps returns a run's steps in order.
func (s *Store) AgentSteps(cycle int, run string) ([]AgentStep, error) {
	rows, err := s.db.Query(`SELECT payload FROM workflow_agent_steps WHERE project=? AND cycle=? AND run=? ORDER BY seq`, s.project, cycle, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []AgentStep
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var st AgentStep
		if err = json.Unmarshal([]byte(raw), &st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---- Git sessions and operations ----

// Git session states.
const (
	GitSessionPending  = "pending"   // approved, nothing done in Git yet
	GitSessionOnBranch = "on_branch" // the checkout is on the ticket branch
	GitSessionReturned = "returned"  // back on the original branch, SHA unchanged
)

// GitCommitRecord is one commit the harness made on the ticket branch.
type GitCommitRecord struct {
	SHA       string   `json:"sha"`
	Parent    string   `json:"parent"`
	Operation string   `json:"operation"`
	Subject   string   `json:"subject"`
	Paths     []string `json:"paths"`
}

// GitSession is the mutable Git side of an approved contract: where the
// checkout started, which branch the cycle owns, and what HEAD must be now.
// The contract keeps the immutable approval identity; this keeps the state
// that legitimately moves after each recorded action.
type GitSession struct {
	Contract       string            `json:"contract"`
	Root           string            `json:"root"`
	CommonDir      string            `json:"common_dir"`
	OriginalBranch string            `json:"original_branch"`
	OriginalSHA    string            `json:"original_sha"`
	TicketBranch   string            `json:"ticket_branch"`
	BaseSHA        string            `json:"base_sha"`
	ExpectedBranch string            `json:"expected_branch"`
	ExpectedSHA    string            `json:"expected_sha"`
	State          string            `json:"state"`
	AuthorName     string            `json:"author_name"`
	AuthorEmail    string            `json:"author_email"`
	Commits        []GitCommitRecord `json:"commits,omitempty"`
	// HeadState is the clean repository state at the current expected
	// commit. The executor predicts working-tree status against it, since
	// after a commit the approved base no longer describes HEAD.
	HeadState RepositoryState `json:"head_state"`
}

// GitSession returns the session for a contract, with its revision.
func (s *Store) GitSession(cycle int, contract string) (GitSession, int64, bool, error) {
	var raw string
	var revision int64
	err := s.db.QueryRow(`SELECT payload,revision FROM workflow_git_sessions WHERE project=? AND cycle=? AND contract_hash=?`, s.project, cycle, contract).Scan(&raw, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return GitSession{}, 0, false, nil
	}
	if err != nil {
		return GitSession{}, 0, false, err
	}
	var g GitSession
	err = json.Unmarshal([]byte(raw), &g)
	return g, revision, true, err
}

// SaveGitSession creates the session (expected revision 0) or advances it
// under its conditional revision.
func (s *Store) SaveGitSession(cycle int, g GitSession, expected int64) (int64, error) {
	if g.Contract == "" || g.TicketBranch == "" || g.BaseSHA == "" {
		return 0, errors.New("git session requires contract, ticket branch and base commit")
	}
	payload, err := json.Marshal(g)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if expected == 0 {
		if _, err = tx.Exec(`INSERT INTO workflow_git_sessions(project,cycle,contract_hash,state,payload,revision,updated_at) VALUES(?,?,?,?,?,1,?)`, s.project, cycle, g.Contract, g.State, string(payload), now()); err != nil {
			return 0, err
		}
	} else {
		res, err := tx.Exec(`UPDATE workflow_git_sessions SET state=?,payload=?,revision=revision+1,updated_at=? WHERE project=? AND cycle=? AND contract_hash=? AND revision=?`, g.State, string(payload), now(), s.project, cycle, g.Contract, expected)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return 0, &ErrStaleVersion{Kind: "git session", Expected: expected}
		}
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "git.session." + g.State, Payload: g.ExpectedBranch + "@" + g.ExpectedSHA}); err != nil {
		return 0, err
	}
	return expected + 1, tx.Commit()
}

// Git operation kinds and states.
const (
	GitOpCreateBranch = "create_branch"
	GitOpSwitch       = "switch"
	GitOpCommit       = "commit"
	GitOpReturn       = "return"

	GitOpIntent = "intent"
	GitOpDone   = "done"
)

// GitOperation is the journal entry written before a ref or HEAD moves. It
// holds everything needed to finish or recognize the operation after a
// crash without guessing: the expected pre-state, and for a commit the exact
// parent, tree, message, identity and timestamp, so recreating the commit
// object yields the same SHA.
type GitOperation struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	State       string          `json:"state"`
	Branch      string          `json:"branch"`
	FromBranch  string          `json:"from_branch,omitempty"`
	FromSHA     string          `json:"from_sha"`
	ToSHA       string          `json:"to_sha,omitempty"`
	Parent      string          `json:"parent,omitempty"`
	Tree        string          `json:"tree,omitempty"`
	Message     string          `json:"message,omitempty"`
	Paths       []string        `json:"paths,omitempty"`
	AuthorName  string          `json:"author_name,omitempty"`
	AuthorEmail string          `json:"author_email,omitempty"`
	Timestamp   string          `json:"timestamp,omitempty"`
	Pre         RepositoryState `json:"pre"`
	Post        RepositoryState `json:"post,omitempty"`
}

// BeginGitOperation records intent. Replaying identical intent returns the
// recorded operation; a conflicting one under the same ID is refused.
func (s *Store) BeginGitOperation(cycle int, contract string, op GitOperation) (GitOperation, error) {
	if op.ID == "" || op.Kind == "" {
		return op, errors.New("git operation requires id and kind")
	}
	op.State = GitOpIntent
	if existing, found, err := s.GitOperationByID(cycle, contract, op.ID); err != nil {
		return op, err
	} else if found {
		probe := existing
		probe.State, probe.ToSHA, probe.Post = op.State, op.ToSHA, op.Post
		if !reflect.DeepEqual(probe, op) {
			return existing, fmt.Errorf("git operation %s already recorded with different intent", op.ID)
		}
		return existing, nil
	}
	pending, err := s.PendingGitOperations(cycle, contract)
	if err != nil {
		return op, err
	}
	if len(pending) > 0 {
		return op, fmt.Errorf("git operation %s is still pending; recover it first", pending[0].ID)
	}
	payload, _ := json.Marshal(op)
	tx, err := s.db.Begin()
	if err != nil {
		return op, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err = tx.Exec(`INSERT INTO workflow_git_operations(project,cycle,contract_hash,id,kind,state,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		s.project, cycle, contract, op.ID, op.Kind, op.State, string(payload), now(), now()); err != nil {
		return op, err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "git.intent." + op.Kind, Payload: op.ID}); err != nil {
		return op, err
	}
	return op, tx.Commit()
}

// CompleteGitOperation marks an operation done with the observed result and,
// in the same transaction, moves the executor's recorded repository state
// from the operation's pre-state to its post-state and saves the session.
func (s *Store) CompleteGitOperation(cycle int, contract string, op GitOperation, session GitSession, sessionRevision int64) (int64, error) {
	op.State = GitOpDone
	payload, _ := json.Marshal(op)
	sessionPayload, _ := json.Marshal(session)
	post, _ := json.Marshal(op.Post)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.Exec(`UPDATE workflow_git_operations SET state=?,payload=?,updated_at=? WHERE project=? AND cycle=? AND contract_hash=? AND id=? AND state=?`, GitOpDone, string(payload), now(), s.project, cycle, contract, op.ID, GitOpIntent)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, fmt.Errorf("git operation %s is not pending", op.ID)
	}
	res, err = tx.Exec(`UPDATE workflow_patch_states SET current_hash=?,current_json=?,revision=revision+1 WHERE project=? AND cycle=? AND contract_hash=? AND current_hash=? AND post_hash=''`,
		op.Post.Baseline(), string(post), s.project, cycle, contract, op.Pre.Baseline())
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, errors.New("recorded repository state moved during the Git operation")
	}
	res, err = tx.Exec(`UPDATE workflow_git_sessions SET state=?,payload=?,revision=revision+1,updated_at=? WHERE project=? AND cycle=? AND contract_hash=? AND revision=?`, session.State, string(sessionPayload), now(), s.project, cycle, contract, sessionRevision)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, &ErrStaleVersion{Kind: "git session", Expected: sessionRevision}
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "git.done." + op.Kind, Payload: op.ID + " " + op.ToSHA}); err != nil {
		return 0, err
	}
	return sessionRevision + 1, tx.Commit()
}

func (s *Store) GitOperationByID(cycle int, contract, id string) (GitOperation, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT payload FROM workflow_git_operations WHERE project=? AND cycle=? AND contract_hash=? AND id=?`, s.project, cycle, contract, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return GitOperation{}, false, nil
	}
	if err != nil {
		return GitOperation{}, false, err
	}
	var op GitOperation
	err = json.Unmarshal([]byte(raw), &op)
	return op, true, err
}

// PendingGitOperations returns intents that were never confirmed.
func (s *Store) PendingGitOperations(cycle int, contract string) ([]GitOperation, error) {
	return s.gitOperations(cycle, contract, `AND state='intent'`)
}

// GitOperations returns every recorded operation, oldest first.
func (s *Store) GitOperations(cycle int, contract string) ([]GitOperation, error) {
	return s.gitOperations(cycle, contract, "")
}

func (s *Store) gitOperations(cycle int, contract, filter string) ([]GitOperation, error) {
	rows, err := s.db.Query(`SELECT payload FROM workflow_git_operations WHERE project=? AND cycle=? AND contract_hash=? `+filter+` ORDER BY created_at, id`, s.project, cycle, contract)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []GitOperation
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var op GitOperation
		if err = json.Unmarshal([]byte(raw), &op); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// ---- Configuration activation ----

// Configuration activation states.
const (
	ActivationPending = "pending"
	ActivationDone    = "done"
)

// ConfigActivation is the recoverable intent to replace the live
// yanai.config.json with the approved proposal. It is written in the same
// transaction as the approval, and nothing may act for the worker until it
// is done.
type ConfigActivation struct {
	Contract    string      `json:"contract"`
	OriginalSHA string      `json:"original_sha"`
	ProposedSHA string      `json:"proposed_sha"`
	Proposed    ArtifactRef `json:"proposed"`
	State       string      `json:"state"`
}

func (s *Store) ConfigActivation(cycle int, contract string) (ConfigActivation, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT payload FROM workflow_config_activations WHERE project=? AND cycle=? AND contract_hash=?`, s.project, cycle, contract).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigActivation{}, false, nil
	}
	if err != nil {
		return ConfigActivation{}, false, err
	}
	var a ConfigActivation
	err = json.Unmarshal([]byte(raw), &a)
	return a, true, err
}

// PendingConfigActivations lists every activation not yet completed, across
// cycles, so any command can finish one before relying on the live config.
func (s *Store) PendingConfigActivations() ([]ConfigActivation, []int, error) {
	rows, err := s.db.Query(`SELECT cycle,payload FROM workflow_config_activations WHERE project=? AND state=?`, s.project, ActivationPending)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []ConfigActivation
	var cycles []int
	for rows.Next() {
		var cycle int
		var raw string
		if err = rows.Scan(&cycle, &raw); err != nil {
			return nil, nil, err
		}
		var a ConfigActivation
		if err = json.Unmarshal([]byte(raw), &a); err != nil {
			return nil, nil, err
		}
		out = append(out, a)
		cycles = append(cycles, cycle)
	}
	return out, cycles, rows.Err()
}

func (s *Store) CompleteConfigActivation(cycle int, contract string) error {
	a, found, err := s.ConfigActivation(cycle, contract)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("no configuration activation recorded")
	}
	if a.State == ActivationDone {
		return nil
	}
	a.State = ActivationDone
	raw, _ := json.Marshal(a)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err = tx.Exec(`UPDATE workflow_config_activations SET state=?,payload=?,updated_at=? WHERE project=? AND cycle=? AND contract_hash=?`, a.State, string(raw), now(), s.project, cycle, contract); err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "config.activated", Payload: a.ProposedSHA}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- Project state proposals ----

// State proposal states.
const (
	StateUpdateProposed = "proposed"
	StateUpdateAccepted = "accepted"
)

// StateProposal is the parent's proposed replacement for project/estado.md,
// bound to the state file it was written against and the branch it describes.
type StateProposal struct {
	Hash      string      `json:"hash"`
	OldHash   string      `json:"old_hash"`
	BranchSHA string      `json:"branch_sha"`
	Branch    string      `json:"branch"`
	Summary   string      `json:"summary"`
	Content   ArtifactRef `json:"content"`
	Report    ArtifactRef `json:"report"`
	State     string      `json:"state"`
}

func (s *Store) SaveStateProposal(cycle int, p StateProposal) error {
	if p.Hash == "" || p.OldHash == "" || p.BranchSHA == "" {
		return errors.New("state proposal requires hashes and branch tip")
	}
	p.State = StateUpdateProposed
	raw, _ := json.Marshal(p)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err = tx.Exec(`INSERT INTO workflow_state_proposals(project,cycle,hash,state,payload,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, s.project, cycle, p.Hash, p.State, string(raw), now()); err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "state_update.proposed", Payload: p.Hash}); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestStateProposal returns the most recent proposal for a cycle.
func (s *Store) LatestStateProposal(cycle int) (StateProposal, bool, error) {
	var raw, state string
	err := s.db.QueryRow(`SELECT payload,state FROM workflow_state_proposals WHERE project=? AND cycle=? ORDER BY created_at DESC LIMIT 1`, s.project, cycle).Scan(&raw, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return StateProposal{}, false, nil
	}
	if err != nil {
		return StateProposal{}, false, err
	}
	var p StateProposal
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return p, false, err
	}
	p.State = state
	return p, true, nil
}

// CloseCycle accepts the reviewed state proposal and completes the cycle in
// one transaction. It is a human act, and repeating it is a no-op.
func (s *Store) CloseCycle(cycle int, expected int64, hash, payload string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var state string
	if err = tx.QueryRow(`SELECT state FROM workflow_state_proposals WHERE project=? AND cycle=? AND hash=?`, s.project, cycle, hash).Scan(&state); err != nil {
		return fmt.Errorf("state update %s is not recorded for cycle %03d: %w", hash, cycle, err)
	}
	var phase string
	if err = tx.QueryRow(`SELECT phase FROM workflow_cycles WHERE project=? AND cycle=?`, s.project, cycle).Scan(&phase); err != nil {
		return err
	}
	if state == StateUpdateAccepted && phase == PhaseCompleted {
		return nil
	}
	if !actorAllowed(cycleTransitions, phase, PhaseCompleted, ActorHuman) {
		return &ErrTransitionNotAllowed{From: phase, To: PhaseCompleted, Actor: ActorHuman}
	}
	if _, err = tx.Exec(`UPDATE workflow_state_proposals SET state=?,accepted_at=? WHERE project=? AND cycle=? AND hash=?`, StateUpdateAccepted, now(), s.project, cycle, hash); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE workflow_cycles SET phase=?,payload=?,state_version=state_version+1,updated_at=? WHERE project=? AND cycle=? AND state_version=?`, PhaseCompleted, payload, now(), s.project, cycle, expected)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return &ErrStaleVersion{Kind: "cycle", Expected: expected}
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorHuman, Type: "cycle.completed", Payload: hash}); err != nil {
		return err
	}
	return tx.Commit()
}

// EnsureRepairAttempts charges repair attempts up to n for a task, exactly
// once each, so replaying a saved step after a restart cannot charge twice.
// The first attempt is free; later ones consume the cycle's repair budget.
func (s *Store) EnsureRepairAttempts(cycle int, ticket string, n, maxAttempts int) error {
	for {
		var attempts int
		err := s.db.QueryRow(`SELECT attempts FROM workflow_repairs WHERE project=? AND cycle=? AND ticket=?`, s.project, cycle, ticket).Scan(&attempts)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if attempts >= n {
			return nil
		}
		if err = s.ChargeRepair(cycle, ticket, maxAttempts); err != nil {
			return err
		}
	}
}

// RepairAttempts reports the attempts recorded for a task.
func (s *Store) RepairAttempts(cycle int, ticket string) (int, error) {
	var attempts int
	err := s.db.QueryRow(`SELECT attempts FROM workflow_repairs WHERE project=? AND cycle=? AND ticket=?`, s.project, cycle, ticket).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return attempts, err
}

// AgentRuns lists the run keys of a cycle that start with prefix, in the
// order they were first recorded.
func (s *Store) AgentRuns(cycle int, prefix string) ([]string, error) {
	rows, err := s.db.Query(`SELECT run FROM workflow_agent_steps WHERE project=? AND cycle=? AND run LIKE ? GROUP BY run ORDER BY MIN(created_at)`, s.project, cycle, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var run string
		if err = rows.Scan(&run); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}
