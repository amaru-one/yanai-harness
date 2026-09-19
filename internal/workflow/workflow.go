// Package workflow contains the durable, provider-neutral workflow contracts
// used by the agent team. Markdown remains a human-readable projection; these
// types are the canonical state exchanged by the engine.
package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Outcome string

const (
	OutcomeNoChange        Outcome = "NO_CHANGE_NEEDED"
	OutcomeProposeChange   Outcome = "PROPOSE_CHANGE"
	OutcomeNeedsEvidence   Outcome = "NEEDS_EVIDENCE"
	OutcomeOutOfScope      Outcome = "OUT_OF_SCOPE"
	OutcomeBlockedBaseline Outcome = "BLOCKED_BY_BASELINE"
)

type Role struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Enabled bool   `json:"enabled"`
}

type ArtifactRef struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Media   string `json:"media,omitempty"`
	Version string `json:"version"`
}

type Ticket struct {
	SchemaVersion string        `json:"schema_version"`
	ID            string        `json:"id"`
	Type          string        `json:"type"`
	Owner         string        `json:"owner"`
	Status        string        `json:"status"`
	Evidence      []string      `json:"evidence,omitempty"`
	Scope         []string      `json:"scope,omitempty"`
	DependsOn     []string      `json:"depends_on,omitempty"`
	Inputs        []ArtifactRef `json:"inputs,omitempty"`
	Outputs       []string      `json:"outputs"`
	Criteria      []string      `json:"criteria"`
	AllowedPaths  []string      `json:"allowed_paths,omitempty"`
	BaseCommit    string        `json:"base_commit,omitempty"`
	MaxAttempts   int           `json:"max_attempts"`
	Revision      int           `json:"revision"`
}

type Proposal struct {
	SchemaVersion string        `json:"schema_version"`
	ID            string        `json:"id"`
	Outcome       Outcome       `json:"outcome"`
	Evidence      []string      `json:"evidence,omitempty"`
	Scope         []string      `json:"scope,omitempty"`
	Summary       string        `json:"summary"`
	Tickets       []Ticket      `json:"tickets,omitempty"`
	Inputs        []ArtifactRef `json:"inputs,omitempty"`
}

type Approval struct {
	ID           string    `json:"id"`
	Actor        string    `json:"actor"`
	PlanHash     string    `json:"plan_hash"`
	ScopeHash    string    `json:"scope_hash"`
	Baseline     string    `json:"baseline"`
	ApprovedAt   time.Time `json:"approved_at"`
	ContractHash string    `json:"contract_hash"`
}

func ApprovalValid(a Approval, plan, scope, baseline string) bool {
	return a.Actor != "" && a.PlanHash != "" && a.ScopeHash != "" && a.Baseline != "" &&
		a.PlanHash == plan && a.ScopeHash == scope && a.Baseline == baseline
}

type Event struct {
	ID             string    `json:"id"`
	TicketID       string    `json:"ticket_id,omitempty"`
	Actor          string    `json:"actor"`
	ExpectedState  int64     `json:"expected_state"`
	Type           string    `json:"type"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
}

func ValidateRoles(roles []Role) error {
	seen := map[string]bool{}
	for _, role := range roles {
		id := strings.TrimSpace(role.ID)
		if id == "" || seen[id] {
			return fmt.Errorf("role id must be unique and non-empty: %q", role.ID)
		}
		if strings.TrimSpace(role.Model) == "" {
			return fmt.Errorf("role %q has no model", id)
		}
		seen[id] = true
	}
	return nil
}

func ValidateProposal(p Proposal, roles []Role) error {
	if p.SchemaVersion == "" || p.ID == "" {
		return errors.New("proposal schema_version and id are required")
	}
	switch p.Outcome {
	case OutcomeNoChange, OutcomeNeedsEvidence, OutcomeOutOfScope, OutcomeBlockedBaseline:
		if len(p.Tickets) != 0 {
			return fmt.Errorf("outcome %s cannot contain tickets", p.Outcome)
		}
	case OutcomeProposeChange:
		if len(p.Tickets) == 0 {
			return errors.New("PROPOSE_CHANGE requires tickets")
		}
	default:
		return fmt.Errorf("unknown outcome %q", p.Outcome)
	}
	if err := ValidateRoles(roles); err != nil {
		return err
	}
	return ValidateTickets(p.Tickets, roles)
}

func ValidateTickets(tickets []Ticket, roles []Role) error {
	owners := map[string]bool{}
	for _, role := range roles {
		owners[role.ID] = true
	}
	ids := map[string]bool{}
	for _, t := range tickets {
		if t.ID == "" || ids[t.ID] {
			return fmt.Errorf("ticket id must be unique and non-empty: %q", t.ID)
		}
		if !owners[t.Owner] {
			return fmt.Errorf("ticket %s has unknown owner %q", t.ID, t.Owner)
		}
		if strings.TrimSpace(t.Type) == "" || len(t.Criteria) == 0 || len(t.Outputs) == 0 {
			return fmt.Errorf("ticket %s requires type, criteria, and outputs", t.ID)
		}
		ids[t.ID] = true
	}
	for _, t := range tickets {
		for _, dep := range t.DependsOn {
			if dep == t.ID {
				return fmt.Errorf("ticket %s depends on itself", t.ID)
			}
			if !ids[dep] {
				return fmt.Errorf("ticket %s depends on missing ticket %s", t.ID, dep)
			}
		}
	}
	state := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("dependency cycle includes %s", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, t := range tickets {
			if t.ID == id {
				for _, dep := range t.DependsOn {
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

func ReadyTickets(tickets []Ticket) []Ticket {
	done := map[string]bool{}
	for _, t := range tickets {
		if t.Status == "verified" || t.Status == "integrated" {
			done[t.ID] = true
		}
	}
	var ready []Ticket
	for _, t := range tickets {
		if t.Status != "pending" && t.Status != "revision_required" {
			continue
		}
		ok := true
		for _, dep := range t.DependsOn {
			if !done[dep] {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, t)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	return ready
}

func Hash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func SafeRelativePath(root, candidate string) (string, error) {
	if filepath.IsAbs(candidate) {
		return "", errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(candidate)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(filepath.Join(rootAbs, clean))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace")
	}
	// Reject existing symlink components so a permitted-looking path cannot
	// redirect reads or writes outside the workspace.
	cur := rootAbs
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if info, statErr := os.Lstat(cur); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink paths are not allowed")
		}
	}
	return filepath.ToSlash(rel), nil
}
