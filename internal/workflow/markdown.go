package workflow

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MarkdownOrigin = "engineering_ticket"

// MarkdownTicket is an immutable human decision, not an interview or a model proposal.
type MarkdownTicket struct {
	Title string `json:"title"`
	// Type is the ticket's declared Conventional Commits type (## Type). It
	// names the ticket branch prefix and the only commit type the worker may use.
	Type string `json:"type,omitempty"`
	// Slug is derived from Title once, when the cycle is created, and names
	// the ticket branch and the generated prompt directory.
	Slug        string        `json:"slug,omitempty"`
	Task        string        `json:"task"`
	Criteria    []Requirement `json:"criteria"`
	Constraints string        `json:"constraints,omitempty"`
	// Names are visible to agents; values stay only in the approved source artifact.
	CheckInputNames []string    `json:"check_input_names,omitempty"`
	Revision        string      `json:"revision"`
	Source          ArtifactRef `json:"source"`
}

func ParseMarkdownTicket(raw string) (MarkdownTicket, error) {
	t := MarkdownTicket{Revision: Digest(raw)}
	if len(raw) > 1<<20 || !utf8.ValidString(raw) {
		return t, errors.New("ticket must be UTF-8 Markdown, at most 1 MiB")
	}
	section := ""
	seen := map[string]bool{}
	inputs := map[string]bool{}
	var task, constraints []string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			if t.Title != "" {
				return t, errors.New("ticket must have exactly one title")
			}
			t.Title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			section = ""
			continue
		}
		if strings.HasPrefix(line, "## ") {
			section = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
			if section != "type" && section != "task" && section != "acceptance criteria" && section != "constraints" && section != "check inputs" {
				return t, fmt.Errorf("unknown ticket section %q", section)
			}
			if seen[section] {
				return t, fmt.Errorf("duplicate ticket section %q", section)
			}
			seen[section] = true
			continue
		}
		if line == "" {
			continue
		}
		switch section {
		case "type":
			if t.Type != "" {
				return t, errors.New("## Type must contain exactly one line with the ticket type")
			}
			value := line
			if i := strings.Index(value, "<!--"); i >= 0 && strings.HasSuffix(value, "-->") {
				value = strings.TrimSpace(value[:i])
			}
			if !ValidTicketType(value) {
				return t, fmt.Errorf("unknown ticket type %q in ## Type; use one of: %s", value, strings.Join(TicketTypes, ", "))
			}
			t.Type = value
		case "task":
			task = append(task, line)
		case "constraints":
			constraints = append(constraints, line)
		case "acceptance criteria":
			if !strings.HasPrefix(line, "- ") || strings.TrimSpace(line[2:]) == "" {
				return t, errors.New("acceptance criteria must be nonempty '- ' bullets")
			}
			t.Criteria = append(t.Criteria, Requirement{ID: fmt.Sprintf("AC-%03d", len(t.Criteria)+1), Text: strings.TrimSpace(line[2:])})
		case "check inputs":
			name, value, ok := strings.Cut(strings.TrimPrefix(line, "- "), "=")
			name = strings.TrimSpace(name)
			if !strings.HasPrefix(line, "- ") || !ok || !ValidCheckEnvName(name) || strings.TrimSpace(value) == "" || len(value) > 16<<10 || strings.ContainsRune(value, 0) || inputs[name] {
				return t, errors.New("check inputs require unique '- NAME=value' lines with nonempty values")
			}
			inputs[name] = true
			t.CheckInputNames = append(t.CheckInputNames, name)
		default:
			return t, errors.New("ticket content must be under Type, Task, Acceptance criteria, Constraints, or Check inputs")
		}
	}
	t.Task = strings.Join(task, "\n")
	t.Constraints = strings.Join(constraints, "\n")
	if t.Title == "" || t.Task == "" || len(t.Criteria) == 0 {
		return t, errors.New("ticket requires '# Title', '## Task', and '## Acceptance criteria' with bullets")
	}
	if !seen["type"] || t.Type == "" {
		return t, fmt.Errorf("ticket requires a '## Type' section with one of: %s", strings.Join(TicketTypes, ", "))
	}
	slug, err := TicketSlug(t.Title)
	if err != nil {
		return t, err
	}
	t.Slug = slug
	return t, nil
}

// TicketTypes are the Conventional Commits types a ticket may declare.
var TicketTypes = []string{"feat", "fix", "refactor", "perf", "test", "docs", "build", "ci", "chore", "style"}

// ValidTicketType reports whether value is one of TicketTypes.
func ValidTicketType(value string) bool {
	for _, t := range TicketTypes {
		if value == t {
			return true
		}
	}
	return false
}

var slugRuns = regexp.MustCompile(`[^a-z0-9]+`)

// TicketSlug derives a stable branch-safe name from a ticket title:
// lowercase ASCII letters and digits, every other run replaced by a single
// hyphen, trimmed, at most 64 characters.
func TicketSlug(title string) (string, error) {
	slug := slugRuns.ReplaceAllString(strings.ToLower(title), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 64 {
		slug = strings.TrimRight(slug[:64], "-")
	}
	if slug == "" {
		return "", fmt.Errorf("ticket title %q produces an empty branch name; use a title with ASCII letters or digits", title)
	}
	return slug, nil
}

// TicketBranch is the branch a level 0 cycle works on: <type>/<slug>.
func (t MarkdownTicket) TicketBranch() string { return t.Type + "/" + t.Slug }

// ParseCheckInputs returns human-supplied values only to the check executor.
// ParseMarkdownTicket deliberately exposes names alone to the model and review.
func ParseCheckInputs(raw string) (map[string]string, error) {
	if _, err := ParseMarkdownTicket(raw); err != nil {
		return nil, err
	}
	inputs := map[string]string{}
	section := ""
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			section = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
			continue
		}
		if section != "check inputs" || !strings.HasPrefix(line, "- ") {
			continue
		}
		name, value, _ := strings.Cut(strings.TrimPrefix(line, "- "), "=")
		inputs[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return inputs, nil
}

// RedactCheckInputs keeps the human ticket readable in model context without
// sending values that belong only to approved checks.
func RedactCheckInputs(raw string) (string, error) {
	if _, err := ParseMarkdownTicket(raw); err != nil {
		return "", err
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	section := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			section = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")))
			continue
		}
		if section == "check inputs" && strings.HasPrefix(trimmed, "- ") {
			name, _, _ := strings.Cut(strings.TrimPrefix(trimmed, "- "), "=")
			lines[i] = "- " + strings.TrimSpace(name) + "=[provided to approved checks]"
		}
	}
	return strings.Join(lines, "\n"), nil
}

// DecodeStrict shares the duplicate-key and trailing-data defenses of candidates.
func DecodeStrict(raw string, out any) error {
	if !utf8.ValidString(raw) {
		return errors.New("response is not UTF-8")
	}
	d := newStrictDecoder(raw)
	if err := uniqueValue(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("expected exactly one JSON object")
	}
	d = newStrictDecoder(raw)
	d.DisallowUnknownFields()
	return d.Decode(out)
}

type ObservationDetail struct {
	Description string `json:"description"`
	Requirement string `json:"requirement"`
	Question    string `json:"question"`
}

func (o ObservationDetail) Validate() error {
	if strings.TrimSpace(o.Description) == "" || strings.TrimSpace(o.Requirement) == "" || strings.TrimSpace(o.Question) == "" {
		return errors.New("observation needs description, affected requirement, and question")
	}
	return nil
}
