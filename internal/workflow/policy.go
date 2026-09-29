package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strings"
)

// ModelPrice is an operator supplied upper bound, in USD per million tokens.
// It is an admission estimate, not a claim about the provider's final invoice.
type ModelPrice struct {
	Input  float64 `json:"input_usd_per_million"`
	Output float64 `json:"output_usd_per_million"`
}

// CheckTool is an installed executable a check runs by name. The harness
// resolves a proposed check's program from PATH; the human approves the
// resolved binary in the review.
type CheckTool struct {
	Binary string `json:"binary"`
}

type Check struct {
	ID             string   `json:"id"`
	Args           []string `json:"args"`
	Dir            string   `json:"dir"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	RequiredEnv    []string `json:"required_env,omitempty"`
	PostgresURLVar string   `json:"postgres_url_var,omitempty"`
	Evidence       string   `json:"evidence,omitempty"`
	SuccessPattern string   `json:"success_pattern,omitempty"`
	FailurePattern string   `json:"failure_pattern,omitempty"`
}

// Check refuses removed or misspelled fields instead of silently weakening a
// required check when an older workspace is opened.
func (c *Check) UnmarshalJSON(data []byte) error {
	type wire Check
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*wire)(c))
}

type ExecutionPolicy struct {
	MaxTokens        int64                 `json:"max_tokens"`
	MaxCostUSD       float64               `json:"max_cost_usd"`
	MaxActiveSeconds int64                 `json:"max_active_seconds"`
	MaxCalls         int                   `json:"max_calls"`
	MaxRepairs       int                   `json:"max_repairs"`
	Prices           map[string]ModelPrice `json:"prices"`
	Checks           []Check               `json:"checks"`
	Tools            map[string]CheckTool  `json:"tools,omitempty"`
	// Commit permits Conventional Commits on the cycle's own ticket branch
	// only, through the controlled Git service. Merge, deploy and destructive
	// database operations remain unavailable.
	Commit        bool `json:"commit"`
	Merge         bool `json:"merge"`
	Deploy        bool `json:"deploy"`
	DestructiveDB bool `json:"destructive_db"`
}

// PlanningBudget bounds the parent agent's spending for one cycle. It is a
// separate bucket from the worker's ExecutionPolicy; both are reported and
// neither is ever reset by a policy change or a restart.
type PlanningBudget struct {
	MaxTokens        int64                 `json:"max_tokens"`
	MaxCostUSD       float64               `json:"max_cost_usd"`
	MaxActiveSeconds int64                 `json:"max_active_seconds"`
	MaxCalls         int                   `json:"max_calls"`
	Prices           map[string]ModelPrice `json:"prices"`
}

func (p PlanningBudget) Validate() error {
	if p.MaxTokens <= 0 || p.MaxTokens > 1e12 || !finite(p.MaxCostUSD) || p.MaxCostUSD <= 0 || p.MaxActiveSeconds <= 0 || p.MaxActiveSeconds > 31536000 || p.MaxCalls <= 0 {
		return fmt.Errorf("finite positive max_tokens, max_cost_usd, max_active_seconds and max_calls are required")
	}
	for model, price := range p.Prices {
		if model == "" || !finite(price.Input) || !finite(price.Output) {
			return fmt.Errorf("invalid model price bound")
		}
	}
	return nil
}

// Policy expresses the planning budget in the shape the shared budget ledger
// stores; the fields a parent cannot use are zero.
func (p PlanningBudget) Policy() ExecutionPolicy {
	return ExecutionPolicy{MaxTokens: p.MaxTokens, MaxCostUSD: p.MaxCostUSD, MaxActiveSeconds: p.MaxActiveSeconds, MaxCalls: p.MaxCalls, Prices: p.Prices}
}

// Exceeds reports whether any limit of p is higher than the matching limit of
// other; a parent may lower its future budget but never raise it.
func (p PlanningBudget) Exceeds(other PlanningBudget) bool {
	return p.MaxTokens > other.MaxTokens || p.MaxCostUSD > other.MaxCostUSD || p.MaxActiveSeconds > other.MaxActiveSeconds || p.MaxCalls > other.MaxCalls
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func (p ExecutionPolicy) Validate() error {
	if p.MaxTokens <= 0 || p.MaxTokens > 1e12 || !finite(p.MaxCostUSD) || p.MaxCostUSD <= 0 || p.MaxActiveSeconds <= 0 || p.MaxActiveSeconds > 31536000 || p.MaxCalls <= 0 || p.MaxRepairs < 0 {
		return fmt.Errorf("configure execution: finite positive max_tokens, max_cost_usd, max_active_seconds, max_calls and nonnegative max_repairs are required")
	}
	if p.Merge || p.Deploy || p.DestructiveDB {
		return fmt.Errorf("merge, deploy and destructive_db are not supported by this executor")
	}
	for model, price := range p.Prices {
		if model == "" || !finite(price.Input) || !finite(price.Output) {
			return fmt.Errorf("invalid model price bound")
		}
	}
	for name, tool := range p.Tools {
		if err := ValidateCheckTool(name, tool); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, c := range p.Checks {
		if c.ID == "" || seen[c.ID] || len(c.Args) == 0 || c.Args[0] == "" || c.TimeoutSeconds <= 0 || c.Dir == "" || filepath.IsAbs(c.Dir) || strings.ContainsAny(c.Dir, "\\\x00\r\n") || filepath.ToSlash(filepath.Clean(c.Dir)) != c.Dir || (c.Dir == ".." || strings.HasPrefix(c.Dir, "../")) {
			return fmt.Errorf("invalid or duplicate required check %q", c.ID)
		}
		for _, arg := range c.Args {
			if strings.ContainsRune(arg, '\x00') {
				return fmt.Errorf("check %s contains a NUL argument", c.ID)
			}
		}
		if err := ValidateCheckEvidence(c); err != nil {
			return err
		}
		envSeen := map[string]bool{}
		for _, name := range c.RequiredEnv {
			if !ValidCheckEnvName(name) || envSeen[name] {
				return fmt.Errorf("check %s has an invalid or duplicate required environment name", c.ID)
			}
			envSeen[name] = true
		}
		if c.PostgresURLVar != "" && !envSeen[c.PostgresURLVar] {
			return fmt.Errorf("check %s postgres_url_var must name a required_env entry", c.ID)
		}
		if c.Args[0] != "go" {
			if err := ValidateCheckArguments(c.Args); err != nil {
				return fmt.Errorf("check %s: %w", c.ID, err)
			}
			if _, ok := p.Tools[c.Args[0]]; !ok && !RepositoryProgram(c.Args[0]) {
				return fmt.Errorf("check %s runs %q, which is neither in execution.tools nor found on PATH", c.ID, c.Args[0])
			}
		}
		seen[c.ID] = true
	}
	return nil
}

// Check inputs may name project-specific test variables, never process or
// language-runtime controls that could redirect an approved executable.
func ValidCheckEnvName(name string) bool {
	if !regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`).MatchString(name) || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "GIT_") {
		return false
	}
	switch name {
	case "PATH", "HOME", "TMPDIR", "GOTMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH", "GOENV", "GOWORK", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB", "GOVCS", "GOTELEMETRY", "GOFLAGS", "CGO_ENABLED", "LANG", "TZ", "PYTHONPATH", "PYTHONHOME", "NODE_OPTIONS", "RUSTFLAGS", "OPENROUTER_API_KEY", "BASH_ENV", "ENV", "CDPATH", "RUBYOPT", "PERL5OPT":
		return false
	}
	return true
}
func (p ExecutionPolicy) ReserveCost(model string, input, output int64) (float64, error) {
	price, ok := p.Prices[model]
	if !ok {
		return 0, fmt.Errorf("configure execution.prices upper bounds for %s before paid calls", model)
	}
	cost := (float64(input)*price.Input + float64(output)*price.Output) / 1e6
	if !finite(cost) {
		return 0, fmt.Errorf("invalid cost reservation")
	}
	return cost, nil
}

// Checks may run any program: the human approves every check command in the
// review, and that approval is the gate. Checks execute project code; this is
// not a sandbox. A tool is a simple name bound to an absolute executable.
func ValidateCheckTool(name string, tool CheckTool) error {
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.+-]*$`).MatchString(name) || name == "go" || !filepath.IsAbs(tool.Binary) || strings.ContainsAny(tool.Binary, "\x00\r\n") {
		return fmt.Errorf("invalid check tool %q: a name of letters, digits, '.', '_', '+' or '-' bound to an absolute binary path", name)
	}
	return nil
}

// RepositoryProgram reports whether a check runs a program the repository
// itself provides (a relative path such as ./scripts/smoke.sh) rather than a
// tool installed on the machine.
func RepositoryProgram(program string) bool { return strings.Contains(program, "/") }

func ValidateCheckArguments(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return fmt.Errorf("check command is empty")
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("NUL in check argument")
		}
	}
	if RepositoryProgram(args[0]) {
		clean := filepath.ToSlash(filepath.Clean(args[0]))
		if filepath.IsAbs(args[0]) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("check program %q must be a program name found on PATH or a relative path inside the repository", args[0])
		}
	}
	return nil
}
func ValidateCheckEvidence(c Check) error {
	builtin := len(c.Args) > 1 && c.Args[0] == "go"
	if c.Evidence != "exit_code" && c.Evidence != "output" && !(builtin && c.Evidence == "") {
		return fmt.Errorf("check %s requires evidence: exit_code or output", c.ID)
	}
	if c.Evidence == "output" && c.SuccessPattern == "" {
		return fmt.Errorf("output evidence requires success_pattern")
	}
	for _, pattern := range []string{c.SuccessPattern, c.FailurePattern} {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("check %s evidence pattern: %w", c.ID, err)
		}
	}
	return nil
}

// CheckOutputFailure evaluates only the evidence the human approved. A generic
// exit_code check claims command success, not that a particular test suite ran.
func CheckOutputFailure(c Check, output string) string {
	failure, err := regexp.Compile(c.FailurePattern)
	if err != nil {
		return "invalid failure evidence pattern"
	}
	success, err := regexp.Compile(c.SuccessPattern)
	if err != nil {
		return "invalid success evidence pattern"
	}
	if c.FailurePattern != "" && failure.MatchString(output) {
		return "approved failure pattern matched check output"
	}
	if c.SuccessPattern != "" && !success.MatchString(output) {
		return "approved success pattern missing from check output"
	}
	return ""
}
