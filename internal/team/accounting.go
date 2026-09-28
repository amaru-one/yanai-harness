package team

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yanai/yanai-harness/internal/openrouter"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// beginWork brackets active command time charged to one budget bucket; the
// CLI workspace lock excludes a second process while the heartbeat lease is
// alive.
func (r *Runner) beginWork(ctx context.Context, cycle int, bucket string, policy workflow.ExecutionPolicy) (context.Context, func() error, error) {
	if r.Workspace.Store == nil {
		return ctx, func() error { return nil }, fmt.Errorf("model work requires the workflow store")
	}
	s := r.Workspace.Store
	if err := s.EnsureBucket(cycle, bucket, policy); err != nil {
		return ctx, func() error { return nil }, err
	}
	id, deadline, err := s.StartBucketSession(cycle, bucket)
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

// observer brackets every physical provider request, including retries,
// with a reservation against the bucket's budget and its settlement.
func (r *Runner) observer(ctx context.Context, role, model, kind string, maxOutput int, bucket string, policy workflow.ExecutionPolicy) openrouter.Observer {
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
			if err := s.EnsureBucket(r.cycle, bucket, policy); err != nil {
				return err
			}
			// The loop's estimate is the provider's native count of the previous
			// turn plus a byte bound on what was added since (or an exact probe).
			// Without it, the byte count of the whole request is the bound.
			input := r.inputEstimate
			if input <= 0 {
				input = int64(len(body)) + 1024
			}
			output := int64(maxOutput)
			if output <= 0 {
				return fmt.Errorf("max_tokens must be positive")
			}
			cost, err := policy.ReserveCost(model, input, output)
			if err != nil {
				return err
			}
			id, err = s.ReserveCall(workflow.Reservation{AttemptInput: workflow.AttemptInput{Cycle: r.cycle, TicketID: r.ticket, Role: role, Kind: kind, RequestHash: workflow.Digest(string(body))}, Bucket: bucket, Model: model, Tokens: input + output, Cost: cost})
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
			// The answer is durable before anything acts on it.
			if r.captureResponse != nil && result.Err == nil && !result.Uncertain {
				raw, err := json.Marshal(result.Message)
				if err != nil {
					return err
				}
				if err := r.captureResponse(string(raw)); err != nil {
					return err
				}
			}
			u := result.Usage
			if err := s.SettleCall(id, workflow.CallResult{State: state, ResponseHash: workflow.Digest(result.Text), Usage: workflow.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}, UsageKnown: result.UsageKnown, Cost: u.Cost, ProviderID: result.ProviderID, FinishReason: result.FinishReason, Error: message}); err != nil {
				return err
			}
			b, err := s.BucketBudget(r.cycle, bucket)
			if err != nil {
				return err
			}
			if b.Tokens > b.Policy.MaxTokens || b.Cost > b.Policy.MaxCostUSD {
				return fmt.Errorf("provider usage exceeded the %s budget; further work is blocked", bucket)
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
