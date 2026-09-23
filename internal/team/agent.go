// Package team runs the agents and translates their responses into structures.
package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/openrouter"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// Runner runs a specific agent.
type Runner struct {
	Cfg             *config.Config
	Client          *openrouter.Client
	Workspace       *ws.Workspace
	Verbose         bool
	Redactions      []string
	cycle           int
	ticket          string
	guard           func() error
	captureResponse func(string) error
}

// Run calls the role's model with its system prompt and the given message.
func (r *Runner) Run(ctx context.Context, role, message string) (string, error) {
	ag, err := r.Cfg.Agent(role)
	if err != nil {
		return "", err
	}
	system, err := os.ReadFile(r.Workspace.PromptPath(ag.Prompt))
	if err != nil {
		return "", fmt.Errorf("could not read the prompt for %s: %w", role, err)
	}
	fmt.Fprintf(os.Stderr, "→ %s (%s) thinking...\n", ag.Name, ag.Model)
	msgs := []openrouter.Message{
		{Role: "system", Content: workflow.Redact(string(system), r.Redactions)},
		{Role: "user", Content: workflow.Redact(message, r.Redactions)},
	}
	kind := "role_turn"
	switch {
	case strings.HasPrefix(message, "SELECCIONA_ARCHIVOS"):
		kind = "select_files"
	case strings.HasPrefix(message, "DECISION_JSON"):
		kind = "decision"
	case strings.HasPrefix(message, "TAREA_DE_EJECUCION"):
		kind = "candidate"
	}
	text, usage, err := r.Client.Chat(ctx, ag.Model, msgs, ag.Temperature, ag.MaxTokens, r.observer(ctx, role, ag.Model, kind, ag.MaxTokens))
	if err != nil {
		return "", fmt.Errorf("%s failed: %w", ag.Name, err)
	}
	if usage.TotalTokens > 0 {
		fmt.Fprintf(os.Stderr, "  %s responded (%d tokens)\n", ag.Name, usage.TotalTokens)
	} else {
		fmt.Fprintf(os.Stderr, "  %s responded\n", ag.Name)
	}
	return text, nil
}

// The marker keywords matched below (VEREDICTO, TAREA, RESPONSABLE, TITULO,
// DESCRIPCION, CRITERIOS, DEPENDE_DE, NECESITO, ARCHIVO/FIN ARCHIVO) are a
// wire-format contract with the (Spanish, untouched) prompt templates in
// internal/templates/files/prompts, which instruct the LLM to emit exactly
// these words. They are intentionally left untranslated.
// The verdicts the Product Owner may emit, and the single place that defines
// them. The prompt template, the inline analysis instructions in flow.go and
// the CLI usage text all render from these lists, so the vocabulary cannot
// drift between what the model is told and what the engine accepts.
//
// NUEVO_PLAN and SUFICIENTE are the original pair, kept so cycles and
// workspaces created before the split still parse.
const (
	VerdictProposeChange     = "PROPOSE_CHANGE"
	VerdictNoChangeNeeded    = "NO_CHANGE_NEEDED"
	VerdictNeedsEvidence     = "NEEDS_EVIDENCE"
	VerdictOutOfScope        = "OUT_OF_SCOPE"
	VerdictBlockedByBaseline = "BLOCKED_BY_BASELINE"

	VerdictLegacyNewPlan    = "NUEVO_PLAN"
	VerdictLegacySufficient = "SUFICIENTE"
)

// ChangeVerdicts open a cycle for discussion; TerminalVerdicts close it
// without one. Every verdict belongs to exactly one of the two.
var (
	ChangeVerdicts = []string{VerdictProposeChange, VerdictLegacyNewPlan}

	TerminalVerdicts = []string{
		VerdictNoChangeNeeded,
		VerdictNeedsEvidence,
		VerdictOutOfScope,
		VerdictBlockedByBaseline,
		VerdictLegacySufficient,
	}
)

var reVerdict = regexp.MustCompile(
	`(?mi)^\s*VEREDICTO:\s*(` + strings.Join(append(append([]string{}, ChangeVerdicts...), TerminalVerdicts...), "|") + `)\s*$`)

// Verdict extracts the Product Owner's decision from the analysis text.
func Verdict(text string) string {
	m := reVerdict.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// IsTerminalVerdict reports whether the verdict closes the cycle without a
// plan. The four non-change outcomes are distinct findings — "we lack the
// evidence to judge" is not "the app is sufficient" — and new cycles persist distinct states.
func IsTerminalVerdict(v string) bool {
	for _, t := range TerminalVerdicts {
		if strings.EqualFold(v, t) {
			return true
		}
	}
	return false
}

var (
	reTask     = regexp.MustCompile(`(?mi)^#{2,4}\s*TAREA:\s*([A-Za-z0-9\-_]+)\s*$`)
	reField    = regexp.MustCompile(`(?mi)^\s*(RESPONSABLE|TITULO|TÍTULO|DESCRIPCION|DESCRIPCIÓN|DEPENDE_DE)\s*:\s*(.*)$`)
	reCriteria = regexp.MustCompile(`(?mi)^\s*(CRITERIOS|CRITERIOS_DE_ACEPTACION|CRITERIOS_DE_ACEPTACIÓN)\s*:\s*$`)
	reBullet   = regexp.MustCompile(`^\s*[-*]\s+(.*)$`)
)

// ParseTasks reads the "### TAREA: T-001" blocks from the PO's plan.
func ParseTasks(text string) []ws.Task {
	lines := strings.Split(text, "\n")
	var tasks []ws.Task
	var current *ws.Task
	inCriteria := false

	closeTask := func() {
		if current != nil && current.ID != "" {
			if current.Status == "" {
				current.Status = "pending"
			}
			tasks = append(tasks, *current)
		}
		current = nil
		inCriteria = false
	}

	for _, ln := range lines {
		if m := reTask.FindStringSubmatch(ln); m != nil {
			closeTask()
			current = &ws.Task{ID: strings.ToUpper(strings.TrimSpace(m[1])), Status: "pending"}
			continue
		}
		if current == nil {
			continue
		}
		if reCriteria.MatchString(ln) {
			inCriteria = true
			continue
		}
		if m := reField.FindStringSubmatch(ln); m != nil {
			inCriteria = false
			value := strings.TrimSpace(m[2])
			switch strings.ToUpper(m[1]) {
			case "RESPONSABLE":
				current.Owner = normalizeRole(value)
			case "TITULO", "TÍTULO":
				current.Title = value
			case "DESCRIPCION", "DESCRIPCIÓN":
				current.Description = value
			case "DEPENDE_DE":
				current.DependsOn = parseDeps(value)
			}
			continue
		}
		if inCriteria {
			if m := reBullet.FindStringSubmatch(ln); m != nil {
				current.Criteria = append(current.Criteria, strings.TrimSpace(m[1]))
				continue
			}
			if strings.TrimSpace(ln) == "" {
				continue
			}
			inCriteria = false
		}
		// Loose text after DESCRIPCION accumulates.
		if strings.TrimSpace(ln) != "" && current.Title != "" && !strings.HasPrefix(strings.TrimSpace(ln), "#") {
			if current.Description == "" {
				current.Description = strings.TrimSpace(ln)
			}
		}
	}
	closeTask()
	return tasks
}

func parseDeps(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" || v == "-" || strings.EqualFold(v, "ninguna") || strings.EqualFold(v, "ninguno") || strings.EqualFold(v, "none") {
		return nil
	}
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
	var out []string
	for _, p := range parts {
		p = strings.ToUpper(strings.TrimSpace(p))
		if p != "" && p != "-" {
			out = append(out, p)
		}
	}
	return out
}

func normalizeRole(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.Trim(v, "`*_ ")
	switch {
	case strings.Contains(v, "product") || strings.Contains(v, "owner") || strings.Contains(v, "dueñ") || strings.Contains(v, "duen"):
		return config.RolePO
	case strings.Contains(v, "arquitect") || strings.Contains(v, "architect") || strings.Contains(v, "bd") || strings.Contains(v, "base de datos") || strings.Contains(v, "database") || strings.Contains(v, "db"):
		return config.RoleArchitect
	case strings.Contains(v, "diseñ") || strings.Contains(v, "disen") || strings.Contains(v, "design") || strings.Contains(v, "ux") || strings.Contains(v, "ui"):
		return config.RoleDesigner
	case strings.Contains(v, "ingenier") || strings.Contains(v, "engineer") || strings.Contains(v, "software") || strings.Contains(v, "go") || strings.Contains(v, "svelte"):
		return config.RoleEngineer
	}
	return v
}

var reNeed = regexp.MustCompile(`(?mi)^\s*NECESITO:\s*(.+?)\s*$`)

// ParseNeeds reads the "NECESITO: path" lines an agent uses to request the
// full content of specific files after seeing the repository Index
// (repoctx.Index). "NECESITO: -" or no line at all means the agent didn't
// request any code.
func ParseNeeds(text string) []string {
	var paths []string
	for _, m := range reNeed.FindAllStringSubmatch(text, -1) {
		v := strings.TrimSpace(m[1])
		if v == "" || v == "-" {
			continue
		}
		// Preserve requests so the repository reader can report denied paths.
		paths = append(paths, strings.Trim(v, "`\"' "))
	}
	return paths
}

var reFile = regexp.MustCompile(`(?s)===\s*ARCHIVO:\s*(.+?)\s*===\n(.*?)\n===\s*FIN ARCHIVO\s*===`)

// ExtractedFile is a file an agent asked to write.
type ExtractedFile struct {
	Path    string
	Content string
}

// ExtractFiles finds the "=== ARCHIVO: path ===" blocks in the response.
func ExtractFiles(text string) []ExtractedFile {
	var out []ExtractedFile
	for _, m := range reFile.FindAllStringSubmatch(text, -1) {
		path := cleanPath(m[1])
		if path == "" {
			continue
		}
		out = append(out, ExtractedFile{Path: path, Content: m[2]})
	}
	return out
}

// cleanPath rejects unsafe paths rather than rewriting their destination.
func cleanPath(r string) string {
	r = strings.TrimSpace(strings.Trim(r, "`\"' "))
	if r == "" || r == "." || filepath.IsAbs(r) || strings.ContainsAny(r, "\\\x00\r\n") || filepath.ToSlash(filepath.Clean(r)) != r {
		return ""
	}
	for _, p := range strings.Split(r, "/") {
		if p == ".." {
			return ""
		}
	}
	return r
}
