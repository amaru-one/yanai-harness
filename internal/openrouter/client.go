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

// Message is a conversation turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage reports the token consumption of a call.
type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost"`
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
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Chat sends the messages to the model and returns the response text.
func (c *Client) Chat(ctx context.Context, model string, msgs []Message, temp float64, maxTokens int) (string, Usage, error) {
	if c.mock {
		return mockResponse(model, msgs), Usage{}, nil
	}

	body, err := json.Marshal(request{
		Model: model, Messages: msgs, Temperature: temp, MaxTokens: maxTokens,
	})
	if err != nil {
		return "", Usage{}, err
	}

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<attempt)*time.Second + time.Duration(rand.Intn(500))*time.Millisecond
			select {
			case <-ctx.Done():
				return "", Usage{}, ctx.Err()
			case <-time.After(wait):
			}
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

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("openrouter returned %d: %s", resp.StatusCode, truncate(string(data), 400))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return "", Usage{}, fmt.Errorf("openrouter returned %d: %s", resp.StatusCode, truncate(string(data), 800))
		}

		var r response
		if err := json.Unmarshal(data, &r); err != nil {
			lastErr = fmt.Errorf("unreadable response: %w", err)
			continue
		}
		if r.Error != nil {
			return "", Usage{}, fmt.Errorf("openrouter: %s", r.Error.Message)
		}
		if len(r.Choices) == 0 {
			lastErr = fmt.Errorf("openrouter did not return any response")
			continue
		}
		text := strings.TrimSpace(r.Choices[0].Message.Content)
		if text == "" {
			lastErr = fmt.Errorf("the model returned empty text")
			continue
		}
		return text, r.Usage, nil
	}
	return "", Usage{}, fmt.Errorf("failed after %d attempts: %w", c.retries+1, lastErr)
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
	case strings.Contains(last, "SELECCIONA_ARCHIVOS"):
		b.WriteString("NECESITO: -\n")
	case strings.Contains(last, "TAREA_DE_EJECUCION"):
		b.WriteString("## Entregable simulado\n\nContenido de ejemplo.\n\n")
		b.WriteString("=== ARCHIVO: ejemplo.md ===\n# Ejemplo\nArchivo generado en modo simulado.\n=== FIN ARCHIVO ===\n")
	case strings.Contains(last, "CONSOLIDA EL PLAN"):
		b.WriteString("## Plan de implementación (simulado)\n\nResumen del plan.\n\n")
		b.WriteString("### TAREA: T-001\nRESPONSABLE: arquitecto-bd\nTITULO: Modelar evaluación formativa\nDESCRIPCION: Diseñar las tablas de competencias y calificaciones.\nCRITERIOS:\n- Diagrama ER entregado\n- DDL ejecutable\nDEPENDE_DE: -\n\n")
		b.WriteString("### TAREA: T-002\nRESPONSABLE: disenador\nTITULO: Maqueta de registro de notas\nDESCRIPCION: Pantalla HTML para registrar logros por competencia.\nCRITERIOS:\n- HTML autocontenido\nDEPENDE_DE: T-001\n\n")
		b.WriteString("### TAREA: T-003\nRESPONSABLE: ingeniero\nTITULO: Endpoint de calificaciones\nDESCRIPCION: API en Go y vista Svelte.\nCRITERIOS:\n- Handler con pruebas\nDEPENDE_DE: T-001\n")
	case strings.Contains(last, "ANALIZA LAS ENTREVISTAS"):
		b.WriteString("## Insights (simulado)\n\n- Los docentes pierden tiempo transcribiendo notas al SIAGIE.\n\n")
		b.WriteString("## Propuesta\n\nAgregar exportación a SIAGIE.\n\nVEREDICTO: NUEVO_PLAN\n")
	default:
		b.WriteString("Comentario simulado del agente sobre la propuesta recibida.\n")
	}
	return b.String()
}
