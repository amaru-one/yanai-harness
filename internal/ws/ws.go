// Package ws manages the on-disk workspace: prompts, context,
// and each cycle's documents. Cycle *state* itself is authoritative in
// internal/workflow's Store once a Workspace has one attached (Step 5);
// cycles/NNN/state.json becomes a generated, labelled projection of that —
// see writeProjection — kept only because it is a convenient, greppable
// snapshot. A Workspace opened with Store == nil (bare ws.Open) uses the
// file projection for lightweight local operations and tests.
package ws

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/lockfile"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// Cycle phases. The flow only advances in this order. Values are shared,
// deliberately, with internal/workflow's own phase constants (PhaseAnalyzed
// here and workflow.PhaseAnalyzed are the same string) — TestPhasesMatchWorkflowConstants
// in ws_test.go pins that down, since SaveState's dispatch (below) depends on it.
const (
	PhaseEmpty             = "no_cycle"
	PhaseAnalyzed          = "analyzed" // the engineer produced a ticket plan
	PhaseNoChange          = "no_change_needed"
	PhaseNeedsEvidence     = "needs_evidence"
	PhaseOutOfScope        = "out_of_scope"
	PhaseBlockedBaseline   = "blocked_by_baseline"
	PhaseWaiting           = "awaiting_approval"
	PhaseApproved          = "approved"
	PhaseRejected          = "rejected"
	PhaseAwaitingExecution = "awaiting_execution"
	PhaseAwaitingReview    = "awaiting_review"
)

// Task is an assignment to a team member. Status
// mirrors internal/workflow's ticket statuses (see transitions.go there) —
// "pending", "claimed", "response_recorded", "candidate_ready" or
// "response_rejected" — never "done": nothing here has been verified.
type Task struct {
	ID          string   `json:"id"`
	Owner       string   `json:"owner"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Criteria    []string `json:"criteria"`
	DependsOn   []string `json:"depends_on"`
	Status      string   `json:"status"`
	Deliverable string   `json:"deliverable,omitempty"`
}

// Event leaves a trace of what happened, for auditing. Distinct from
// workflow.Event: this one is human-readable history embedded in State;
// workflow.Event is the store's own append-only, transactionally-committed
// fact log.
type Event struct {
	When time.Time `json:"when"`
	What string    `json:"what"`
	Who  string    `json:"who,omitempty"`
	Note string    `json:"note,omitempty"`
}

// State is a cycle's data. For a store-backed cycle it is exactly what's
// marshalled into the store's payload column (see SaveState) plus the
// column-backed fields (Phase, Verdict, the hashes) kept in sync with it;
// StateVersion is never part of that payload (json:"-") since it is the
// version *of* the payload, not data inside it.
type State struct {
	SchemaVersion string                   `json:"schema_version,omitempty"`
	Markdown      *workflow.MarkdownTicket `json:"markdown_ticket,omitempty"`
	Planning      *PlanningState           `json:"planning,omitempty"`
	BaseCommit    string                   `json:"base_commit,omitempty"`
	Plan          *workflow.Proposal       `json:"plan,omitempty"`
	Cycle         int                      `json:"cycle"`
	Phase         string                   `json:"phase"`
	Verdict       string                   `json:"verdict,omitempty"`
	PlanHash      string                   `json:"plan_hash,omitempty"`
	ScopeHash     string                   `json:"scope_hash,omitempty"`
	BaselineHash  string                   `json:"baseline_hash,omitempty"`
	Approval      *ApprovalBinding         `json:"approval,omitempty"`
	Created       time.Time                `json:"created"`
	Updated       time.Time                `json:"updated"`
	Tasks         []Task                   `json:"tasks"`
	History       []Event                  `json:"history"`
	StateVersion  int64                    `json:"-"`
}

// ApprovalBinding records the exact inputs a human approved. A plan or scope
// edit invalidates the binding before execution can resume.
type ApprovalBinding struct {
	ContractHash string    `json:"contract_hash"`
	Actor        string    `json:"actor"`
	PlanHash     string    `json:"plan_hash"`
	ScopeHash    string    `json:"scope_hash"`
	BaselineHash string    `json:"baseline_hash"`
	ApprovedAt   time.Time `json:"approved_at"`
}

// Workspace points to the working directory. Store is nil until the caller
// attaches one (main.go's openWorkspace does, for every real CLI command);
// every persistence method below checks it explicitly rather than assuming
// it's set, so a bare ws.Open keeps working exactly as it always has.
type Workspace struct {
	Root  string
	Store *workflow.Store
}

// Open returns a Workspace over the given path, with no store attached.
func Open(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Workspace{Root: abs}, nil
}

func (w *Workspace) path(p ...string) string {
	return filepath.Join(append([]string{w.Root}, p...)...)
}

// Prompts and context.
func (w *Workspace) PromptPath(rel string) string { return w.path(rel) }
func (w *Workspace) ContextDir() string           { return w.path("context") }
func (w *Workspace) CyclesDir() string            { return w.path("cycles") }

// CycleDir returns cycles/NNN.
func (w *Workspace) CycleDir(n int) string {
	return filepath.Join(w.CyclesDir(), fmt.Sprintf("%03d", n))
}

// LockWriter takes the workspace-level writer lock (workflow.lock). It
// answers "is another yanai process using this workspace right now?" — the
// repository writer lock (internal/repository) answers the same question
// for the application checkout, a separate resource with its own lock.
func (w *Workspace) LockWriter() (func(), error) {
	unlock, err := lockfile.Lock(w.path("workflow.lock"))
	if err != nil {
		return nil, fmt.Errorf("another yanai process is already using this workspace: %w", err)
	}
	return unlock, nil
}

// LastCycle returns the highest cycle number that has a directory under
// cycles/, or 0 if there are none.
func (w *Workspace) LastCycle() int {
	if w.Store != nil {
		n, err := w.Store.MaxCycle()
		if err != nil {
			return 0
		}
		return n
	}
	entries, err := os.ReadDir(w.CyclesDir())
	if err != nil {
		return 0
	}
	var nums []int
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		if n, err := strconv.Atoi(d.Name()); err == nil {
			nums = append(nums, n)
		}
	}
	if len(nums) == 0 {
		return 0
	}
	sort.Ints(nums)
	return nums[len(nums)-1]
}

// NewCycle reserves the next cycle number and its directory. With a store
// attached, the store row is created at workflow.PhaseNoCycle (state_version
// 1) and nothing is written to disk yet — the caller's first SaveState call
// does that, alongside recording whatever it already knows (origin comes
// from the caller because at this point, before intake is even attached to
// the returned State, it's the only thing that is known). Without a store,
// this is the original, unconditional file write.
func (w *Workspace) NewCycle(origin string) (*State, error) {
	if w.Store != nil {
		n, err := w.Store.MaxCycle()
		if err != nil {
			return nil, err
		}
		n++
		if _, err := w.Store.CreateCycle(n, origin); err != nil {
			return nil, err
		}
		if err := w.mkdirs(n); err != nil {
			return nil, err
		}
		now := time.Now()
		st := &State{Cycle: n, Phase: PhaseEmpty, Created: now, Updated: now, StateVersion: 1}
		st.Log("cycle created", "", "")
		return st, nil
	}

	n := w.LastCycle() + 1
	if err := w.mkdirs(n); err != nil {
		return nil, err
	}
	now := time.Now()
	st := &State{Cycle: n, Phase: PhaseEmpty, Created: now, Updated: now}
	st.Log("cycle created", "", "")
	return st, w.SaveState(st, workflow.ActorEngine)
}

func (w *Workspace) mkdirs(n int) error {
	dir := w.CycleDir(n)
	for _, sub := range []string{"", "deliverables"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// LoadState reads the state of the last cycle.
func (w *Workspace) LoadState() (*State, error) {
	n := w.LastCycle()
	if n == 0 {
		return nil, fmt.Errorf("there's no cycle yet; start with 'yanai plan <ticket.md>'")
	}
	return w.LoadCycleState(n)
}

// LoadCycleState reads the state of a specific cycle from the store when one
// is attached, or from the generated projection for lightweight operations.
func (w *Workspace) LoadCycleState(n int) (*State, error) {
	if w.Store != nil {
		return w.loadFromStore(n)
	}
	b, err := os.ReadFile(filepath.Join(w.CycleDir(n), "state.json"))
	if err != nil {
		return nil, fmt.Errorf("could not read the state of cycle %03d: %w", n, err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (w *Workspace) loadFromStore(n int) (*State, error) {
	rec, err := w.Store.GetCycle(n)
	if err != nil {
		return nil, err
	}
	var st State
	if rec.Payload != "" {
		if err := json.Unmarshal([]byte(rec.Payload), &st); err != nil {
			return nil, fmt.Errorf("cycle %03d: corrupt stored payload: %w", n, err)
		}
	}
	// The columns are authoritative over whatever the payload also happens
	// to carry for these same fields — they're written together (SaveState)
	// so they should never disagree, but if they ever did, the column that
	// the transition table and every version check actually gate on wins.
	st.Cycle = n
	st.Phase = rec.Phase
	st.Verdict = rec.Verdict
	st.BaseCommit = rec.BaseCommit
	st.PlanHash = rec.PlanHash
	st.ScopeHash = rec.ScopeHash
	st.BaselineHash = rec.BaselineHash
	st.StateVersion = rec.StateVersion
	return &st, nil
}

// SaveState persists st. With a store attached, this is a conditional
// commit: st.StateVersion must match what the store currently holds for
// this cycle, or the write is refused (workflow.ErrStaleVersion) rather than
// silently overwriting whatever else committed first. Whether the commit is
// a phase change or a same-phase update (e.g. recording a task's progress
// mid-run, or intake before the model is even asked) is detected by
// comparing st.Phase to what's currently stored — not something the caller
// has to get right. actor is who is requesting this: workflow.ActorHuman
// for Approve/Reject, workflow.ActorEngine for everything else; the
// transition table (internal/workflow/transitions.go) is what actually
// enforces that only a human reaches "approved" or "rejected".
//
// Without a store, this writes the local projection directly.
func (w *Workspace) SaveState(st *State, actor string) error {
	st.Updated = time.Now()
	if w.Store != nil {
		if err := w.commitToStore(st, actor); err != nil {
			return err
		}
		return w.writeProjection(st)
	}
	return w.writeStateFile(st)
}

func (w *Workspace) commitToStore(st *State, actor string) error {
	current, err := w.Store.GetCycle(st.Cycle)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(st)
	if err != nil {
		return err
	}
	fields := workflow.CycleFields{
		Verdict:      st.Verdict,
		BaseCommit:   st.BaseCommit,
		PlanHash:     st.PlanHash,
		ScopeHash:    st.ScopeHash,
		BaselineHash: st.BaselineHash,
		Payload:      string(payload),
	}
	// A field explicitly cleared back to "" (e.g. a rejected plan's PlanHash)
	// can't be expressed by CycleFields' overlay-if-nonblank convention;
	// nothing in this flow needs that yet, so it's left as a known limit
	// rather than solved speculatively.
	var rec workflow.CycleRecord
	if st.Phase == current.Phase {
		rec, err = w.Store.SetCyclePayload(st.Cycle, st.StateVersion, actor, fields, workflow.Event{})
	} else {
		rec, err = w.Store.ApplyCyclePhase(st.Cycle, st.Phase, actor, st.StateVersion, fields, workflow.Event{})
	}
	if err != nil {
		return err
	}
	st.StateVersion = rec.StateVersion
	return nil
}

func (w *Workspace) writeStateFile(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.CycleDir(st.Cycle), "state.json"), b, 0o600)
}

// writeProjection regenerates cycles/NNN/state.json as a labelled,
// read-only snapshot of what the store now holds: every field state.json
// has always had, plus underscore-prefixed metadata naming where it came
// from and which store revision it reflects. The engine never reads this
// file back for a store-backed cycle (LoadCycleState reads the store
// directly); it exists for a human to read, and so a hand-edit is visibly
// what it is rather than indistinguishable from real state.
func (w *Workspace) writeProjection(st *State) error {
	body, err := json.Marshal(st)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	header := map[string]any{
		"_generated_from":  "../../workflow.db",
		"_generated_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"_source_revision": st.StateVersion,
		"_do_not_edit":     "Generated from workflow.db. Edits are ignored and overwritten.",
	}
	for k, v := range header {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		fields[k] = b
	}
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.CycleDir(st.Cycle), "state.json"), out, 0o600)
}

// Log appends an entry to the history.
func (st *State) Log(what, who, note string) {
	st.History = append(st.History, Event{When: time.Now(), What: what, Who: who, Note: note})
}

// Task looks up a task by ID.
func (st *State) Task(id string) *Task {
	for i := range st.Tasks {
		if strings.EqualFold(st.Tasks[i].ID, id) {
			return &st.Tasks[i]
		}
	}
	return nil
}

// WriteDocument saves a file within the current cycle.
func (w *Workspace) WriteDocument(cycle int, name, content string) (string, error) {
	path := filepath.Join(w.CycleDir(cycle), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ReadDocument reads a file from the cycle. Returns "" if it doesn't exist.
func (w *Workspace) ReadDocument(cycle int, name string) string {
	b, err := os.ReadFile(filepath.Join(w.CycleDir(cycle), name))
	if err != nil {
		return ""
	}
	return string(b)
}

// ReadContext concatenates the context/ documents (scope, product, etc.).
func (w *Workspace) ReadContext() string {
	var b strings.Builder
	entries, err := os.ReadDir(w.ContextDir())
	if err != nil {
		return "_(no context documents)_"
	}
	var names []string
	for _, d := range entries {
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(w.ContextDir(), n))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "### context/%s\n\n%s\n\n", n, string(data))
	}
	if b.Len() == 0 {
		return "_(no context documents)_"
	}
	return b.String()
}

// CycleHistory summarizes previous cycles so the PO doesn't repeat work.
func (w *Workspace) CycleHistory(upTo int) string {
	var b strings.Builder
	for n := 1; n < upTo; n++ {
		st, err := w.LoadCycleState(n)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "- Cycle %03d: phase=%s verdict=%s tasks=%d\n", n, st.Phase, st.Verdict, len(st.Tasks))
		if plan := w.ReadDocument(n, "04-plan.md"); plan != "" {
			fmt.Fprintf(&b, "  Plan summary: %s\n", firstLines(plan, 8))
		}
	}
	if b.Len() == 0 {
		return "_(this is the first cycle)_"
	}
	return b.String()
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

// RefreshProjection writes a view after a transaction owned by the workflow store.
func (w *Workspace) RefreshProjection(st *State) error { return w.writeProjection(st) }

// PlanningState checkpoints bounded role turns; raw responses remain artifacts.
type PlanningState struct {
	Complete    bool                   `json:"complete"`
	Binding     string                 `json:"binding"`
	Stage       int                    `json:"stage"`
	Specialists []string               `json:"specialists,omitempty"`
	Turns       []workflow.ArtifactRef `json:"turns,omitempty"`
	Draft       *workflow.Proposal     `json:"draft,omitempty"`
	Resolutions string                 `json:"resolutions,omitempty"`
}
