// Package openrouter is a minimal client for the OpenRouter chat API.
// It uses no external dependencies.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/config"
)

// Message is a conversation turn. An assistant turn may carry tool calls;
// a "tool" turn carries one call's result, matched by ToolCallID.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a client-tool request in the OpenAI-compatible format that
// OpenRouter uses. Arguments is the JSON text the model produced.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool declares one client tool to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Usage reports the token consumption of a call.
type Usage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	Cost             *float64 `json:"cost"`
	Complete         bool     `json:"-"`
}

func (u *Usage) UnmarshalJSON(data []byte) error {
	type plain Usage
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*u = Usage(value)
	has := func(key string) bool { v, ok := fields[key]; return ok && string(v) != "null" }
	u.Complete = has("prompt_tokens") && has("completion_tokens") && has("total_tokens") && u.TotalTokens >= u.PromptTokens+u.CompletionTokens
	return nil
}

// TruncatedAnswer replaces a tool-loop answer that the provider cut at
// max_tokens. Reasoning models can spend the whole output budget thinking, so
// the turn is recorded (it was paid) and the loop asks for a shorter answer
// instead of stopping the run.
const TruncatedAnswer = "[answer cut at the max_tokens limit before a complete tool call]"

// Client talks to OpenRouter.
type Client struct {
	baseURL string
	apiKey  string
	referer string
	title   string
	retries int
	http    *http.Client
	mock    bool
}

// New builds the client from the config.
// If YANAI_MOCK=1, no network call is made: mock responses are returned so
// the full flow can be tested without spending the key.
func New(c *config.Config) (*Client, error) {
	mock := os.Getenv("YANAI_MOCK") == "1"
	key := c.APIKey()
	if key == "" && !mock {
		return nil, fmt.Errorf("missing the environment variable %s with your OpenRouter key\n"+
			"  export %s=sk-or-...\n"+
			"  (or use YANAI_MOCK=1 to test the flow without calling the API)",
			c.OpenRouter.APIKeyEnv, c.OpenRouter.APIKeyEnv)
	}
	return &Client{
		baseURL: strings.TrimRight(c.OpenRouter.BaseURL, "/"),
		apiKey:  key,
		referer: c.OpenRouter.Referer,
		title:   c.OpenRouter.Title,
		retries: c.OpenRouter.Retries,
		http:    &http.Client{Timeout: time.Duration(c.OpenRouter.TimeoutSec) * time.Second},
		mock:    mock,
	}, nil
}

// IsMock reports whether the client is mocking responses.
func (c *Client) IsMock() bool { return c.mock }

type request struct {
	Model             string          `json:"model"`
	Messages          []Message       `json:"messages"`
	Temperature       float64         `json:"temperature"`
	MaxTokens         int             `json:"max_tokens,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	// probe asks only for the prompt's token count: a cut at max_tokens or an
	// empty answer is the expected outcome, not an error.
	probe bool
}

type response struct {
	ID      string `json:"id"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Observer brackets every physical request, including retries, with durable accounting.
type Observer struct {
	Before func([]byte) error
	After  func(Result) error
}
type Result struct {
	Text         string
	Message      Message
	Usage        Usage
	UsageKnown   bool
	ProviderID   string
	FinishReason string
	Err          error
	Uncertain    bool
}

func (c *Client) Chat(ctx context.Context, model string, msgs []Message, temp float64, maxTokens int, o Observer) (string, Usage, error) {
	reply, usage, err := c.send(ctx, request{Model: model, Messages: msgs, Temperature: temp, MaxTokens: maxTokens}, o)
	return reply.Content, usage, err
}

// ChatTools sends the declared tools and returns the assistant turn, which
// may contain text, tool calls, or both. Parallel tool calls are disabled:
// the harness executes exactly one tool per turn. A non-empty force makes
// the model call that one tool.
func (c *Client) ChatTools(ctx context.Context, model string, msgs []Message, tools []Tool, force string, temp float64, maxTokens int, o Observer) (Message, Usage, error) {
	return c.send(ctx, toolRequest(model, msgs, tools, force, temp, maxTokens), o)
}

// ProbePromptTokens sends the request ChatTools would send, capped at one
// output token, and returns the provider's native usage. It is a paid call
// whose only product is the exact prompt token count.
func (c *Client) ProbePromptTokens(ctx context.Context, model string, msgs []Message, tools []Tool, force string, temp float64, o Observer) (Usage, error) {
	r := toolRequest(model, msgs, tools, force, temp, 1)
	r.probe = true
	_, usage, err := c.send(ctx, r, o)
	return usage, err
}

func toolRequest(model string, msgs []Message, tools []Tool, force string, temp float64, maxTokens int) request {
	parallel := false
	choice := json.RawMessage(`"auto"`)
	if force != "" {
		choice, _ = json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": force}})
	}
	return request{Model: model, Messages: msgs, Temperature: temp, MaxTokens: maxTokens, Tools: tools, ToolChoice: choice, ParallelToolCalls: &parallel}
}

func (c *Client) send(ctx context.Context, r request, o Observer) (Message, Usage, error) {
	if o.Before == nil || o.After == nil {
		return Message{}, Usage{}, fmt.Errorf("provider requests require accounting hooks")
	}
	body, err := json.Marshal(r)
	if err != nil {
		return Message{}, Usage{}, err
	}
	model, msgs, tools := r.Model, r.Messages, r.Tools
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<min(attempt, 6))*time.Second + time.Duration(rand.Intn(500))*time.Millisecond
			select {
			case <-ctx.Done():
				return Message{}, Usage{}, ctx.Err()
			case <-time.After(wait):
			}
		}
		if err = ctx.Err(); err != nil {
			return Message{}, Usage{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return Message{}, Usage{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		if c.referer != "" {
			req.Header.Set("HTTP-Referer", c.referer)
		}
		if c.title != "" {
			req.Header.Set("X-Title", c.title)
		}
		if o.Before != nil {
			if err = o.Before(body); err != nil {
				return Message{}, Usage{}, err
			}
		}
		result := Result{}
		retry := false
		if c.mock {
			zero := 0.0
			message := Message{Role: "assistant"}
			usage := Usage{Cost: &zero}
			switch {
			case r.probe:
				usage.PromptTokens = len(body) / 4
				usage.TotalTokens = usage.PromptTokens
			case len(tools) > 0:
				message = mockToolReply(model, msgs, tools)
			default:
				message.Content = mockResponse(model, msgs)
			}
			result = Result{Text: message.Content, Message: message, Usage: usage, UsageKnown: true, FinishReason: "mock"}
		} else {
			resp, callErr := c.http.Do(req)
			if callErr != nil {
				result.Err = callErr
				result.Uncertain = true
			} else {
				data, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
				resp.Body.Close()
				var parsed response
				if readErr != nil {
					result.Err = readErr
					result.Uncertain = true
				} else if err = json.Unmarshal(data, &parsed); err != nil {
					result.Err = fmt.Errorf("unreadable provider response: %w", err)
					result.Uncertain = true
				} else {
					result.ProviderID = parsed.ID
					if parsed.Usage != nil {
						result.Usage = *parsed.Usage
						result.UsageKnown = parsed.Usage.Complete
					}
					if len(parsed.Choices) > 0 {
						result.Message = parsed.Choices[0].Message
						result.Message.Role = "assistant"
						result.Message.Content = strings.TrimSpace(result.Message.Content)
						result.Text = result.Message.Content
						result.FinishReason = parsed.Choices[0].FinishReason
					}
					switch {
					case resp.StatusCode != http.StatusOK:
						result.Err = fmt.Errorf("openrouter returned %d", resp.StatusCode)
						retry = resp.StatusCode == 429 || resp.StatusCode >= 500
					case parsed.Error != nil:
						result.Err = fmt.Errorf("openrouter: %s", parsed.Error.Message)
					case r.probe:
					case result.FinishReason == "length" && len(r.Tools) > 0:
						result.Message = Message{Role: "assistant", Content: TruncatedAnswer}
						result.Text = TruncatedAnswer
					case result.FinishReason == "length":
						result.Err = fmt.Errorf("openrouter response was truncated at the token limit")
					case result.Text == "" && len(result.Message.ToolCalls) == 0:
						result.Err = fmt.Errorf("the model returned empty text")
					}
				}
			}
		}
		if o.After != nil {
			if err = o.After(result); err != nil {
				return Message{}, result.Usage, err
			}
		}
		if result.Err == nil {
			return result.Message, result.Usage, nil
		}
		if result.Uncertain || !retry || attempt == c.retries {
			return Message{}, result.Usage, result.Err
		}
	}
	return Message{}, Usage{}, fmt.Errorf("retry limit exhausted")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// mockResponse produces plausible text for YANAI_MOCK=1 when no tools are
// declared. Every level 0 agent call declares tools; see mock.go.
func mockResponse(model string, msgs []Message) string {
	return fmt.Sprintf("_(mock response — YANAI_MOCK=1, configured model: %s)_", model)
}
