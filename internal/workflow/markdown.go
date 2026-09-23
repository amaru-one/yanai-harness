package workflow

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const MarkdownOrigin = "engineering_ticket"

// MarkdownTicket is an immutable human decision, not an interview or a model proposal.
type MarkdownTicket struct {
	Title       string        `json:"title"`
	Task        string        `json:"task"`
	Criteria    []Requirement `json:"criteria"`
	Constraints string        `json:"constraints,omitempty"`
	Revision    string        `json:"revision"`
	Source      ArtifactRef   `json:"source"`
}

func ParseMarkdownTicket(raw string) (MarkdownTicket, error) {
	t := MarkdownTicket{Revision: Digest(raw)}
	if len(raw) > 1<<20 || !utf8.ValidString(raw) {
		return t, errors.New("ticket must be UTF-8 Markdown, at most 1 MiB")
	}
	section := ""
	seen := map[string]bool{}
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
			if section != "task" && section != "acceptance criteria" && section != "constraints" {
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
		case "task":
			task = append(task, line)
		case "constraints":
			constraints = append(constraints, line)
		case "acceptance criteria":
			if !strings.HasPrefix(line, "- ") || strings.TrimSpace(line[2:]) == "" {
				return t, errors.New("acceptance criteria must be nonempty '- ' bullets")
			}
			t.Criteria = append(t.Criteria, Requirement{ID: fmt.Sprintf("AC-%03d", len(t.Criteria)+1), Text: strings.TrimSpace(line[2:])})
		default:
			return t, errors.New("ticket content must be under Task, Acceptance criteria, or Constraints")
		}
	}
	t.Task = strings.Join(task, "\n")
	t.Constraints = strings.Join(constraints, "\n")
	if t.Title == "" || t.Task == "" || len(t.Criteria) == 0 {
		return t, errors.New("ticket requires '# Title', '## Task', and '## Acceptance criteria' with bullets")
	}
	return t, nil
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
