package team

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/yanai/yanai-harness/internal/workflow"
	"os"
	"reflect"
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repoctx"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/ws"
)

func validateTasks(ts []ws.Task) error {
	valid := map[string]bool{config.RoleArchitect: true, config.RoleDesigner: true, config.RoleEngineer: true}
	return validateTaskGraph(ts, valid)
}

func validateTasksForRoles(ts []ws.Task, configured map[string]config.Agent) error {
	valid := map[string]bool{}
	for id := range configured {
		valid[id] = true
	}
	return validateTaskGraph(ts, valid)
}

func validateTaskGraph(ts []ws.Task, valid map[string]bool) error {
	ids := map[string]bool{}
	for _, t := range ts {
		if t.ID == "" || ids[t.ID] {
			return fmt.Errorf("task IDs must be unique and non-empty: %q", t.ID)
		}
		ids[t.ID] = true
	}
	for _, t := range ts {
		if !valid[t.Owner] {
			return fmt.Errorf("task %s has an invalid owner: %q", t.ID, t.Owner)
		}
		if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Description) == "" || len(t.Criteria) == 0 {
			return fmt.Errorf("task %s requires a title, description, and acceptance criteria", t.ID)
		}
		for _, d := range t.DependsOn {
			if !ids[d] {
				return fmt.Errorf("task %s depends on %s, which doesn't exist", t.ID, d)
			}
		}
	}
	state := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("task dependency cycle includes %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, t := range ts {
			if t.ID == id {
				for _, dep := range t.DependsOn {
					if dep == id {
						return fmt.Errorf("task %s depends on itself", id)
					}
					if err := visit(dep); err != nil {
						return err
					}
				}
				break
			}
		}
		state[id] = 2
		return nil
	}
	for id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

// Reject sends the plan back to the team with the human's reason.
func (r *Runner) Reject(note string) (*ws.State, error) {
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	if st.Phase != ws.PhaseWaiting && !(st.Markdown != nil && st.Phase == ws.PhaseAnalyzed) {
		return nil, fmt.Errorf("cycle %03d is in phase %q; only a plan in %q can be rejected", st.Cycle, st.Phase, ws.PhaseWaiting)
	}
	if strings.TrimSpace(note) == "" {
		return nil, fmt.Errorf("a rejection needs a reason: use --note \"...\"")
	}
	st.Phase = ws.PhaseRejected
	st.Tasks = nil
	st.Log("plan REJECTED by the human", "human", note)
	text := fmt.Sprintf("# Aprobación\n\nEstado: RECHAZADO\nMotivo:\n\n%s\n", note)
	if _, err := r.Workspace.WriteDocument(st.Cycle, "05-aprobacion.md", text); err != nil {
		return nil, err
	}
	return st, r.Workspace.SaveState(st, workflow.ActorHuman)
}

// repoIndex returns the compressed repository map (tree + SPEC.md),
// without code. This is what 'yanai context' shows and what
// kicks off every two-pass selection.
func (r *Runner) repoIndex() (string, error) {
	if os.Getenv("YANAI_NO_REPO") == "1" {
		return "_(omitido con YANAI_NO_REPO=1)_", nil
	}
	return repoctx.Index(r.Cfg.Repo)
}

// repoContextFor builds the repository context for an agent in two passes:
// first it shows the Index and asks which files it needs for the given task
// (format "NECESITO: path"), then it builds the block with the full content
// of those paths. This replaces dumping the whole repo: a fixed byte cap
// doesn't scale as the code grows, but a SPEC.md map plus a targeted
// selection does.
func (r *Runner) repoContextFor(ctx context.Context, role, task string) (string, error) {
	paths, err := r.selectFiles(ctx, role, task)
	if err != nil || paths == nil {
		if err != nil {
			return "", err
		}
		return "_(omitido con YANAI_NO_REPO=1)_", nil
	}
	return repoctx.Files(r.Cfg.Repo, paths)
}

// selectFiles is the first pass on its own: it returns the paths the agent
// asked for, capped by configuration. Execution uses it directly, because it
// reads those files completely through the executor rather than through
// repoctx's byte-capped renderer — an execution context that silently drops
// the tail of a file is what makes a model invent it back.
func (r *Runner) selectFiles(ctx context.Context, role, task string) ([]string, error) {
	if os.Getenv("YANAI_NO_REPO") == "1" {
		return nil, nil
	}
	index, err := repoctx.Index(r.Cfg.Repo)
	if err != nil {
		return nil, err
	}

	var m strings.Builder
	m.WriteString("SELECCIONA_ARCHIVOS\n\n")
	m.WriteString("# Índice del repositorio (estructura + SPEC.md)\n\n")
	m.WriteString(index)
	m.WriteString("\n\n# Qué vas a hacer\n\n")
	m.WriteString(task)
	m.WriteString("\n\n# Lo que tienes que responder\n\n")
	fmt.Fprintf(&m, `Antes de trabajar, dinos qué archivos de código necesitas leer completos.
Guíate por los SPEC.md de arriba: cada uno te dice qué archivos viven en esa
carpeta y qué hace cada uno.

Responde solo con líneas en este formato exacto, una por archivo, nada más:
NECESITO: ruta/relativa/al/archivo

Pide como máximo %d archivos, los mínimos indispensables para hacer bien tu
parte. Si de verdad no necesitas ver código todavía, responde exactamente:
NECESITO: -
Si tienes cualquier observación, responde OBSERVATION: <pregunta para el humano> y no selecciones archivos.`, r.Cfg.Repo.MaxSelectedFiles)

	resp, err := r.Run(ctx, role, m.String())
	if err != nil {
		return nil, err
	}

	for _, line := range strings.Split(resp, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "OBSERVATION:") {
			question := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "OBSERVATION:"))
			detail := workflow.ObservationDetail{Description: question, Requirement: "task", Question: question}
			if err := r.Workspace.Store.RecordObservations(r.cycle, role, workflow.Digest(resp), []workflow.ObservationDetail{detail}); err != nil {
				return nil, err
			}
			return nil, r.Workspace.Store.RequireResolved(r.cycle)
		}
	}
	paths := ParseNeeds(resp)
	if len(paths) > r.Cfg.Repo.MaxSelectedFiles {
		paths = paths[:r.Cfg.Repo.MaxSelectedFiles]
	}
	if paths == nil {
		paths = []string{}
	}
	return paths, nil
}

func optional(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(sin nota)"
	}
	return s
}

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:])
}

// sameTaskProjection compares a persisted task list against the projection its
// plan would produce now. It exists instead of a bare reflect.DeepEqual because
// "no dependencies" does not round-trip symmetrically: workflow.Ticket.DependsOn
// carries omitempty, so an empty list is dropped on write and read back as nil,
// while ws.Task.DependsOn has no omitempty and is read back as an empty slice.
// A model that emits "depends_on": [] rather than omitting the key therefore
// produced a projection that could never match itself, wedging every cycle whose
// tickets have no dependencies. Both spellings mean the same thing, so they
// compare equal here.
func sameTaskProjection(actual, expected []ws.Task) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i := range actual {
		a, e := actual[i], expected[i]
		if len(a.DependsOn) == 0 {
			a.DependsOn = nil
		}
		if len(e.DependsOn) == 0 {
			e.DependsOn = nil
		}
		if !reflect.DeepEqual(a, e) {
			return false
		}
	}
	return true
}

// validateCurrentPlan makes structured data authoritative; Markdown/tasks are
// projections, not an alternative way to authorize executable instructions.
func (r *Runner) validateCurrentPlan(st *ws.State) error {
	if st.Markdown != nil {
		return r.validateMarkdownPlan(st)
	}

	if st.SchemaVersion != "1" || st.Plan == nil || st.Intake == nil || st.Scope == nil {
		return fmt.Errorf("legacy cycle is read-only; re-import its input with analyze --privacy-reviewed")
	}
	if r.Cfg == nil {
		cfg, err := config.Load(r.Workspace.Root)
		if err != nil {
			return err
		}
		r.Cfg = cfg
	}
	target, err := repository.Open(r.Cfg.Repo, r.Workspace.Root)
	if err != nil {
		return err
	}
	c, err := r.decisionContext(st)
	if err != nil {
		return err
	}
	if err := workflow.ValidateDecision(*st.Plan, c, r.roles(), target.CheckPath); err != nil {
		return err
	}
	hash, err := workflow.Hash(st.Plan)
	if err != nil {
		return err
	}
	if st.PlanHash != hash || r.Workspace.ReadDocument(st.Cycle, "04-plan.md") != workflow.RenderProposal(*st.Plan) {
		return fmt.Errorf("structured plan or review projection changed; discuss again")
	}
	expected := tasksForProposal(*st.Plan)
	actual := append([]ws.Task(nil), st.Tasks...)
	for i := range actual {
		actual[i].Status = "pending"
		actual[i].Deliverable = ""
	}
	if !sameTaskProjection(actual, expected) {
		return fmt.Errorf("task projection changed; discuss again")
	}
	if st.ScopeHash != contentHash(r.Workspace.ReadContext()) {
		return fmt.Errorf("project context changed; analyze again")
	}
	return nil
}
