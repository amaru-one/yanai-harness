package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repoctx"
	"github.com/yanai/yanai-harness/internal/ws"
)

// Analyze opens a new cycle: the PO reads the interviews and decides
// whether a new plan is needed or the app already covers what teachers need.
func (r *Runner) Analyze(ctx context.Context, interviews string) (*ws.State, error) {
	st, err := r.Workspace.NewCycle()
	if err != nil {
		return nil, err
	}

	if _, err := r.Workspace.WriteDocument(st.Cycle, "00-entrada.md", interviews); err != nil {
		return nil, err
	}

	repo, err := r.repoContextFor(ctx, config.RolePO,
		"Vas a leer notas de entrevistas a docentes y decidir si el producto actual "+
			"ya las cubre (SUFICIENTE) o si hace falta un plan nuevo (NUEVO_PLAN). "+
			"Necesitas ver el código de las funcionalidades que la entrevista toca, "+
			"para no proponer algo que ya existe.\n\n# Entrevistas\n\n"+interviews)
	if err != nil {
		return nil, err
	}

	var m strings.Builder
	m.WriteString("ANALIZA LAS ENTREVISTAS\n\n")
	m.WriteString("# 1. Alcance y estado del producto\n\n")
	m.WriteString(r.Workspace.ReadContext())
	m.WriteString("\n\n# 2. Ciclos anteriores\n\n")
	m.WriteString(r.Workspace.CycleHistory(st.Cycle))
	m.WriteString("\n\n# 3. Código actual de la aplicación docente\n\n")
	m.WriteString(repo)
	m.WriteString("\n\n# 4. Notas de las entrevistas presenciales\n\n")
	m.WriteString(interviews)
	m.WriteString("\n\n# 5. Lo que tienes que entregar\n\n")
	m.WriteString(`Escribe un documento en markdown con estas secciones, en este orden:

## Insights
Hallazgos concretos extraídos de las entrevistas. Cada uno citando la evidencia
textual que lo sustenta. Separa lo que un docente dijo de lo que tú infieres.

## Validación de funcionalidades existentes
Para cada funcionalidad actual de la app: la entrevista la confirma, la
contradice, o no dice nada.

## Fuera de alcance
Lista las peticiones que rechazas por estar fuera del alcance definido, con el
motivo. Si una petición es valiosa pero no ahora, ponla aquí como "postergada".

## Propuesta
Si corresponde un plan nuevo: describe QUÉ se va a construir y POR QUÉ, ligado a
los insights. Si NO corresponde: explica por qué la app ya es suficiente para
los docentes entrevistados.

Termina el documento con exactamente una línea:
VEREDICTO: NUEVO_PLAN
o bien
VEREDICTO: SUFICIENTE
`)

	text, err := r.Run(ctx, config.RolePO, m.String())
	if err != nil {
		return nil, err
	}

	v := Verdict(text)
	if v == "" {
		v = "NUEVO_PLAN"
		text += "\n\n_(El PO no emitió veredicto explícito; se asume NUEVO_PLAN.)_\n"
	}
	st.Verdict = v

	if v == "SUFICIENTE" {
		if _, err := r.Workspace.WriteDocument(st.Cycle, "02-reporte-suficiencia.md", text); err != nil {
			return nil, err
		}
		st.Phase = ws.PhaseSufficient
		st.Log("analysis: the app is sufficient", config.RolePO, "")
	} else {
		if _, err := r.Workspace.WriteDocument(st.Cycle, "02-propuesta.md", text); err != nil {
			return nil, err
		}
		st.Phase = ws.PhaseAnalyzed
		st.Log("analysis: proposing a new plan", config.RolePO, "")
	}
	return st, r.Workspace.SaveState(st)
}

// Discuss runs the working session: each specialist weighs in on the
// proposal after seeing what the others said, and the PO consolidates the plan.
func (r *Runner) Discuss(ctx context.Context) (*ws.State, error) {
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	if st.Phase == ws.PhaseSufficient {
		return nil, fmt.Errorf("cycle %03d closed with verdict SUFICIENTE: there's nothing to discuss", st.Cycle)
	}
	if st.Phase != ws.PhaseAnalyzed && st.Phase != ws.PhaseRejected {
		return nil, fmt.Errorf("cycle %03d is in phase %q; 'discuss' only applies after 'analyze' or after a rejection", st.Cycle, st.Phase)
	}

	proposal := r.Workspace.ReadDocument(st.Cycle, "02-propuesta.md")
	if proposal == "" {
		return nil, fmt.Errorf("02-propuesta.md was not found in cycle %03d", st.Cycle)
	}
	context_ := r.Workspace.ReadContext()

	var transcript strings.Builder
	transcript.WriteString(fmt.Sprintf("# Discusión del ciclo %03d\n\n", st.Cycle))

	// If we're coming from a rejection, the team needs to keep it in mind.
	rejection := r.Workspace.ReadDocument(st.Cycle, "05-aprobacion.md")

	for _, role := range r.Cfg.DiscussionOrder {
		ag, err := r.Cfg.Agent(role)
		if err != nil {
			return nil, err
		}
		repo, err := r.repoContextFor(ctx, role,
			"Vas a opinar sobre esta propuesta del Product Owner desde tu rol. "+
				"Necesitas ver el código que tu parte del trabajo tocaría.\n\n"+
				"# Propuesta\n\n"+proposal)
		if err != nil {
			return nil, err
		}
		var m strings.Builder
		m.WriteString("REVISA LA PROPUESTA\n\n")
		m.WriteString("# Alcance y estado del producto\n\n" + context_)
		m.WriteString("\n\n# Código actual\n\n" + repo)
		m.WriteString("\n\n# Propuesta del Product Owner\n\n" + proposal)
		if rejection != "" {
			m.WriteString("\n\n# El humano rechazó la versión anterior de este plan\n\n" + rejection)
		}
		m.WriteString("\n\n# Lo que han dicho tus compañeros hasta ahora\n\n" + transcript.String())
		m.WriteString("\n\n# Lo que tienes que entregar\n\n")
		m.WriteString(`Responde en markdown, corto y concreto, con estas secciones:

## Riesgos y objeciones
Lo que va a fallar si se construye así. Sé específico; no des generalidades.

## Qué necesito de los demás
Decisiones o entregables que necesitas de otro miembro del equipo para poder
avanzar, nombrando el rol.

## Mi parte del trabajo
Qué harías tú, desglosado en piezas de trabajo del tamaño de un día o menos.

## Preguntas abiertas
Lo que nadie ha respondido todavía y bloquea el diseño.

No escribas código ni maquetas todavía: esto es la discusión previa.`)

		resp, err := r.Run(ctx, role, m.String())
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&transcript, "## %s (%s)\n\n%s\n\n---\n\n", ag.Name, role, resp)
	}

	// The PO consolidates.
	var mp strings.Builder
	mp.WriteString("CONSOLIDA EL PLAN\n\n")
	mp.WriteString("# Alcance y estado del producto\n\n" + context_)
	mp.WriteString("\n\n# Tu propuesta original\n\n" + proposal)
	if rejection != "" {
		mp.WriteString("\n\n# El humano rechazó la versión anterior\n\n" + rejection)
	}
	mp.WriteString("\n\n# Discusión del equipo\n\n" + transcript.String())
	mp.WriteString("\n\n# Lo que tienes que entregar\n\n")
	mp.WriteString(`Escribe el plan de implementación en markdown:

## Decisión
Qué se construye en este ciclo, en dos o tres frases.

## Conflictos resueltos
Dónde el equipo no estuvo de acuerdo y cómo lo resolviste. Di quién cede y por qué.

## Recortes de alcance
Qué dejaste fuera de este ciclo aunque se haya propuesto.

## Riesgos aceptados
Los riesgos que el equipo señaló y que decides asumir igual.

## Preguntas para el humano
Lo que necesita decidir una persona antes de implementar. Si no hay, escribe "Ninguna".

## Tareas
Un bloque por tarea, con este formato exacto y nada más entre los campos:

### TAREA: T-001
RESPONSABLE: arquitecto-bd
TITULO: <una línea>
DESCRIPCION: <una o dos líneas>
CRITERIOS:
- <criterio de aceptación verificable>
- <otro>
DEPENDE_DE: -

Usa IDs correlativos T-001, T-002, ... El campo RESPONSABLE solo puede ser uno de:
arquitecto-bd, disenador, ingeniero. En DEPENDE_DE pon "-" si no depende de nada,
o una lista de IDs separados por coma. Ordena las tareas por dependencia.`)

	plan, err := r.Run(ctx, config.RolePO, mp.String())
	if err != nil {
		return nil, err
	}

	if _, err := r.Workspace.WriteDocument(st.Cycle, "03-discusion.md", transcript.String()); err != nil {
		return nil, err
	}
	if _, err := r.Workspace.WriteDocument(st.Cycle, "04-plan.md", plan); err != nil {
		return nil, err
	}

	tasks := ParseTasks(plan)
	if len(tasks) == 0 {
		return nil, fmt.Errorf("the PO didn't produce tasks in the expected format; check %s", filepath.Join(r.Workspace.CycleDir(st.Cycle), "04-plan.md"))
	}
	if err := validateTasks(tasks); err != nil {
		return nil, err
	}
	st.Tasks = tasks
	st.Phase = ws.PhaseWaiting
	st.Log("plan consolidated, awaiting human approval", config.RolePO, fmt.Sprintf("%d tasks", len(tasks)))
	return st, r.Workspace.SaveState(st)
}

func validateTasks(ts []ws.Task) error {
	valid := map[string]bool{config.RoleArchitect: true, config.RoleDesigner: true, config.RoleEngineer: true}
	ids := map[string]bool{}
	for _, t := range ts {
		ids[t.ID] = true
	}
	for _, t := range ts {
		if !valid[t.Owner] {
			return fmt.Errorf("task %s has an invalid owner: %q", t.ID, t.Owner)
		}
		for _, d := range t.DependsOn {
			if !ids[d] {
				return fmt.Errorf("task %s depends on %s, which doesn't exist", t.ID, d)
			}
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
	st.Phase = ws.PhaseApproved
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
	st, err := r.Workspace.LoadState()
	if err != nil {
		return nil, err
	}
	if st.Phase != ws.PhaseApproved && st.Phase != ws.PhaseExecuted {
		return nil, fmt.Errorf("cycle %03d is in phase %q: nothing runs without the human's approval ('yanai approve')", st.Cycle, st.Phase)
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

Las rutas son relativas y describen dónde debería vivir el archivo en el
repositorio. No escribas rutas absolutas ni "..". Entrega archivos completos,
nunca fragmentos con "resto igual".`)

		resp, err := r.Run(ctx, t.Owner, m.String())
		if err != nil {
			return st, err
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

// repoIndex returns the compressed repository map (tree + SPEC.md/
// CLAUDE.md), without code. This is what 'yanai context' shows and what
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
	m.WriteString("# Índice del repositorio (estructura + SPEC.md/CLAUDE.md)\n\n")
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
