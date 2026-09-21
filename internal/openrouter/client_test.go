package openrouter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yanai/yanai-harness/internal/workflow"
	"path/filepath"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func TestEveryRetryHasReservationAndUsage(t *testing.T) {
	s, err := workflow.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateCycle(1, workflow.Product)
	p := workflow.ExecutionPolicy{MaxCalls: 2, MaxTokens: 1000, MaxCostUSD: 1, MaxActiveSeconds: 60}
	if err = s.EnsureBudget(1, p); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := &Client{baseURL: "https://fake.invalid", retries: 1, http: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return reply(429, `{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"cost":0},"error":{"message":"rate limit"}}`), nil
		}
		return reply(200, `{"id":"generation-2","usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7,"cost":0.1},"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`), nil
	})}}
	var id string
	obs := Observer{Before: func(body []byte) error {
		var err error
		id, err = s.ReserveCall(workflow.Reservation{AttemptInput: workflow.AttemptInput{Cycle: 1, RequestHash: workflow.Digest(string(body))}, Tokens: 100, Cost: .2})
		return err
	}, After: func(r Result) error {
		state := workflow.AttemptCompleted
		if r.Err != nil {
			state = workflow.AttemptFailed
		}
		return s.SettleCall(id, workflow.CallResult{State: state, UsageKnown: r.UsageKnown, Usage: workflow.Usage{TotalTokens: r.Usage.TotalTokens}, Cost: r.Usage.Cost, ProviderID: r.ProviderID})
	}}
	text, _, err := c.Chat(context.Background(), "model", []Message{{Role: "user", Content: "test"}}, 0, 10, obs)
	if err != nil || text != "ok" {
		t.Fatalf("%s %v", text, err)
	}
	b, _ := s.Budget(1)
	if b.Calls != 2 || calls != 2 || b.Tokens != 7 || b.Cost != .1 {
		t.Fatalf("calls %d budget %+v", calls, b)
	}
	if _, _, err = c.Chat(context.Background(), "model", nil, 0, 10, obs); err == nil || calls != 2 {
		t.Fatal("exhaustion sent another request")
	}
}
func TestTruncationAndAmbiguousTransportPreserveOutcome(t *testing.T) {
	for _, kind := range []string{"truncated", "network", "missing usage"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			var observed Result
			c := &Client{baseURL: "https://fake.invalid", retries: 3, http: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				switch kind {
				case "network":
					return nil, fmt.Errorf("connection lost after dispatch")
				case "missing usage":
					return reply(200, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`), nil
				default:
					return reply(200, `{"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20,"cost":0.2},"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`), nil
				}
			})}}
			_, _, err := c.Chat(context.Background(), "model", nil, 0, 10, Observer{Before: func([]byte) error { return nil }, After: func(r Result) error {
				observed = r
				if !r.UsageKnown || r.Usage.Cost == nil {
					return fmt.Errorf("billing unresolved")
				}
				return nil
			}})
			if err == nil || calls != 1 {
				t.Fatalf("unexpected retry/success %d %v", calls, err)
			}
			if kind == "truncated" && (!observed.UsageKnown || observed.Usage.TotalTokens != 20 || *observed.Usage.Cost != .2) {
				t.Fatal("discarded truncated usage")
			}
			if kind == "network" && !observed.Uncertain {
				t.Fatal("network outcome assumed known")
			}
			if kind == "missing usage" && (observed.UsageKnown || observed.Usage.Cost != nil) {
				t.Fatal("unknown cost treated as zero")
			}
		})
	}
}

func TestDeadlineStopsDispatchAndDoesNotRetryUncertainCall(t *testing.T) {
	calls := 0
	var observed Result
	c := &Client{baseURL: "https://fake.invalid", retries: 3, http: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := c.Chat(ctx, "model", nil, 0, 10, Observer{Before: func([]byte) error { return nil }, After: func(r Result) error { observed = r; return nil }})
	if err == nil || calls != 1 || !observed.Uncertain {
		t.Fatalf("deadline outcome: calls=%d result=%+v err=%v", calls, observed, err)
	}
	if _, _, err = c.Chat(ctx, "model", nil, 0, 10, Observer{Before: func([]byte) error { return nil }, After: func(Result) error { return nil }}); err == nil || calls != 1 {
		t.Fatal("expired deadline dispatched another call")
	}
}
