// Package team runs the parent and worker agents through saved tool loops
// and translates their tool calls into validated, durable actions.
package team

import (
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/openrouter"
	"github.com/yanai/yanai-harness/internal/ws"
)

// Runner binds one workspace, its configuration and a model provider.
type Runner struct {
	Cfg       *config.Config
	Client    *openrouter.Client
	Workspace *ws.Workspace
	// Secrets are exact values (approved check inputs) that must never reach
	// a model. Variable names may; values go only to approved checks.
	Secrets []string

	cycle           int
	ticket          string
	guard           func() error
	captureResponse func(string) error
	// inputEstimate, when positive, is the loop's token estimate for the
	// next request's input; the budget reservation uses it instead of the
	// request's byte count.
	inputEstimate int64
}

// redact removes approved check-input values from text bound for a model.
func (r *Runner) redact(text string) string {
	for _, secret := range r.Secrets {
		if strings.TrimSpace(secret) != "" {
			text = strings.ReplaceAll(text, secret, "[provided to approved checks]")
		}
	}
	return text
}
