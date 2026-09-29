package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestEmptyAnswerRetriesWithoutTheFailingProvider(t *testing.T) {
	var ignored [][]string
	calls := 0
	c := &Client{baseURL: "https://fake.invalid", retries: 2, http: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		var sent struct {
			Provider *providerPrefs `json:"provider"`
		}
		_ = json.Unmarshal(raw, &sent)
		if sent.Provider != nil {
			ignored = append(ignored, sent.Provider.Ignore)
		} else {
			ignored = append(ignored, nil)
		}
		if calls == 1 {
			return reply(200, `{"id":"g1","provider":"Bad","usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14,"cost":0},"choices":[{"finish_reason":"error","message":{"content":""}}]}`), nil
		}
		return reply(200, `{"id":"g2","provider":"Good","usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8,"cost":0.01},"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`), nil
	})}}
	settled := 0
	obs := Observer{Before: func([]byte) error { return nil }, After: func(Result) error { settled++; return nil }}
	m, _, err := c.ChatTools(context.Background(), "model", []Message{{Role: "user", Content: "go"}}, []Tool{{Type: "function", Function: ToolFunction{Name: "finish", Parameters: json.RawMessage(`{}`)}}}, "", 0, 0, 100, obs)
	if err != nil || len(m.ToolCalls) != 1 {
		t.Fatalf("expected the retry to succeed: %+v %v", m, err)
	}
	if calls != 2 || settled != 2 {
		t.Fatalf("each physical request must be accounted: calls %d settled %d", calls, settled)
	}
	if ignored[0] != nil || len(ignored[1]) != 1 || ignored[1][0] != "Bad" {
		t.Fatalf("retry must exclude the failing provider: %v", ignored)
	}
}
