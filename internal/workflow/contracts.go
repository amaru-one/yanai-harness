package workflow

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func Digest(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

var opaqueID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var email = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
var identifiers = regexp.MustCompile(`\b[0-9][0-9 .()-]{6,}[0-9]\b`)

const (
	Product          = "product_change"
	TechnicalEnabler = "technical_enabler"
)

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

type Requirement struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Citation struct {
	ID        string `json:"id"`
	SourceID  string `json:"source_id"`
	Revision  string `json:"revision"`
	ExcerptID string `json:"excerpt_id"`
	Quote     string `json:"quote"`
}

type Finding struct {
	Kind       string   `json:"kind"`
	Text       string   `json:"text"`
	Evidence   []string `json:"evidence"`
	Resolution string   `json:"resolution,omitempty"`
}

func DecodeProposal(raw string) (Proposal, error) {
	d := newStrictDecoder(raw)
	if err := uniqueValue(d); err != nil {
		return Proposal{}, err
	}
	if _, err := d.Token(); err != io.EOF {
		return Proposal{}, fmt.Errorf("expected exactly one JSON object")
	}
	d = newStrictDecoder(raw)
	d.DisallowUnknownFields()
	var p Proposal
	if err := d.Decode(&p); err != nil {
		return Proposal{}, err
	}
	return p, nil
}

func newStrictDecoder(raw string) *json.Decoder {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	return d
}

func uniqueValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid JSON key")
			}
			seen[name] = true
			if err := uniqueValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueValue(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func RenderProposal(p Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nOutcome: %s\nOrigin: %s\n\n%s\n\n", p.ID, p.Outcome, p.Origin, p.Summary)
	for _, t := range p.Tickets {
		fmt.Fprintf(&b, "## %s — %s\n\nOwner: %s\n\n%s\n\nAllowed paths: %s\nOutputs: %s\n\n", t.ID, t.Title, t.Owner, t.Description, strings.Join(t.AllowedPaths, ", "), strings.Join(t.Outputs, ", "))
		for _, criterion := range t.Criteria {
			fmt.Fprintf(&b, "- %s\n", criterion)
		}
	}
	return b.String()
}
