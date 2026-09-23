package workflow

import (
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

// CheckTool is an operator-installed executable, never a model-supplied command.
type CheckTool struct {
	Binary string `json:"binary"`
}

type Check struct {
	ID               string   `json:"id"`
	Args             []string `json:"args"`
	Dir              string   `json:"dir"`
	TimeoutSeconds   int      `json:"timeout_seconds"`
	RequiresPostgres bool     `json:"requires_postgres,omitempty"`
	Evidence         string   `json:"evidence,omitempty"`
	SuccessPattern   string   `json:"success_pattern,omitempty"`
	FailurePattern   string   `json:"failure_pattern,omitempty"`
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
	// These permissions are deliberately unavailable until a controlled executor exists.
	Commit        bool `json:"commit"`
	Merge         bool `json:"merge"`
	Deploy        bool `json:"deploy"`
	DestructiveDB bool `json:"destructive_db"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func (p ExecutionPolicy) Validate() error {
	if p.MaxTokens <= 0 || p.MaxTokens > 1e12 || !finite(p.MaxCostUSD) || p.MaxCostUSD <= 0 || p.MaxActiveSeconds <= 0 || p.MaxActiveSeconds > 31536000 || p.MaxCalls <= 0 || p.MaxRepairs < 0 {
		return fmt.Errorf("configure execution: finite positive max_tokens, max_cost_usd, max_active_seconds, max_calls and nonnegative max_repairs are required")
	}
	if p.Commit || p.Merge || p.Deploy || p.DestructiveDB {
		return fmt.Errorf("commit, merge, deploy and destructive_db are not supported by this executor")
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
		if c.Args[0] != "go" {
			if _, ok := p.Tools[c.Args[0]]; !ok {
				return fmt.Errorf("check %s uses a tool not in execution.tools", c.ID)
			}
			if err := ValidateCheckArguments(c.Args); err != nil {
				return err
			}
		}
		seen[c.ID] = true
	}
	return nil
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

// Direct shell, repository-control and destructive commands are unavailable.
// Configured runtimes still execute trusted project code; this is not a sandbox.
func ForbiddenCheckProgram(name string) bool {
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(name)), ".exe") {
	case "commit", "merge", "deploy", "destructive_db", "git", "sh", "bash", "dash", "zsh", "fish", "cmd", "powershell", "pwsh", "sudo", "doas", "env", "xargs", "rm", "rmdir", "mv", "cp", "dd", "chmod", "chown":
		return true
	}
	return false
}
func ValidateCheckTool(name string, tool CheckTool) error {
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`).MatchString(name) || name == "go" || ForbiddenCheckProgram(name) || !filepath.IsAbs(tool.Binary) || strings.ContainsAny(tool.Binary, "\x00\r\n") || ForbiddenCheckProgram(tool.Binary) {
		return fmt.Errorf("invalid or forbidden check tool %q", name)
	}
	return nil
}
func ValidateCheckArguments(args []string) error {
	if len(args) == 0 || ForbiddenCheckProgram(args[0]) {
		return fmt.Errorf("forbidden check command")
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("NUL in check argument")
		}
		switch strings.ToLower(arg) {
		case "commit", "merge", "deploy", "destructive_db":
			return fmt.Errorf("check action %q is unavailable", arg)
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
