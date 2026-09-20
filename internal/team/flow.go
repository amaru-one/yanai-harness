package team

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/yanai/yanai-harness/internal/workflow"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

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

// Approve opens the human gate.
func (r *Runner) Approve(note string) (*ws.State, error) {
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	if st.Phase != ws.PhaseWaiting {
		return nil, fmt.Errorf("cycle %03d is in phase %q; only a plan in %q can be approved", st.Cycle, st.Phase, ws.PhaseWaiting)
	}
	// Validate before mutating: an approval that fails its own checks must
	// leave the cycle exactly as it was.
	if err := r.validateCurrentPlan(st); err != nil {
		return nil, err
	}
	plan := r.Workspace.ReadDocument(st.Cycle, "04-plan.md")
	if plan == "" || st.PlanHash == "" || plan != workflow.RenderProposal(*st.Plan) || st.ScopeHash == "" || st.BaselineHash == "" {
		return nil, fmt.Errorf("approval inputs are missing or changed; regenerate the plan before approval")
	}
	st.Phase = ws.PhaseApproved
	st.Approval = &ws.ApprovalBinding{Actor: "human", PlanHash: st.PlanHash, ScopeHash: st.ScopeHash, BaselineHash: st.BaselineHash, ApprovedAt: time.Now().UTC()}
	st.Log("plan APPROVED by the human", "human", note)
	text := fmt.Sprintf("# Aprobación\n\nEstado: APROBADO\nNota: %s\n", optional(note))
	if _, err := r.Workspace.WriteDocument(st.Cycle, "05-aprobacion.md", text); err != nil {
		return nil, err
	}
	return st, r.Workspace.SaveState(st)
}

// Reject sends the plan back to the team with the human's reason.
func (r *Runner) Reject(note string) (*ws.State, error) {
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	if st.Phase != ws.PhaseWaiting {
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
	return st, r.Workspace.SaveState(st)
}

// Execute runs the approved tasks, in dependency order.
func (r *Runner) Execute(ctx context.Context, onlyID string) (*ws.State, error) {
	if os.Getenv("YANAI_NO_REPO") == "1" {
		return nil, fmt.Errorf("run requires repository context; unset YANAI_NO_REPO")
	}
	target, err := repository.Open(r.Cfg.Repo, r.Workspace.Root)
	if err != nil {
		return nil, err
	}
	unlock, err := target.LockWriter()
	if err != nil {
		return nil, err
	}
	defer unlock()
	baseline, err := target.Snapshot()
	if err != nil {
		return nil, err
	}
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	if st.Phase != ws.PhaseApproved && st.Phase != ws.PhaseExecuted {
		return nil, fmt.Errorf("cycle %03d is in phase %q: nothing runs without the human's approval ('yanai approve')", st.Cycle, st.Phase)
	}
	if err := r.validateCurrentPlan(st); err != nil {
		return st, err
	}
	r.Redactions = st.Intake.Redactions
	if st.Approval == nil || st.Approval.PlanHash != st.PlanHash || st.Approval.ScopeHash != contentHash(r.Workspace.ReadContext()) || st.Approval.BaselineHash != baseline.Baseline() {
		return st, fmt.Errorf("approval is stale or incomplete; plan, scope, or repository baseline changed")
	}

	plan := r.Workspace.ReadDocument(st.Cycle, "04-plan.md")
	discussion := r.Workspace.ReadDocument(st.Cycle, "03-discusion.md")
	context_ := r.Workspace.ReadContext()

	done := map[string]bool{}
	for _, t := range st.Tasks {
		if t.Status == "done" {
			done[t.ID] = true
		}
	}

	for i := range st.Tasks {
		t := &st.Tasks[i]
		if onlyID != "" && !strings.EqualFold(t.ID, onlyID) {
			continue
		}
		if t.Status == "done" {
			continue
		}
		for _, d := range t.DependsOn {
			if !done[d] {
				return st, fmt.Errorf("task %s depends on %s, which isn't done yet", t.ID, d)
			}
		}

		var taskDesc strings.Builder
		fmt.Fprintf(&taskDesc, "Vas a ejecutar la tarea %s del plan aprobado.\n\nTítulo: %s\nDescripción: %s\nCriterios de aceptación:\n", t.ID, t.Title, t.Description)
		for _, c := range t.Criteria {
			fmt.Fprintf(&taskDesc, "- %s\n", c)
		}
		repo, err := r.repoContextFor(ctx, t.Owner, taskDesc.String())
		if err != nil {
			return st, err
		}

		var m strings.Builder
		m.WriteString("TAREA_DE_EJECUCION\n\n")
		m.WriteString("# Alcance y estado del producto\n\n" + context_)
		m.WriteString("\n\n# Código actual\n\n" + repo)
		m.WriteString("\n\n# Plan aprobado\n\n" + plan)
		m.WriteString("\n\n# Discusión del equipo\n\n" + discussion)
		m.WriteString("\n\n# Entregables ya producidos en este ciclo\n\n" + r.previousDeliverables(st.Cycle))
		fmt.Fprintf(&m, "\n\n# Tu tarea: %s\n\nTítulo: %s\nDescripción: %s\nCriterios de aceptación:\n", t.ID, t.Title, t.Description)
		for _, c := range t.Criteria {
			fmt.Fprintf(&m, "- %s\n", c)
		}
		m.WriteString(`
# Cómo entregar

Explica brevemente tus decisiones y luego entrega los archivos. Cada archivo va
en un bloque con este formato exacto:

=== ARCHIVO: ruta/relativa/del/archivo.ext ===
<contenido completo del archivo, sin cercas de código>
=== FIN ARCHIVO ===

Las rutas son relativas a la raíz Git de Yanai y deben empezar con yanai-server/.
No generes yanai-ui ni archivos del harness. No escribas rutas absolutas ni "..". Entrega archivos completos,
nunca fragmentos con "resto igual".`)

		resp, err := r.Run(ctx, t.Owner, m.String())
		if err != nil {
			return st, err
		}

		// Validate against both the repository policy and the structured ticket.
		var contract workflow.Ticket
		for _, ticket := range st.Plan.Tickets {
			if ticket.ID == t.ID {
				contract = ticket
			}
		}
		// Validate raw paths before the legacy extractor can normalize them.
		for _, match := range reFile.FindAllStringSubmatch(resp, -1) {
			expected := false
			for _, output := range contract.Outputs {
				if match[1] == output {
					expected = true
				}
			}
			if !expected {
				return st, fmt.Errorf("task %s output: path is not a declared ticket output", t.ID)
			}
			if err := target.CheckPath(match[1]); err != nil {
				return st, fmt.Errorf("task %s output: %w", t.ID, err)
			}
		}
		current, err := target.Snapshot()
		if err != nil {
			return st, err
		}
		if current.Baseline() != baseline.Baseline() {
			return st, fmt.Errorf("repository changed during run; refusing to save stale deliverables")
		}
		dir := filepath.Join("entregables", t.Owner, t.ID)
		if _, err := r.Workspace.WriteDocument(st.Cycle, filepath.Join(dir, "respuesta.md"), resp); err != nil {
			return st, err
		}
		files := ExtractFiles(resp)
		for _, a := range files {
			destPath := filepath.Join(dir, "archivos", filepath.FromSlash(a.Path))
			if _, err := r.Workspace.WriteDocument(st.Cycle, destPath, a.Content); err != nil {
				return st, err
			}
		}
		t.Status = "done"
		t.Deliverable = filepath.ToSlash(dir)
		done[t.ID] = true
		st.Log("task executed: "+t.ID, t.Owner, fmt.Sprintf("%d files", len(files)))
		fmt.Fprintf(os.Stderr, "  %s → %d file(s) in %s\n", t.ID, len(files), filepath.Join(r.Workspace.CycleDir(st.Cycle), dir))
		if err := r.Workspace.SaveState(st); err != nil {
			return st, err
		}
	}

	allDone := true
	for _, t := range st.Tasks {
		if t.Status != "done" {
			allDone = false
		}
	}
	if allDone {
		st.Phase = ws.PhaseExecuted
		st.Log("all tasks executed", "", "")
	}
	return st, r.Workspace.SaveState(st)
}

func (r *Runner) previousDeliverables(cycle int) string {
	root := filepath.Join(r.Workspace.CycleDir(cycle), "entregables")
	var b strings.Builder
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(data)
		if len(text) > 12000 {
			text = text[:12000] + "\n… (recortado)"
		}
		fmt.Fprintf(&b, "### %s\n\n```\n%s\n```\n\n", filepath.ToSlash(rel), text)
		return nil
	})
	if b.Len() == 0 {
		return "_(ninguno todavía)_"
	}
	return b.String()
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
	if os.Getenv("YANAI_NO_REPO") == "1" {
		return "_(omitido con YANAI_NO_REPO=1)_", nil
	}
	index, err := repoctx.Index(r.Cfg.Repo)
	if err != nil {
		return "", err
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
NECESITO: -`, r.Cfg.Repo.MaxSelectedFiles)

	resp, err := r.Run(ctx, role, m.String())
	if err != nil {
		return "", err
	}

	paths := ParseNeeds(resp)
	if len(paths) > r.Cfg.Repo.MaxSelectedFiles {
		paths = paths[:r.Cfg.Repo.MaxSelectedFiles]
	}
	return repoctx.Files(r.Cfg.Repo, paths)
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

// validateCurrentPlan makes structured data authoritative; Markdown/tasks are
// projections, not an alternative way to authorize executable instructions.
func (r *Runner) validateCurrentPlan(st *ws.State) error {
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
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("task projection changed; discuss again")
	}
	if st.ScopeHash != contentHash(r.Workspace.ReadContext()) {
		return fmt.Errorf("project context changed; analyze again")
	}
	return nil
}
