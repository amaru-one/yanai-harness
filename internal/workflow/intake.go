package workflow

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const Product = "product_change"
const TechnicalEnabler = "technical_enabler"

// Intake is local provenance. Public() deliberately omits the local source
// name, original hash and redaction terms. The original stays in 00-entrada.md.
type Intake struct {
	ID              string    `json:"id"`
	Name            string    `json:"local_source"`
	Date            string    `json:"interview_date,omitempty"`
	ReceivedAt      time.Time `json:"received_at"`
	OriginalHash    string    `json:"original_sha256"`
	Revision        string    `json:"redacted_sha256"`
	Origin          string    `json:"origin"`
	PrivacyReviewed bool      `json:"privacy_reviewed"`
	Redactions      []string  `json:"redactions,omitempty"`
	Excerpts        []Excerpt `json:"excerpts"`
}

type Excerpt struct {
	ID   string `json:"id"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type Source struct {
	ID       string    `json:"id"`
	Date     string    `json:"date,omitempty"`
	Revision string    `json:"revision"`
	Origin   string    `json:"origin"`
	Excerpts []Excerpt `json:"excerpts"`
}

type IntakeOptions struct {
	ID, Name, Date, Origin string
	PrivacyReviewed        bool
	Redactions             []string
}

func Digest(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

var opaqueID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var email = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
var identifiers = regexp.MustCompile(`\b[0-9][0-9 .()-]{6,}[0-9]\b`)

// Redact removes explicitly supplied names/identifiers and recognizable email,
// phone/DNI-like numeric strings. It is not a names detector: the interviewer
// must review names, school details and indirect identifiers before ingestion.
func Redact(text string, terms []string) string {
	ordered := append([]string(nil), terms...)
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, term := range ordered {
		if strings.TrimSpace(term) == "" {
			continue
		}
		var b strings.Builder
		last := 0
		for _, pos := range regexp.MustCompile("(?i)"+regexp.QuoteMeta(term)).FindAllStringIndex(text, -1) {
			before, _ := utf8.DecodeLastRuneInString(text[:pos[0]])
			after, _ := utf8.DecodeRuneInString(text[pos[1]:])
			if unicode.IsLetter(before) || unicode.IsDigit(before) || unicode.IsLetter(after) || unicode.IsDigit(after) {
				continue
			}
			b.WriteString(text[last:pos[0]])
			b.WriteString("[REDACTED]")
			last = pos[1]
		}
		b.WriteString(text[last:])
		text = b.String()
	}
	text = email.ReplaceAllString(text, "[EMAIL]")
	return identifiers.ReplaceAllStringFunc(text, func(s string) string {
		if _, err := time.Parse("2006-01-02", s); err == nil {
			return s
		}
		n := 0
		for _, r := range s {
			if r >= '0' && r <= '9' {
				n++
			}
		}
		if n >= 8 {
			return "[IDENTIFIER]"
		}
		return s
	})
}

func NewIntake(raw string, o IntakeOptions) (Intake, error) {
	if !o.PrivacyReviewed {
		return Intake{}, fmt.Errorf("review/remove personal and indirect identifiers first, then pass --privacy-reviewed; use repeatable --redact for known identifiers")
	}
	if o.Origin == "" {
		o.Origin = Product
	}
	if o.Origin != Product && o.Origin != TechnicalEnabler {
		return Intake{}, fmt.Errorf("unknown intake origin")
	}
	if o.Date != "" {
		if _, err := time.Parse("2006-01-02", o.Date); err != nil {
			return Intake{}, fmt.Errorf("--date must be YYYY-MM-DD; omit it if unknown")
		}
	}
	if o.ID == "" {
		o.ID = "source-" + Digest(o.Name)[:16]
	}
	if !opaqueID.MatchString(o.ID) {
		return Intake{}, fmt.Errorf("--source-id must be an opaque identifier of 1–64 letters, digits, underscores or hyphens")
	}
	redacted := Redact(raw, o.Redactions)
	in := Intake{ID: o.ID, Name: o.Name, Date: o.Date, ReceivedAt: time.Now().UTC(), OriginalHash: Digest(raw), Revision: Digest(redacted), Origin: o.Origin, PrivacyReviewed: true, Redactions: o.Redactions}
	for line, text := range strings.Split(redacted, "\n") {
		if strings.TrimSpace(text) != "" {
			in.Excerpts = append(in.Excerpts, Excerpt{ID: fmt.Sprintf("E-%03d", line+1), Line: line + 1, Text: text})
		}
	}
	return in, nil
}

func (i Intake) Public() Source {
	return Source{ID: i.ID, Date: i.Date, Revision: i.Revision, Origin: i.Origin, Excerpts: i.Excerpts}
}

type Requirement struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Scope struct {
	Revision     string        `json:"revision"`
	Requirements []Requirement `json:"requirements"`
}

func NewScope(text string) (Scope, error) {
	s := Scope{Revision: Digest(text)}
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") {
			continue
		}
		id := "S-" + Digest(line)[:16]
		if !seen[id] {
			s.Requirements = append(s.Requirements, Requirement{ID: id, Text: line})
			seen[id] = true
		}
	}
	if len(s.Requirements) == 0 {
		return Scope{}, fmt.Errorf("context/alcance.md must contain scope requirements")
	}
	return s, nil
}

type DecisionContext struct {
	BaseCommit string        `json:"base_commit"`
	Source     Source        `json:"source"`
	Scope      Scope         `json:"scope"`
	Inputs     []ArtifactRef `json:"inputs"`
}

func NewDecisionContext(in Intake, scope Scope) DecisionContext {
	return DecisionContext{Source: in.Public(), Scope: scope, Inputs: []ArtifactRef{
		{ID: "scope", Path: "context/alcance.md", SHA256: scope.Revision, Version: scope.Revision},
		// A virtual reference resolves only to Public(), never the raw local file.
		{ID: "source", Path: "source:" + in.ID, SHA256: in.Revision, Version: in.Revision},
	}}
}
