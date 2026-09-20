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
	if st.Phase == ws.PhaseApproved {
		// Idempotent replay: re-running 'yanai approve' against a cycle
		// already approved under this exact plan/scope/baseline is a no-op,
		// not an error -- a script retried after a transient failure
		// shouldn't need the human to approve twice. A *different* plan
		// reaching this phase should be unreachable (Discuss only ever
		// leaves awaiting_approval or rejected), but this never trusts that
		// silently.
		if st.Approval != nil && st.Approval.PlanHash == st.PlanHash && st.Approval.ScopeHash == st.ScopeHash && st.Approval.BaselineHash == st.BaselineHash {
			return st, nil
		}
		return nil, fmt.Errorf("cycle %03d is already approved under a different plan, scope or baseline", st.Cycle)
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
	approvedAt := time.Now().UTC()
	st.Phase = ws.PhaseApproved
	st.Approval = &ws.ApprovalBinding{Actor: "human", PlanHash: st.PlanHash, ScopeHash: st.ScopeHash, BaselineHash: st.BaselineHash, ApprovedAt: approvedAt}
	st.Log("plan APPROVED by the human", "human", note)
	text := fmt.Sprintf("# Aprobación\n\nEstado: APROBADO\nNota: %s\n", optional(note))
	if _, err := r.Workspace.WriteDocument(st.Cycle, "05-aprobacion.md", text); err != nil {
		return nil, err
	}
	if r.Workspace.Store != nil {
		approval := workflow.Approval{
			ID: fmt.Sprintf("A-%03d-%d", st.Cycle, approvedAt.UnixNano()), Cycle: st.Cycle, Actor: "human",
			PlanHash: st.PlanHash, ScopeHash: st.ScopeHash, Baseline: st.BaselineHash, ApprovedAt: approvedAt,
		}
		if err := r.Workspace.Store.RecordApproval(approval); err != nil {
			return nil, err
		}
	}
	return st, r.Workspace.SaveState(st, workflow.ActorHuman)
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
	return st, r.Workspace.SaveState(st, workflow.ActorHuman)
}

// isTaskDone reports whether a task's status is a final, staged outcome that
// a fresh run should skip. "done" is kept only so a bare Workspace (Store ==
// nil, as in a direct package test) still behaves as it always has; the real
// CLI path always has a store, whose ticket never reaches "done" at all —
// see internal/workflow/transitions.go.
func isTaskDone(status string) bool {
	return status == workflow.TicketCandidateReady || status == "done"
}

// Execute runs the approved tasks, in dependency order. With a store
// attached, each task's model call is a claimed, attempted, conditionally-
// versioned ticket transition — pending -> claimed -> response_recorded ->
// candidate_ready (or response_rejected on a validation failure) — rather
// than an in-memory "done" flag: see internal/workflow/transitions.go for
// why "done" is gone entirely, and checkUnresolvedAttempts for why an
// interrupted prior model call blocks this rather than silently retrying.
func (r *Runner) Execute(ctx context.Context, onlyID string, retryUnresolved bool) (*ws.State, error) {
	if os.Getenv("YANAI_NO_REPO") == "1" {
		return nil, fmt.Errorf("run requires repository context; unset YANAI_NO_REPO")
	}
	if err := r.checkUnresolvedAttempts(retryUnresolved); err != nil {
		return nil, err
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
	if st.Phase != ws.PhaseApproved && st.Phase != ws.PhaseAwaitingExecution {
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

	store := r.Workspace.Store
	var holder string
	claimTTL := 15 * time.Minute
	if store != nil {
		host, _ := os.Hostname()
		holder = fmt.Sprintf("%s/%d", host, os.Getpid())
		if r.Cfg != nil && r.Cfg.OpenRouter.TimeoutSec > 0 {
			if t := 4 * time.Duration(r.Cfg.OpenRouter.TimeoutSec) * time.Second; t > claimTTL {
				claimTTL = t
			}
		}
		// st.Tasks was fixed at Discuss time (always "pending"); the store
		// is what actually knows what a previous run already finished,
		// rejected, or left claimed.
		for i := range st.Tasks {
			rec, err := store.GetTicket(st.Cycle, st.Tasks[i].ID)
			if err != nil {
				return st, err
			}
			st.Tasks[i].Status = rec.Status
		}
	}

	done := map[string]bool{}
	for _, t := range st.Tasks {
		if isTaskDone(t.Status) {
			done[t.ID] = true
		}
	}

	for i := range st.Tasks {
		t := &st.Tasks[i]
		if onlyID != "" && !strings.EqualFold(t.ID, onlyID) {
			continue
		}
		if isTaskDone(t.Status) {
			continue
		}
		for _, d := range t.DependsOn {
			if !done[d] {
				return st, fmt.Errorf("task %s depends on %s, which isn't done yet", t.ID, d)
			}
		}

		if store != nil {
			if err := r.claimForRetry(store, st.Cycle, t.ID, holder, claimTTL); err != nil {
				return st, fmt.Errorf("task %s: %w", t.ID, err)
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

		var attemptID string
		if store != nil {
			attemptID, err = store.BeginAttempt(workflow.AttemptInput{Cycle: st.Cycle, TicketID: t.ID, Role: t.Owner, Kind: "role_turn", RequestHash: contentHash(m.String())})
			if err != nil {
				return st, err
			}
		}
		resp, runErr := r.Run(ctx, t.Owner, m.String())
		if store != nil {
			if runErr != nil {
				_ = store.FailAttempt(attemptID, runErr.Error())
			} else if err := store.CompleteAttempt(attemptID, contentHash(resp), workflow.Usage{}); err != nil {
				return st, err
			}
		}
		if runErr != nil {
			if store != nil {
				_ = store.ReleaseClaim(st.Cycle, t.ID, holder)
			}
			return st, runErr
		}

		if store != nil {
			rec, err := store.GetTicket(st.Cycle, t.ID)
			if err != nil {
				return st, err
			}
			if _, err := store.ApplyTicketStatus(st.Cycle, t.ID, workflow.TicketResponseRecorded, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "response.recorded"}); err != nil {
				return st, err
			}
		}

		// Validate against both the repository policy and the structured ticket.
		var contract workflow.Ticket
		for _, ticket := range st.Plan.Tickets {
			if ticket.ID == t.ID {
				contract = ticket
			}
		}
		// Validate raw paths before the legacy extractor can normalize them.
		validationErr := validateTaskOutput(resp, contract, target)
		if validationErr == nil {
			current, err := target.Snapshot()
			if err != nil {
				return st, err
			}
			if current.Baseline() != baseline.Baseline() {
				validationErr = fmt.Errorf("repository changed during run; refusing to save stale deliverables")
			}
		}
		if validationErr != nil {
			if store != nil {
				if rec, err := store.GetTicket(st.Cycle, t.ID); err == nil {
					_, _ = store.ApplyTicketStatus(st.Cycle, t.ID, workflow.TicketResponseRejected, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "response.rejected", Payload: validationErr.Error()})
				}
				_ = store.ReleaseClaim(st.Cycle, t.ID, holder)
			}
			return st, validationErr
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
		if store != nil {
			rec, err := store.GetTicket(st.Cycle, t.ID)
			if err != nil {
				return st, err
			}
			rec, err = store.ApplyTicketStatus(st.Cycle, t.ID, workflow.TicketCandidateReady, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "candidate.ready"})
			if err != nil {
				return st, err
			}
			t.Status = rec.Status
			_ = store.ReleaseClaim(st.Cycle, t.ID, holder)
		} else {
			t.Status = "done"
		}
		t.Deliverable = filepath.ToSlash(dir)
		done[t.ID] = true
		st.Log("task executed: "+t.ID, t.Owner, fmt.Sprintf("%d files", len(files)))
		fmt.Fprintf(os.Stderr, "  %s → %d file(s) in %s\n", t.ID, len(files), filepath.Join(r.Workspace.CycleDir(st.Cycle), dir))
		if err := r.Workspace.SaveState(st, workflow.ActorEngine); err != nil {
			return st, err
		}
	}

	allDone := true
	for _, t := range st.Tasks {
		if !isTaskDone(t.Status) {
			allDone = false
		}
	}
	if allDone {
		st.Phase = ws.PhaseAwaitingExecution
		st.Log("all tasks staged", "", "")
	}
	return st, r.Workspace.SaveState(st, workflow.ActorEngine)
}

// validateTaskOutput checks every "=== ARCHIVO: path ===" block's raw path
// against the ticket's declared outputs and the repository's path policy,
// before the legacy extractor (ExtractFiles) can normalize anything —
// a path this rejects must never reach the filesystem at all.
func validateTaskOutput(resp string, contract workflow.Ticket, target *repository.Target) error {
	for _, match := range reFile.FindAllStringSubmatch(resp, -1) {
		expected := false
		for _, output := range contract.Outputs {
			if match[1] == output {
				expected = true
			}
		}
		if !expected {
			return fmt.Errorf("task %s output: path is not a declared ticket output", contract.ID)
		}
		if err := target.CheckPath(match[1]); err != nil {
			return fmt.Errorf("task %s output: %w", contract.ID, err)
		}
	}
	return nil
}

// claimForRetry acquires ticketID's claim, first returning a
// previously-rejected ticket to pending so a new run can retry it —
// response_rejected has no direct claim path (only pending does), by
// design: nothing else in the transition table skips the pending state.
func (r *Runner) claimForRetry(store *workflow.Store, cycle int, ticketID, holder string, ttl time.Duration) error {
	rec, err := store.GetTicket(cycle, ticketID)
	if err != nil {
		return err
	}
	if rec.Status == workflow.TicketResponseRejected {
		rec, err = store.ApplyTicketStatus(cycle, ticketID, workflow.TicketPending, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "retry"})
		if err != nil {
			return err
		}
	}
	_, _, err = store.ClaimTicket(cycle, ticketID, holder, ttl, workflow.Event{})
	return err
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
