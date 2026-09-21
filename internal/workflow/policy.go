package workflow

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
)

// ModelPrice is an operator supplied upper bound, in USD per million tokens.
// It is an admission estimate, not a claim about the provider's final invoice.
type ModelPrice struct {
	Input  float64 `json:"input_usd_per_million"`
	Output float64 `json:"output_usd_per_million"`
}
type Check struct {
	ID             string   `json:"id"`
	Args           []string `json:"args"`
	Dir            string   `json:"dir"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}
type ExecutionPolicy struct {
	MaxTokens        int64                 `json:"max_tokens"`
	MaxCostUSD       float64               `json:"max_cost_usd"`
	MaxActiveSeconds int64                 `json:"max_active_seconds"`
	MaxCalls         int                   `json:"max_calls"`
	MaxRepairs       int                   `json:"max_repairs"`
	Prices           map[string]ModelPrice `json:"prices"`
	Checks           []Check               `json:"checks"`
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
	seen := map[string]bool{}
	for _, c := range p.Checks {
		if c.ID == "" || seen[c.ID] || len(c.Args) == 0 || c.Args[0] == "" || c.TimeoutSeconds <= 0 || c.Dir == "" || filepath.IsAbs(c.Dir) || strings.ContainsAny(c.Dir, "\\\x00\r\n") || filepath.ToSlash(filepath.Clean(c.Dir)) != c.Dir || !(c.Dir == "yanai-server" || strings.HasPrefix(c.Dir, "yanai-server/")) {
			return fmt.Errorf("invalid or duplicate required check %q", c.ID)
		}
		for _, arg := range c.Args {
			if strings.ContainsRune(arg, '\x00') {
				return fmt.Errorf("check %s contains a NUL argument", c.ID)
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
