package team

import "testing"

const examplePlan = `## Decisión

Construir el registro rápido de logros.

## Tareas

### TAREA: T-001
RESPONSABLE: Arquitecto de BD
TITULO: Modelar competencias y niveles de logro
DESCRIPCION: Diseñar el esquema que soporta la evaluación formativa.
CRITERIOS:
- Diagrama ER en Mermaid
- DDL de PostgreSQL ejecutable
DEPENDE_DE: -

### TAREA: T-002
RESPONSABLE: disenador
TÍTULO: Maqueta del registro por sección
DESCRIPCIÓN: Una pantalla para registrar toda la sección.
CRITERIOS_DE_ACEPTACION:
- HTML autocontenido
- Máximo 3 pasos
DEPENDE_DE: T-001

### TAREA: T-003
RESPONSABLE: Software Engineer (Go / Svelte)
TITULO: Endpoint de registro
DESCRIPCION: API y vista.
CRITERIOS:
- Pruebas incluidas
DEPENDE_DE: T-001, T-002
`

func TestParseTasks(t *testing.T) {
	ts := ParseTasks(examplePlan)
	if len(ts) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(ts))
	}
	if ts[0].Owner != "arquitecto-bd" {
		t.Errorf("T-001 owner = %q", ts[0].Owner)
	}
	if ts[1].Owner != "disenador" {
		t.Errorf("T-002 owner = %q", ts[1].Owner)
	}
	if ts[2].Owner != "ingeniero" {
		t.Errorf("T-003 owner = %q", ts[2].Owner)
	}
	if ts[1].Title != "Maqueta del registro por sección" {
		t.Errorf("T-002 title = %q", ts[1].Title)
	}
	if len(ts[1].Criteria) != 2 {
		t.Errorf("T-002 criteria = %v", ts[1].Criteria)
	}
	if len(ts[0].DependsOn) != 0 {
		t.Errorf("T-001 should not depend on anything: %v", ts[0].DependsOn)
	}
	if len(ts[2].DependsOn) != 2 {
		t.Errorf("T-003 dependencies = %v", ts[2].DependsOn)
	}
	if ts[0].Status != "pending" {
		t.Errorf("initial status = %q", ts[0].Status)
	}
}

func TestVerdict(t *testing.T) {
	cases := map[string]string{
		"bla\nVEREDICTO: NUEVO_PLAN\n":          "NUEVO_PLAN",
		"bla\nveredicto: suficiente\n":          "SUFICIENTE",
		"bla\nveredicto: needs_evidence\n":      "NEEDS_EVIDENCE",
		"bla\nVEREDICTO: PROPOSE_CHANGE\n":      "PROPOSE_CHANGE",
		"bla\nVEREDICTO: NO_CHANGE_NEEDED\n":    "NO_CHANGE_NEEDED",
		"bla\nVEREDICTO: OUT_OF_SCOPE\n":        "OUT_OF_SCOPE",
		"bla\nVEREDICTO: BLOCKED_BY_BASELINE\n": "BLOCKED_BY_BASELINE",
		"no verdict anywhere here":              "",
		"bla\nVEREDICTO: ALGO_INVENTADO\n":      "",
	}
	for input, expected := range cases {
		if got := Verdict(input); got != expected {
			t.Errorf("Verdict(%q) = %q, expected %q", input, got, expected)
		}
	}
}

// A cycle closes without a plan on four distinct findings plus the legacy
// SUFICIENTE. Only NO_CHANGE_NEEDED/SUFICIENTE claims the product is enough,
// so the CLI and the flow must agree on the set — they read it from here.
func TestIsTerminalVerdict(t *testing.T) {
	terminal := []string{
		VerdictNoChangeNeeded, VerdictNeedsEvidence, VerdictOutOfScope,
		VerdictBlockedByBaseline, VerdictLegacySufficient,
		"needs_evidence", // the parser upper-cases, but callers may not
	}
	for _, v := range terminal {
		if !IsTerminalVerdict(v) {
			t.Errorf("IsTerminalVerdict(%q) = false, expected true", v)
		}
	}
	for _, v := range []string{VerdictProposeChange, VerdictLegacyNewPlan, "", "ALGO_INVENTADO"} {
		if IsTerminalVerdict(v) {
			t.Errorf("IsTerminalVerdict(%q) = true, expected false", v)
		}
	}
}

// Every verdict the parser accepts must be classified by exactly one of the
// two lists, or a new verdict could silently take the proposal branch.
func TestEveryVerdictIsClassified(t *testing.T) {
	for _, v := range append(append([]string{}, ChangeVerdicts...), TerminalVerdicts...) {
		if got := Verdict("VEREDICTO: " + v); got != v {
			t.Errorf("Verdict did not accept the declared verdict %q (got %q)", v, got)
		}
	}
	for _, v := range ChangeVerdicts {
		if IsTerminalVerdict(v) {
			t.Errorf("%q is in ChangeVerdicts but reports as terminal", v)
		}
	}
}

func TestExtractFiles(t *testing.T) {
	text := "previous text\n" +
		"=== ARCHIVO: db/schema.sql ===\n" +
		"CREATE TABLE competencia (id uuid);\n" +
		"=== FIN ARCHIVO ===\n" +
		"in between\n" +
		"=== ARCHIVO: /../../etc/passwd ===\n" +
		"malicious\n" +
		"=== FIN ARCHIVO ===\n"

	as := ExtractFiles(text)
	if len(as) != 1 {
		t.Fatalf("expected only the safe file, got %d", len(as))
	}
	if as[0].Path != "db/schema.sql" {
		t.Errorf("path 0 = %q", as[0].Path)
	}
	if as[0].Content != "CREATE TABLE competencia (id uuid);" {
		t.Errorf("content 0 = %q", as[0].Content)
	}
}

func TestValidateTasksDetectsMissingDependency(t *testing.T) {
	ts := ParseTasks(`### TAREA: T-001
RESPONSABLE: ingeniero
TITULO: X
DESCRIPCION: Y
DEPENDE_DE: T-099
`)
	if err := validateTasks(ts); err == nil {
		t.Fatal("expected an error for a missing dependency")
	}
}

func TestParseNeeds(t *testing.T) {
	text := "I'm explaining my reasoning.\n\n" +
		"NECESITO: internal/repo/grades.go\n" +
		"necesito: internal/httpapi/grades.go\n" +
		"NECESITO: ../../etc/passwd\n" +
		"NECESITO: -\n"

	paths := ParseNeeds(text)
	if len(paths) != 3 {
		t.Fatalf("expected 3 paths, got %d: %v", len(paths), paths)
	}
	if paths[0] != "internal/repo/grades.go" {
		t.Errorf("path 0 = %q", paths[0])
	}
	if paths[1] != "internal/httpapi/grades.go" {
		t.Errorf("path 1 = %q", paths[1])
	}
	if paths[2] != "../../etc/passwd" {
		t.Errorf("unsafe request must reach the policy unchanged: %q", paths[2])
	}
}

func TestParseNeedsWithoutFiles(t *testing.T) {
	if paths := ParseNeeds("NECESITO: -\n"); paths != nil {
		t.Errorf("expected no paths, got %v", paths)
	}
	if paths := ParseNeeds("there's no NECESITO line here"); paths != nil {
		t.Errorf("expected no paths, got %v", paths)
	}
}

func TestValidateTasksRejectsInvalidOwner(t *testing.T) {
	ts := ParseTasks(`### TAREA: T-001
RESPONSABLE: contador
TITULO: X
DESCRIPCION: Y
DEPENDE_DE: -
`)
	if err := validateTasks(ts); err == nil {
		t.Fatal("expected an error for an invalid owner")
	}
}

func TestValidateTasksRejectsDuplicateAndCyclicIDs(t *testing.T) {
	duplicate := `### TAREA: T-001
RESPONSABLE: ingeniero
TITULO: X
DESCRIPCION: Y
CRITERIOS:
- z
DEPENDE_DE: -
### TAREA: T-001
RESPONSABLE: ingeniero
TITULO: X
DESCRIPCION: Y
CRITERIOS:
- z
DEPENDE_DE: -
`
	if err := validateTasks(ParseTasks(duplicate)); err == nil {
		t.Fatal("expected duplicate task error")
	}
	cycle := `### TAREA: T-001
RESPONSABLE: ingeniero
TITULO: X
DESCRIPCION: Y
CRITERIOS:
- z
DEPENDE_DE: T-002
### TAREA: T-002
RESPONSABLE: ingeniero
TITULO: X
DESCRIPCION: Y
CRITERIOS:
- z
DEPENDE_DE: T-001
`
	if err := validateTasks(ParseTasks(cycle)); err == nil {
		t.Fatal("expected dependency cycle error")
	}
}
