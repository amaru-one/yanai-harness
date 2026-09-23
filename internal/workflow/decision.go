package workflow

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type Citation struct {
	ID        string `json:"id"`
	SourceID  string `json:"source_id"`
	Revision  string `json:"revision"`
	ExcerptID string `json:"excerpt_id"`
	Quote     string `json:"quote"`
}

type Finding struct {
	Kind       string   `json:"kind"` // statement | inference | conflict
	Text       string   `json:"text"`
	Evidence   []string `json:"evidence"`
	Resolution string   `json:"resolution,omitempty"`
}

// DecodeProposal accepts one strict JSON object, never Markdown or a regex
// verdict. Reject duplicate keys rather than letting the final value win.
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

// newStrictDecoder reads a model response with numbers left as strings, so a
// large or fractional value cannot be silently rounded through float64 before
// anything has decided whether it was even allowed to be there.
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

// ValidateDecision checks provenance against engine-owned source/scope
// snapshots, in addition to the structural contract and repository boundary.
// It proves reference/quote integrity, not whether an inference is sound.
func ValidateDecision(p Proposal, c DecisionContext, roles []Role, checkPath func(string) error) error {
	if err := ValidateProposal(p, roles); err != nil {
		return err
	}
	if !opaqueID.MatchString(p.ID) || strings.TrimSpace(p.Summary) == "" {
		return fmt.Errorf("proposal requires a safe ID and summary")
	}
	if p.Origin != c.Source.Origin {
		return fmt.Errorf("proposal origin must match the human-selected intake origin")
	}
	if p.Origin == TechnicalEnabler && strings.TrimSpace(p.Rationale) == "" {
		return fmt.Errorf("technical enabler requires an engineering rationale")
	}
	if err := validateInputs(p.Inputs, c.Inputs); err != nil {
		return err
	}
	scope := map[string]bool{}
	for _, r := range c.Scope.Requirements {
		scope[r.ID] = true
	}
	if err := references(p.Scope, scope, true); err != nil {
		return fmt.Errorf("proposal scope: %w", err)
	}
	excerpts := map[string]string{}
	for _, e := range c.Source.Excerpts {
		excerpts[e.ID] = e.Text
	}
	citations := map[string]bool{}
	quotes := map[string]string{}
	for _, e := range p.Citations {
		if !opaqueID.MatchString(e.ID) || citations[e.ID] {
			return fmt.Errorf("citation IDs must be unique and safe")
		}
		if e.SourceID != c.Source.ID || e.Revision != c.Source.Revision || strings.TrimSpace(e.Quote) == "" || !strings.Contains(excerpts[e.ExcerptID], e.Quote) {
			return fmt.Errorf("citation %s is not an exact quotation from the supplied source revision/excerpt", e.ID)
		}
		citations[e.ID] = true
		quotes[e.ID] = e.Quote
	}
	needsCitation := p.Origin == Product && (p.Outcome == OutcomeProposeChange || p.Outcome == OutcomeNoChange || p.Outcome == OutcomeOutOfScope)
	if err := references(p.Evidence, citations, needsCitation); err != nil {
		return fmt.Errorf("proposal evidence: %w", err)
	}
	for _, f := range p.Findings {
		if strings.TrimSpace(f.Text) == "" {
			return fmt.Errorf("finding text cannot be blank")
		}
		if f.Kind != "statement" && f.Kind != "inference" && f.Kind != "conflict" {
			return fmt.Errorf("finding kind must distinguish statement, inference or conflict")
		}
		if err := references(f.Evidence, citations, true); err != nil {
			return err
		}
		if f.Kind == "statement" {
			exact := false
			for _, id := range f.Evidence {
				if f.Text == quotes[id] {
					exact = true
				}
			}
			if !exact {
				return fmt.Errorf("statement must reproduce a cited quote; label interpretations as inference")
			}
		}
		if f.Kind == "conflict" {
			if len(f.Evidence) < 2 {
				return fmt.Errorf("conflict requires at least two distinct citations")
			}
			if strings.TrimSpace(f.Resolution) == "" && (p.Outcome == OutcomeProposeChange || p.Outcome == OutcomeNoChange) {
				return fmt.Errorf("unresolved conflicting evidence requires NEEDS_EVIDENCE")
			}
		}
	}
	if p.Outcome == OutcomeNeedsEvidence && len(p.Questions) == 0 {
		return fmt.Errorf("NEEDS_EVIDENCE must name the unanswered questions")
	}
	for _, q := range p.Questions {
		if strings.TrimSpace(q) == "" {
			return fmt.Errorf("question cannot be blank")
		}
	}
	if len(p.Questions) > 0 && (p.Outcome == OutcomeProposeChange || p.Outcome == OutcomeNoChange) {
		return fmt.Errorf("unanswered blocking questions require NEEDS_EVIDENCE")
	}
	if len(c.Source.Excerpts) == 0 && p.Outcome != OutcomeNeedsEvidence {
		return fmt.Errorf("absent source evidence requires NEEDS_EVIDENCE")
	}
	for _, t := range p.Tickets {
		if t.BaseCommit != c.BaseCommit {
			return fmt.Errorf("ticket base_commit must match the supplied repository snapshot")
		}
		if !opaqueID.MatchString(t.ID) || strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Description) == "" {
			return fmt.Errorf("ticket requires safe ID, title and description")
		}
		if t.Type != p.Origin || t.Status != "pending" || t.Revision != 1 || t.MaxAttempts < 1 || t.MaxAttempts > 3 {
			return fmt.Errorf("ticket type must match intake; new tickets require pending, revision 1 and 1–3 attempts")
		}
		if t.Type == TechnicalEnabler && strings.TrimSpace(t.Rationale) == "" {
			return fmt.Errorf("technical-enabler ticket requires rationale")
		}
		if err := references(t.Evidence, citations, t.Type == Product); err != nil {
			return fmt.Errorf("ticket evidence: %w", err)
		}
		if err := references(t.Scope, scope, true); err != nil {
			return fmt.Errorf("ticket scope: %w", err)
		}
		if err := validateInputs(t.Inputs, c.Inputs); err != nil {
			return err
		}
		if len(t.AllowedPaths) == 0 {
			return fmt.Errorf("ticket requires allowed_paths")
		}
		for _, path := range append(append([]string{}, t.AllowedPaths...), t.Outputs...) {
			if err := checkPath(path); err != nil {
				return err
			}
		}
		for _, output := range t.Outputs {
			allowed := false
			for _, path := range t.AllowedPaths {
				if output == path || strings.HasPrefix(output, path+"/") {
					allowed = true
				}
			}
			if !allowed {
				return fmt.Errorf("ticket output is outside its allowed_paths")
			}
		}
	}
	return nil
}

func references(refs []string, known map[string]bool, required bool) error {
	if required && len(refs) == 0 {
		return fmt.Errorf("at least one reference is required")
	}
	seen := map[string]bool{}
	for _, id := range refs {
		if !known[id] || seen[id] {
			return fmt.Errorf("unknown or duplicate reference %q", id)
		}
		seen[id] = true
	}
	return nil
}

func validateInputs(got, want []ArtifactRef) error {
	if len(got) != len(want) {
		return fmt.Errorf("inputs must reference the supplied scope and source snapshots")
	}
	seen := map[string]bool{}
	for _, ref := range got {
		found := false
		for _, expected := range want {
			if ref == expected {
				found = true
			}
		}
		if !found || seen[ref.ID] {
			return fmt.Errorf("unknown, stale or duplicate input reference")
		}
		seen[ref.ID] = true
	}
	return nil
}

// RenderProposal is a review projection. It is never parsed back into tasks.
func RenderProposal(p Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nOutcome: %s\nOrigin: %s\n\n%s\n\n", p.ID, p.Outcome, p.Origin, p.Summary)
	if p.Rationale != "" {
		fmt.Fprintf(&b, "Rationale: %s\n\n", p.Rationale)
	}
	fmt.Fprintf(&b, "Scope requirements: %s\n\n", strings.Join(p.Scope, ", "))
	for _, input := range p.Inputs {
		fmt.Fprintf(&b, "Input %s: %s (revision %s, SHA256 %s)\n\n", input.ID, input.Path, input.Version, input.SHA256)
	}
	for _, c := range p.Citations {
		fmt.Fprintf(&b, "- [%s] %s / %s / %s: %q\n", c.ID, c.SourceID, c.Revision, c.ExcerptID, c.Quote)
	}
	for _, f := range p.Findings {
		fmt.Fprintf(&b, "\n**%s**: %s (evidence: %s)\n%s\n", f.Kind, f.Text, strings.Join(f.Evidence, ", "), f.Resolution)
	}
	for _, q := range p.Questions {
		fmt.Fprintf(&b, "\nQuestion: %s\n", q)
	}
	for _, t := range p.Tickets {
		fmt.Fprintf(&b, "\n## %s — %s\n\nOwner: %s\nType: %s\n\n%s\n\nRationale: %s\n\nEvidence: %s\nScope: %s\nDepends on: %s\nAllowed paths: %s\nOutputs: %s\n\n", t.ID, t.Title, t.Owner, t.Type, t.Description, t.Rationale, strings.Join(t.Evidence, ", "), strings.Join(t.Scope, ", "), strings.Join(t.DependsOn, ", "), strings.Join(t.AllowedPaths, ", "), strings.Join(t.Outputs, ", "))
		for _, criterion := range t.Criteria {
			fmt.Fprintf(&b, "- %s\n", criterion)
		}
	}
	return b.String()
}
