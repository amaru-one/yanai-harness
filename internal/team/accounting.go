package team

import (
	"context"
	"fmt"
	"time"

	"github.com/yanai/yanai-harness/internal/openrouter"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// beginWork brackets active command time; the CLI workspace lock excludes a
// second process while the heartbeat lease is alive.
func (r *Runner) beginWork(ctx context.Context, cycle int) (context.Context, func() error, error) {
	if r.Workspace.Store == nil {
		return ctx, func() error { return nil }, fmt.Errorf("model work requires the workflow store")
	}
	s := r.Workspace.Store
	if err := s.EnsureBudget(cycle, r.Cfg.Execution); err != nil {
		return ctx, func() error { return nil }, err
	}
	id, deadline, err := s.StartSession(cycle)
	if err != nil {
		return ctx, func() error { return nil }, err
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.HeartbeatSession(id); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	r.cycle = cycle
	r.ticket = ""
	return ctx, func() error {
		close(stop)
		cancel()
		<-done
		return s.EndSession(id, false)
	}, nil
}
func (r *Runner) observer(ctx context.Context, role, model, kind string, maxOutput int) openrouter.Observer {
	s := r.Workspace.Store
	var id string
	return openrouter.Observer{
		Before: func(body []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if s == nil || r.cycle == 0 {
				return fmt.Errorf("no active durable budget")
			}
			if r.guard != nil {
				if err := r.guard(); err != nil {
					return err
				}
			}
			if err := s.EnsureBudget(r.cycle, r.Cfg.Execution); err != nil {
				return err
			}
			// Byte count of the entire encoded request plus framing allowance is a
			// conservative bound for these text-only requests, not a tokenizer estimate.
			input := int64(len(body)) + 1024
			output := int64(maxOutput)
			if output <= 0 {
				return fmt.Errorf("max_tokens must be positive")
			}
			cost, err := r.Cfg.Execution.ReserveCost(model, input, output)
			if err != nil {
				return err
			}
			id, err = s.ReserveCall(workflow.Reservation{AttemptInput: workflow.AttemptInput{Cycle: r.cycle, TicketID: r.ticket, Role: role, Kind: kind, RequestHash: workflow.Digest(string(body))}, Model: model, Tokens: input + output, Cost: cost})
			return err
		},
		After: func(result openrouter.Result) error {
			state := workflow.AttemptCompleted
			message := ""
			if result.Err != nil {
				state = workflow.AttemptFailed
				message = result.Err.Error()
			}
			if result.Uncertain {
				state = workflow.AttemptUnknown
			}
			if r.captureResponse != nil && result.Err == nil && !result.Uncertain {
				if err := r.captureResponse(result.Text); err != nil {
					return err
				}
			}
			u := result.Usage
			if err := s.SettleCall(id, workflow.CallResult{State: state, ResponseHash: workflow.Digest(result.Text), Usage: workflow.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}, UsageKnown: result.UsageKnown, Cost: u.Cost, ProviderID: result.ProviderID, FinishReason: result.FinishReason, Error: message}); err != nil {
				return err
			}
			b, err := s.Budget(r.cycle)
			if err != nil {
				return err
			}
			if b.Tokens > b.Policy.MaxTokens || b.Cost > b.Policy.MaxCostUSD {
				return fmt.Errorf("provider usage exceeded configured budget; further work is blocked")
			}
			if result.Uncertain {
				return fmt.Errorf("attempt %s has uncertain dispatch; reconcile billing and explicitly acknowledge retry risk", id)
			}
			if !result.UsageKnown || u.Cost == nil {
				return fmt.Errorf("attempt %s billing/usage unknown; use reconcile-attempt before further paid work", id)
			}
			return nil
		},
	}
}
