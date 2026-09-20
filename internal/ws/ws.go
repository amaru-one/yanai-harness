// Package ws manages the on-disk workspace and cycle state.
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

	"github.com/yanai/yanai-harness/internal/workflow"
)

// Cycle phases. The flow only advances in this order.
const (
	PhaseEmpty           = "no_cycle"
	PhaseAnalyzed        = "analyzed"   // the PO produced insights and a proposal
	PhaseSufficient      = "sufficient" // the PO concluded that nothing needs to change
	PhaseNoChange        = "no_change_needed"
	PhaseNeedsEvidence   = "needs_evidence"
	PhaseOutOfScope      = "out_of_scope"
	PhaseBlockedBaseline = "blocked_by_baseline"
	PhaseDiscussed       = "discussed" // the team weighed in and the PO consolidated the plan
	PhaseWaiting         = "awaiting_approval"
	PhaseApproved        = "approved"
	PhaseRejected        = "rejected"
	PhaseExecuted        = "executed"
)

// Task is an assignment from the Product Owner to a team member.
type Task struct {
	ID          string   `json:"id"`
	Owner       string   `json:"owner"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Criteria    []string `json:"criteria"`
	DependsOn   []string `json:"depends_on"`
	Status      string   `json:"status"` // pending | done
	Deliverable string   `json:"deliverable,omitempty"`
}

// Event leaves a trace of what happened, for auditing.
type Event struct {
	When time.Time `json:"when"`
	What string    `json:"what"`
	Who  string    `json:"who,omitempty"`
	Note string    `json:"note,omitempty"`
}

// State is what lives in cycles/NNN/state.json.
type State struct {
	SchemaVersion string             `json:"schema_version,omitempty"`
	BaseCommit    string             `json:"base_commit,omitempty"`
	Intake        *workflow.Intake   `json:"intake,omitempty"`
	Scope         *workflow.Scope    `json:"scope,omitempty"`
	Proposal      *workflow.Proposal `json:"proposal,omitempty"`
	Plan          *workflow.Proposal `json:"plan,omitempty"`
	Cycle         int                `json:"cycle"`
	Phase         string             `json:"phase"`
	Verdict       string             `json:"verdict,omitempty"`
	PlanHash      string             `json:"plan_hash,omitempty"`
	ScopeHash     string             `json:"scope_hash,omitempty"`
	BaselineHash  string             `json:"baseline_hash,omitempty"`
	Approval      *ApprovalBinding   `json:"approval,omitempty"`
	Created       time.Time          `json:"created"`
	Updated       time.Time          `json:"updated"`
	Tasks         []Task             `json:"tasks"`
	History       []Event            `json:"history"`
}

// ApprovalBinding records the exact inputs a human approved. A plan or scope
// edit invalidates the binding before execution can resume.
type ApprovalBinding struct {
	Actor        string    `json:"actor"`
	PlanHash     string    `json:"plan_hash"`
	ScopeHash    string    `json:"scope_hash"`
	BaselineHash string    `json:"baseline_hash"`
	ApprovedAt   time.Time `json:"approved_at"`
}

// Workspace points to the working directory.
type Workspace struct{ Root string }

// Open returns a Workspace over the given path.
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

// Prompts, context and interviews.
func (w *Workspace) PromptPath(rel string) string { return w.path(rel) }
func (w *Workspace) ContextDir() string           { return w.path("context") }
func (w *Workspace) InterviewsDir() string        { return w.path("interviews") }
func (w *Workspace) CyclesDir() string            { return w.path("cycles") }

// CycleDir returns cycles/NNN.
func (w *Workspace) CycleDir(n int) string {
	return filepath.Join(w.CyclesDir(), fmt.Sprintf("%03d", n))
}

// LastCycle returns the highest cycle number, or 0 if there are none.
func (w *Workspace) LastCycle() int {
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

// NewCycle creates the directory for the next cycle and its initial state.
func (w *Workspace) NewCycle() (*State, error) {
	n := w.LastCycle() + 1
	dir := w.CycleDir(n)
	for _, sub := range []string{"", "deliverables"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	st := &State{Cycle: n, Phase: PhaseEmpty, Created: now, Updated: now}
	st.Log("cycle created", "", "")
	return st, w.SaveState(st)
}

// LoadState reads the state of the last cycle.
func (w *Workspace) LoadState() (*State, error) {
	n := w.LastCycle()
	if n == 0 {
		return nil, fmt.Errorf("there's no cycle yet; start with 'yanai analyze <interview-file>'")
	}
	return w.LoadCycleState(n)
}

// LoadCycleState reads the state of a specific cycle.
func (w *Workspace) LoadCycleState(n int) (*State, error) {
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

// SaveState persists the cycle state.
func (w *Workspace) SaveState(st *State) error {
	st.Updated = time.Now()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.CycleDir(st.Cycle), "state.json"), b, 0o600)
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
