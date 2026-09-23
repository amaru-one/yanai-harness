package workflow

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Candidate operations. "source" carries the complete successor bytes of a
// declared output, "delete" removes it, and "unchanged" is an explicit
// statement that this declared output needs no edit — which is not the same
// as omitting it, because omission is indistinguishable from a truncated
// response.
const (
	CandidateSource    = "source"
	CandidateDelete    = "delete"
	CandidateUnchanged = "unchanged"
)

// Candidate results. A change must alter at least one declared output; a
// no_change is an explicit, explained finding that the ticket needs no edit
// at all, and it never becomes an implementation.
const (
	CandidateChange      = "change"
	CandidateNoChange    = "no_change"
	CandidateObservation = "observation"
)

// CandidateFile is one declared output's complete successor. Source is a
// pointer so that an omitted key and an empty file are distinguishable: a
// present empty string is a legitimately empty file, a missing key is a
// truncated response.
type CandidateFile struct {
	Path      string  `json:"path"`
	Operation string  `json:"operation"`
	Source    *string `json:"source,omitempty"`
	Mode      string  `json:"mode,omitempty"`
}

// Candidate is the only wire format a model may use to propose an edit. It
// replaces the "=== ARCHIVO ===" fences, which could not express deletion, an
// explicitly unchanged file, or a no-change finding, and whose missing closing
// marker was indistinguishable from a file whose last lines were cut off.
type Candidate struct {
	SchemaVersion string              `json:"schema_version"`
	Ticket        string              `json:"ticket"`
	Result        string              `json:"result"`
	Explanation   string              `json:"explanation"`
	Files         []CandidateFile     `json:"files,omitempty"`
	Observations  []ObservationDetail `json:"observations,omitempty"`
}

// MaxCandidateBytes bounds one candidate's total declared source. It is the
// executor's own per-file limit times a small number of files; a response
// larger than this is refused rather than partially applied.
const MaxCandidateBytes = 8 << 20

// DecodeCandidate accepts exactly one strict JSON object and validates it
// against the ticket that was approved, not against whatever the response
// claims. Unknown fields, duplicate keys, trailing content, undeclared or
// missing outputs, duplicate paths and effective no-ops are all rejected: an
// edit this refuses never reaches the filesystem, and a refusal is always a
// rejected response rather than a narrowed application.
func DecodeCandidate(raw string, t Ticket) (Candidate, error) {
	var c Candidate
	if !utf8.ValidString(raw) {
		return c, errors.New("candidate is not valid UTF-8")
	}
	d := newStrictDecoder(raw)
	if err := uniqueValue(d); err != nil {
		return c, fmt.Errorf("candidate is not one strict JSON object: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return c, errors.New("candidate must be exactly one JSON object with no trailing content")
	}
	d = newStrictDecoder(raw)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("candidate does not match the required schema: %w", err)
	}
	if c.SchemaVersion != CandidateSchemaVersion {
		return c, fmt.Errorf("candidate schema_version must be %q", CandidateSchemaVersion)
	}
	if c.Ticket != t.ID {
		return c, fmt.Errorf("candidate names ticket %q, not the assigned %q", c.Ticket, t.ID)
	}
	if strings.TrimSpace(c.Explanation) == "" {
		return c, errors.New("candidate requires a nonblank explanation")
	}
	for _, o := range c.Observations {
		if err := o.Validate(); err != nil {
			return c, err
		}
	}
	switch c.Result {
	case CandidateObservation:
		if len(c.Observations) == 0 || len(c.Files) > 0 {
			return c, errors.New("observation result requires observations and no files")
		}
		return c, nil
	case CandidateNoChange:
		if len(c.Files) != 0 {
			return c, errors.New("a no_change candidate cannot carry files")
		}
		return c, nil
	case CandidateChange:
	default:
		return c, fmt.Errorf("candidate result must be %q or %q", CandidateChange, CandidateNoChange)
	}

	declared := map[string]bool{}
	for _, output := range t.Outputs {
		declared[output] = true
	}
	seen := map[string]bool{}
	total, edits := 0, 0
	for _, f := range c.Files {
		if !declared[f.Path] {
			return c, fmt.Errorf("candidate writes %q, which is not a declared output of %s", f.Path, t.ID)
		}
		if seen[f.Path] {
			return c, fmt.Errorf("candidate repeats output %q", f.Path)
		}
		seen[f.Path] = true
		switch f.Operation {
		case CandidateUnchanged, CandidateDelete:
			if f.Source != nil {
				return c, fmt.Errorf("%s operation %q cannot carry source", f.Path, f.Operation)
			}
			if f.Mode != "" {
				return c, fmt.Errorf("%s operation %q cannot carry a mode", f.Path, f.Operation)
			}
			if f.Operation == CandidateDelete {
				edits++
			}
		case CandidateSource:
			if f.Source == nil {
				return c, fmt.Errorf("%s declares a source edit without source; a complete file is required, never a fragment", f.Path)
			}
			// A JSON decoder substitutes U+FFFD for bytes and escapes it
			// cannot represent, and the substitution is indistinguishable
			// from a file that genuinely contains that character. Writing
			// either one into source is silent corruption, so both are
			// refused rather than guessed at.
			if strings.ContainsRune(*f.Source, utf8.RuneError) {
				return c, fmt.Errorf("%s source contains U+FFFD; resend it without unencodable bytes or escapes", f.Path)
			}
			switch f.Mode {
			case "", "644", "755":
			default:
				return c, fmt.Errorf("%s declares unsupported mode %q", f.Path, f.Mode)
			}
			total += len(*f.Source)
			edits++
		default:
			return c, fmt.Errorf("%s declares unknown operation %q", f.Path, f.Operation)
		}
	}
	for _, output := range t.Outputs {
		if !seen[output] {
			return c, fmt.Errorf("candidate omits declared output %q; state it explicitly as %q if it needs no edit", output, CandidateUnchanged)
		}
	}
	if edits == 0 {
		return c, fmt.Errorf("a change candidate must edit at least one declared output; report %q instead", CandidateNoChange)
	}
	if total > MaxCandidateBytes {
		return c, fmt.Errorf("candidate source exceeds %d bytes", MaxCandidateBytes)
	}
	return c, nil
}

// The two headings an execution request always carries: which ticket is being
// executed, and the exact list of paths the response must answer one-for-one.
// They are constants because both the renderer and every reader of a rendered
// request — a mock provider, a test provider — must agree on them exactly.
const (
	TicketHeading          = "# Tu tarea: "
	DeclaredOutputsHeading = "# Salidas declaradas (responde exactamente estas rutas, una entrada por cada una)"
)

// RenderDeclaredOutputs writes the checklist a candidate must answer.
func RenderDeclaredOutputs(outputs []string) string {
	var b strings.Builder
	b.WriteString("\n" + DeclaredOutputsHeading + "\n\n")
	for _, path := range outputs {
		b.WriteString("- " + path + "\n")
	}
	return b.String()
}

// DeclaredOutputs reads the ticket and its checklist back out of a rendered
// execution request. It exists so a mock or a test provider answers the
// request it was actually given rather than a hardcoded guess about it.
func DeclaredOutputs(message string) (ticket string, outputs []string) {
	for _, line := range strings.Split(message, "\n") {
		if strings.HasPrefix(line, TicketHeading) {
			ticket = strings.TrimSpace(strings.TrimPrefix(line, TicketHeading))
			break
		}
	}
	_, rest, ok := strings.Cut(message, DeclaredOutputsHeading+"\n")
	if !ok {
		return ticket, nil
	}
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			outputs = append(outputs, strings.TrimPrefix(line, "- "))
			continue
		}
		if line != "" {
			break
		}
	}
	return ticket, outputs
}

// FileMode returns the permission bits an operation asks for, defaulting to
// 0o644. The executor refuses anything else, so this never widens what a
// model can ask for.
func (f CandidateFile) FileMode() uint32 {
	if f.Mode == "755" {
		return 0o755
	}
	return 0o644
}
