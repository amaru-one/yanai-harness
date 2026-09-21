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
	"github.com/yanai/yanai-harness/internal/workflow"
)

// Message is a conversation turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
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
	Usage        Usage
	UsageKnown   bool
	ProviderID   string
	FinishReason string
	Err          error
	Uncertain    bool
}

func (c *Client) Chat(ctx context.Context, model string, msgs []Message, temp float64, maxTokens int, o Observer) (string, Usage, error) {
	if o.Before == nil || o.After == nil {
		return "", Usage{}, fmt.Errorf("provider requests require accounting hooks")
	}
	body, err := json.Marshal(request{Model: model, Messages: msgs, Temperature: temp, MaxTokens: maxTokens})
	if err != nil {
		return "", Usage{}, err
	}
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<min(attempt, 6))*time.Second + time.Duration(rand.Intn(500))*time.Millisecond
			select {
			case <-ctx.Done():
				return "", Usage{}, ctx.Err()
			case <-time.After(wait):
			}
		}
		if err = ctx.Err(); err != nil {
			return "", Usage{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return "", Usage{}, err
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
				return "", Usage{}, err
			}
		}
		result := Result{}
		retry := false
		if c.mock {
			zero := 0.0
			result = Result{Text: mockResponse(model, msgs), Usage: Usage{Cost: &zero}, UsageKnown: true, FinishReason: "mock"}
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
						result.Text = strings.TrimSpace(parsed.Choices[0].Message.Content)
						result.FinishReason = parsed.Choices[0].FinishReason
					}
					switch {
					case resp.StatusCode != http.StatusOK:
						result.Err = fmt.Errorf("openrouter returned %d", resp.StatusCode)
						retry = resp.StatusCode == 429 || resp.StatusCode >= 500
					case parsed.Error != nil:
						result.Err = fmt.Errorf("openrouter: %s", parsed.Error.Message)
					case result.FinishReason == "length":
						result.Err = fmt.Errorf("openrouter response was truncated at the token limit")
					case result.Text == "":
						result.Err = fmt.Errorf("the model returned empty text")
					}
				}
			}
		}
		if o.After != nil {
			if err = o.After(result); err != nil {
				return "", result.Usage, err
			}
		}
		if result.Err == nil {
			return result.Text, result.Usage, nil
		}
		if result.Uncertain || !retry || attempt == c.retries {
			return "", result.Usage, result.Err
		}
	}
	return "", Usage{}, fmt.Errorf("retry limit exhausted")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// mockResponse produces plausible text for YANAI_MOCK=1.
func mockResponse(model string, msgs []Message) string {
	var last string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			last = msgs[i].Content
			break
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "_(respuesta simulada — YANAI_MOCK=1, modelo configurado: %s)_\n\n", model)
	switch {
	case strings.Contains(last, "DECISION_JSON"):
		return mockDecision(last)
	case strings.Contains(last, "SELECCIONA_ARCHIVOS"):
		b.WriteString("NECESITO: -\n")
	case strings.Contains(last, "TAREA_DE_EJECUCION"):
		b.WriteString("## Entregable simulado\n\nContenido de ejemplo.\n\n")
		b.WriteString("=== ARCHIVO: yanai-server/ejemplo.md ===\n# Ejemplo\nArchivo generado en modo simulado.\n=== FIN ARCHIVO ===\n")
	case strings.Contains(last, "CONSOLIDA EL PLAN"):
		b.WriteString("## Plan de implementación (simulado)\n\nResumen del plan.\n\n")
		b.WriteString("### TAREA: T-001\nRESPONSABLE: arquitecto-bd\nTITULO: Modelar evaluación formativa\nDESCRIPCION: Diseñar las tablas de competencias y calificaciones.\nCRITERIOS:\n- Diagrama ER entregado\n- DDL ejecutable\nDEPENDE_DE: -\n\n")
		b.WriteString("### TAREA: T-002\nRESPONSABLE: disenador\nTITULO: Revisar el recorrido de registro\nDESCRIPCION: Revisar los pasos que da el docente para registrar una nota de voz y señalar dónde se pierde. Sin archivos de frontend.\nCRITERIOS:\n- Pasos contados desde la pantalla de inicio\nDEPENDE_DE: T-001\n\n")
		b.WriteString("### TAREA: T-003\nRESPONSABLE: ingeniero\nTITULO: Endpoint de calificaciones\nDESCRIPCION: Handler chi y consultas pgx en yanai-server.\nCRITERIOS:\n- Handler con pruebas\nDEPENDE_DE: T-001\n")
	case strings.Contains(last, "ANALIZA LAS ENTREVISTAS"):
		b.WriteString("## Insights (simulado)\n\n- Los docentes repiten a mano la conclusión descriptiva de cada estudiante.\n\n")
		b.WriteString("## Propuesta\n\nPartir de las notas de voz ya registradas para redactar un borrador.\n\nVEREDICTO: PROPOSE_CHANGE\n")
	default:
		b.WriteString("Comentario simulado del agente sobre la propuesta recibida.\n")
	}
	return b.String()
}

func mockDecision(message string) string {
	const marker = "DECISION_CONTEXT_JSON\n"
	_, rest, ok := strings.Cut(message, marker)
	if !ok {
		return "{}"
	}
	var c workflow.DecisionContext
	if err := json.NewDecoder(strings.NewReader(rest)).Decode(&c); err != nil {
		return "{}"
	}
	p := workflow.Proposal{SchemaVersion: "1", ID: "mock-decision", Origin: c.Source.Origin, Outcome: workflow.OutcomeProposeChange, Summary: "Propuesta simulada para probar el circuito, no evidencia de valor del producto.", Scope: []string{c.Scope.Requirements[0].ID}, Inputs: c.Inputs}
	if c.Source.Origin == workflow.Product {
		if len(c.Source.Excerpts) == 0 {
			return "{}"
		}
		e := c.Source.Excerpts[0]
		p.Citations = []workflow.Citation{{ID: "C-1", SourceID: c.Source.ID, Revision: c.Source.Revision, ExcerptID: e.ID, Quote: e.Text}}
		p.Evidence = []string{"C-1"}
		p.Findings = []workflow.Finding{{Kind: "inference", Text: "Esta fuente permite probar una propuesta simulada.", Evidence: p.Evidence}}
	} else {
		p.Rationale = "Comprobar la infraestructura del harness con una tarea técnica explícita."
	}
	for i, owner := range []string{"arquitecto-bd", "disenador", "ingeniero"} {
		t := workflow.Ticket{SchemaVersion: "1", ID: fmt.Sprintf("T-%03d", i+1), Type: p.Origin, Title: "Tarea simulada", Description: "Validar la entrega de un candidato de backend.", Rationale: p.Rationale, Owner: owner, Status: "pending", Evidence: p.Evidence, Scope: p.Scope, Inputs: c.Inputs, Outputs: []string{"yanai-server/ejemplo.md"}, AllowedPaths: []string{"yanai-server"}, BaseCommit: c.BaseCommit, Criteria: []string{"El candidato respeta la ruta de backend."}, MaxAttempts: 2, Revision: 1}
		if i > 0 {
			t.DependsOn = []string{"T-001"}
		}
		p.Tickets = append(p.Tickets, t)
	}
	out, _ := json.Marshal(p)
	return string(out)
}
