package team

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// MaxExecutionContextBytes bounds one execution request's complete inputs.
// Exceeding it is an error, never a truncation: a model asked to return the
// complete source of a file it was only shown part of will confidently invent
// the rest, and the result is indistinguishable from a real edit.
const MaxExecutionContextBytes = 4 << 20

// maxSupportingFileBytes bounds one *optional* supporting file. A supporting
// file over the limit is listed with the reason it was left out, so the model
// is told a file exists and was not shown rather than being left to assume it
// does not exist.
const maxSupportingFileBytes = 256 << 10

// maxFailureBytes bounds the check output a repair request carries. The tail
// is kept to favor final diagnostics and summaries; the full output stays in evidence.
const maxFailureBytes = 12 << 10

// Preimage is one declared output's exact current content. Exists is explicit
// so a new file is stated as absent rather than shown as an empty one — the
// two produce different edits and only one of them is a deletion.
type Preimage struct {
	Path   string  `json:"path"`
	Exists bool    `json:"exists"`
	Mode   string  `json:"mode,omitempty"`
	Source *string `json:"source,omitempty"`
}

// Document is a complete governing or supporting file. Omitted names why a
// file the model might need was not included, instead of leaving a silent gap.
type Document struct {
	Path    string `json:"path"`
	Source  string `json:"source,omitempty"`
	Omitted string `json:"omitted,omitempty"`
}

// Dependency is what a ticket this one depends on actually produced. Only an
// implemented dependency appears here, and its change is already in the
// checkout, which is why the preimages above are the truth about the code.
type Dependency struct {
	Ticket  string   `json:"ticket"`
	Status  string   `json:"status"`
	Outputs []string `json:"outputs"`
}

// Repair carries the previous round's failure back to the model: the actual
// diff that is in the checkout right now, and the bounded output of the checks
// that rejected it.
type Repair struct {
	Round    int      `json:"round"`
	Diff     string   `json:"diff"`
	Failures []string `json:"failures"`
}

// ExecutionInput is the complete, durable input of one execution round. It is
// published as an artifact before the model is called, so what was asked is
// recoverable independently of what came back.
type ExecutionInput struct {
	Contract     string                   `json:"contract"`
	Cycle        int                      `json:"cycle"`
	Round        int                      `json:"round"`
	Ticket       workflow.Ticket          `json:"ticket"`
	Dependencies []Dependency             `json:"dependencies"`
	Repository   workflow.RepositoryState `json:"repository"`
	Preimages    []Preimage               `json:"preimages"`
	Governing    []Document               `json:"governing"`
	Supporting   []Document               `json:"supporting"`
	Scope        string                   `json:"scope"`
	Plan         string                   `json:"plan"`
	Repair       *Repair                  `json:"repair,omitempty"`
	Observations []workflow.Observation   `json:"observations,omitempty"`
}

// governingPaths lists the instruction documents that govern a ticket's
// outputs: the repository's AGENTS.md and the SPEC.md of every directory on
// the way to each output. They are required context — a SPEC.md that exists
// and cannot be read stops the round rather than being skipped.
func governingPaths(t workflow.Ticket) []string {
	seen := map[string]bool{"AGENTS.md": true}
	paths := []string{"AGENTS.md"}
	for _, output := range t.Outputs {
		dir := filepath.ToSlash(filepath.Dir(output))
		for dir != "." && dir != "/" {
			for _, name := range []string{"AGENTS.md", "SPEC.md"} {
				spec := dir + "/" + name
				if !seen[spec] {
					seen[spec] = true
					paths = append(paths, spec)
				}
			}
			dir = filepath.ToSlash(filepath.Dir(dir))
		}
	}
	sort.Strings(paths)
	return paths
}

// readPreimages returns the exact current content of every declared output.
// A path the executor refuses is an error here, not an omission: the ticket
// declared it, so a round that cannot see it cannot produce a complete file.
func readPreimages(backend *executor.Native, t workflow.Ticket) ([]Preimage, error) {
	var out []Preimage
	for _, path := range t.Outputs {
		f, err := backend.Read(path)
		if err != nil {
			return nil, fmt.Errorf("declared output %s: %w", path, err)
		}
		p := Preimage{Path: path}
		if f != nil {
			source := string(f.Data)
			p.Exists, p.Mode, p.Source = true, fmt.Sprintf("%o", f.Mode), &source
		}
		out = append(out, p)
	}
	return out, nil
}

// readGoverning reads the instruction documents. An absent SPEC.md is normal
// (the directory may be new) and is reported as such; a SPEC.md that is itself
// a declared output of this ticket is left to the ticket to write, which is
// the only case in which this round may create one.
func readGoverning(backend *executor.Native, t workflow.Ticket) ([]Document, error) {
	declared := map[string]bool{}
	for _, output := range t.Outputs {
		declared[output] = true
	}
	var out []Document
	for _, path := range governingPaths(t) {
		f, err := backend.Read(path)
		if err != nil {
			return nil, fmt.Errorf("governing document %s: %w", path, err)
		}
		switch {
		case f != nil:
			out = append(out, Document{Path: path, Source: string(f.Data)})
		case declared[path]:
			out = append(out, Document{Path: path, Omitted: "absent; this ticket is approved to create it"})
		default:
			out = append(out, Document{Path: path, Omitted: "absent"})
		}
	}
	return out, nil
}

// readSupporting reads the files the model asked to see, completely. A file
// that is too large, denied by policy or missing is reported with its reason
// rather than partially included.
func readSupporting(backend *executor.Native, paths []string, declared map[string]bool) []Document {
	var out []Document
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] || declared[path] {
			continue // already supplied complete, as a preimage
		}
		seen[path] = true
		f, err := backend.Read(path)
		switch {
		case err != nil:
			out = append(out, Document{Path: path, Omitted: "not readable: " + err.Error()})
		case f == nil:
			out = append(out, Document{Path: path, Omitted: "absent"})
		case len(f.Data) > maxSupportingFileBytes:
			out = append(out, Document{Path: path, Omitted: fmt.Sprintf("larger than the %d-byte supporting-file limit; ask for a narrower file", maxSupportingFileBytes)})
		default:
			out = append(out, Document{Path: path, Source: string(f.Data)})
		}
	}
	return out
}

// Encode serializes the complete input and refuses an oversized one. The limit
// is checked against the encoded bytes, which is what is actually sent.
func (in ExecutionInput) Encode() ([]byte, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxExecutionContextBytes {
		return nil, fmt.Errorf("execution context is %d bytes, over the %d-byte limit; narrow the ticket's outputs or supporting files rather than working from partial source", len(data), MaxExecutionContextBytes)
	}
	return data, nil
}

// boundedTail keeps the last n bytes of check output, on a rune boundary, and
// says how much was dropped rather than silently shortening it.
func boundedTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	dropped := len(s) - n
	tail := s[dropped:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < 200 {
		tail = tail[i+1:]
	}
	return fmt.Sprintf("[%d earlier bytes of output omitted]\n%s", dropped, tail)
}

// candidateInstructions is the wire contract for an execution response. It is
// deliberately explicit that omission, fragments and "rest unchanged" are
// rejected: those are the shapes a truncated response takes, and accepting any
// of them is how a partial file becomes a complete overwrite.
const candidateInstructions = `Responde EXACTAMENTE con un objeto JSON, sin cercas de código, sin texto antes ni después.

{"schema_version":"1","ticket":"<id de la tarea>","result":"change"|"no_change"|"observation","explanation":"<qué decidiste y por qué, en español claro>","files":[...]}

Reglas:
- Si tienes CUALQUIER observación, incluso consultiva, responde result="observation", files=[], observations=[{"description":"...","requirement":"AC o tarea afectada","question":"pregunta al humano"}]. El ciclo se pausa hasta respuesta y nueva aprobación humana. No ocultes observaciones en explanation. Las respuestas humanas están en el contexto; no amplían el contrato aprobado.
- "files" lleva UNA entrada por cada salida declarada de la tarea, ni más ni menos, con la ruta exacta.
- Cada entrada es {"path":"<ruta declarada>","operation":"source"|"delete"|"unchanged"} y, solo para "source", "source":"<contenido COMPLETO del archivo>" y opcionalmente "mode":"644"|"755".
- "source" reemplaza el archivo entero: entrega el archivo completo, nunca un fragmento ni "el resto queda igual".
- "unchanged" declara explícitamente que esa salida no necesita cambios. Omitir una salida es un error, no un "sin cambios".
- "delete" borra un archivo que existe hoy.
- "no_change" es para cuando la tarea entera no requiere ningún cambio: entonces "files" va vacío y la explicación debe justificarlo. No es una salida exitosa y no desbloquea tareas dependientes.
- Un "change" debe modificar al menos una salida declarada.
- No inventes rutas: las salidas declaradas son las únicas permitidas.
- El código de arriba es el contenido real y actual del repositorio. Trabaja sobre él, no sobre lo que recuerdes.`
