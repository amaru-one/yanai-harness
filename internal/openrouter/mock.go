package openrouter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// InputMarker precedes the machine-readable input block that every level 0
// agent request carries in its first user message. The mock provider reads
// it back so synthetic answers exercise the real validation paths; a mock
// that hardcoded paths would pass while the contract it is meant to test
// never ran.
const InputMarker = "\nINPUT (JSON):\n"

// MockInput is the subset of an agent's input the mock provider needs.
type MockInput struct {
	Kind          string          `json:"kind"`
	TicketType    string          `json:"ticket_type"`
	Slug          string          `json:"slug"`
	Title         string          `json:"title"`
	Task          string          `json:"task"`
	CriteriaIDs   []string        `json:"criteria_ids"`
	Config        json.RawMessage `json:"config,omitempty"`
	Role          string          `json:"role,omitempty"`
	Outputs       []string        `json:"outputs,omitempty"`
	Checks        []string        `json:"checks,omitempty"`
	CommitScope   string          `json:"commit_scope,omitempty"`
	State         string          `json:"state,omitempty"`
	PromptPathFor string          `json:"prompt_path_for,omitempty"`
	Commits       []string        `json:"commits,omitempty"`
}

func mockInput(msgs []Message) (MockInput, bool) {
	var in MockInput
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		_, raw, ok := strings.Cut(m.Content, InputMarker)
		if !ok {
			continue
		}
		if json.Unmarshal([]byte(raw), &in) == nil {
			return in, true
		}
	}
	return in, false
}

func hasTool(tools []Tool, name string) bool {
	for _, t := range tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

func call(n int, name string, args any) Message {
	raw, _ := json.Marshal(args)
	return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: fmt.Sprintf("mock-call-%d", n), Type: "function", Function: ToolCallFunction{Name: name, Arguments: string(raw)}}}}
}

// mockToolReply answers one turn of a saved tool loop deterministically.
func mockToolReply(model string, msgs []Message, tools []Tool) Message {
	in, ok := mockInput(msgs)
	turn := 0
	for _, m := range msgs {
		if m.Role == "assistant" {
			turn++
		}
	}
	if !ok {
		return Message{Role: "assistant", Content: "mock: no input block"}
	}
	// A forced final turn offers only the worker's terminal tool.
	if len(tools) == 1 && tools[0].Function.Name == "finish" {
		return call(turn, "finish", map[string]any{"result": "blocked", "explanation": "Simulated worker reached its budget or step limit.", "observations": []any{map[string]any{"description": "The simulated worker ran out of budget or steps.", "requirement": "task", "question": "Raise the worker budget and approve again?"}}})
	}
	switch {
	case hasTool(tools, "submit_proposal"):
		return call(turn, "submit_proposal", mockProposal(model, in))
	case hasTool(tools, "submit_state_update"):
		state := strings.TrimRight(in.State, "\n") + fmt.Sprintf("\n- %s (%s): implemented on review branch %s/%s, commits %s (mock, YANAI_MOCK=1).\n", in.Title, in.TicketType, in.TicketType, in.Slug, strings.Join(in.Commits, ", "))
		return call(turn, "submit_state_update", map[string]any{"content": state, "summary": "Mock project state update."})
	case hasTool(tools, "commit"):
		return mockWorkerTurn(turn, msgs, in)
	}
	return Message{Role: "assistant", Content: "mock: unknown tool set"}
}

func mockProposal(model string, in MockInput) map[string]any {
	var cfg map[string]any
	_ = json.Unmarshal(in.Config, &cfg)
	if cfg == nil {
		cfg = map[string]any{}
	}
	execution, _ := cfg["execution"].(map[string]any)
	if execution == nil {
		execution = map[string]any{}
		cfg["execution"] = execution
	}
	execution["commit"] = true
	// Choose like a parent must: rate the task "media", take the first catalog
	// category (alphabetical) and its cheapest option that covers "media".
	const complexity = "media"
	workerModel, category := model, ""
	prices, _ := execution["prices"].(map[string]any)
	cost := func(m string) float64 {
		p, _ := prices[m].(map[string]any)
		in, _ := p["input_usd_per_million"].(float64)
		out, _ := p["output_usd_per_million"].(float64)
		return in + out
	}
	if models, ok := cfg["models"].(map[string]any); ok {
		keys := make([]string, 0, len(models))
		for key := range models {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			category = keys[0]
			entry, _ := models[category].(map[string]any)
			options, _ := entry["options"].([]any)
			best := ""
			for _, raw := range options {
				option, _ := raw.(map[string]any)
				name, _ := option["model"].(string)
				levels, _ := option["difficulty"].([]any)
				for _, l := range levels {
					if l == complexity && (best == "" || cost(name) < cost(best)) {
						best = name
					}
				}
			}
			if best != "" {
				workerModel = best
			}
		}
	}
	var checks []string
	if list, ok := execution["checks"].([]any); ok {
		for _, c := range list {
			if m, ok := c.(map[string]any); ok {
				if id, ok := m["id"].(string); ok {
					checks = append(checks, id)
				}
			}
		}
	}
	output := "yanai-mock/" + in.Slug + ".md"
	if repo, ok := cfg["repo"].(map[string]any); ok {
		if allowed, ok := repo["allowed_paths"].([]any); ok && len(allowed) > 0 {
			if first, ok := allowed[0].(string); ok && first != "." {
				output = first + "/yanai-mock-" + in.Slug + ".md"
			}
		}
	}
	role := "mock-worker"
	agent := map[string]any{"id": role, "name": "Mock worker", "purpose": "Synthetic worker for YANAI_MOCK=1", "model": workerModel, "model_category": category, "temperature": 0.2, "max_tokens": 4000, "max_steps": 20, "prompt": in.PromptPathFor + role + ".md"}
	cfg["agents"] = map[string]any{role: agent}
	return map[string]any{
		"summary": "Mock proposal (YANAI_MOCK=1): a single worker writes an example file.",
		"worker": map[string]any{
			"id": role, "name": "Mock worker", "purpose": "Synthetic worker for YANAI_MOCK=1", "model": workerModel,
			"model_category": category, "model_reason": "The cheapest option in the first category covering medium difficulty (mock selection, YANAI_MOCK=1).",
			"task_complexity": complexity, "complexity_reason": "One new file with tests (mock classification, YANAI_MOCK=1).",
			"temperature": 0.2, "max_tokens": 4000, "max_steps": 20, "base_prompt": "",
			"prompt": "You are a mock worker. Write the declared file, run the checks, and commit. Write prompts and Markdown in English.",
		},
		"task": map[string]any{
			"id": "T-001", "title": in.Title, "description": in.Task, "criteria_ids": in.CriteriaIDs,
			"criteria": []string{}, "outputs": []string{output}, "checks": checks, "max_attempts": 2,
		},
		"commit_scope": map[string]any{"scope": "mock", "module": output},
		"config":       cfg,
		"observations": []any{},
	}
}

type mockResult struct {
	OK       bool     `json:"ok"`
	Error    string   `json:"error"`
	Exists   bool     `json:"exists"`
	SHA256   string   `json:"sha256"`
	Passed   bool     `json:"passed"`
	CheckID  string   `json:"check_id"`
	Commit   string   `json:"commit"`
	Required []string `json:"required_checks"`
	Files    []struct {
		Exists bool   `json:"exists"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

func mockWorkerTurn(turn int, msgs []Message, in MockInput) Message {
	// Rebuild what already happened from the tool transcript.
	var calls []ToolCall
	results := map[string]mockResult{}
	for _, m := range msgs {
		if m.Role == "assistant" {
			calls = append(calls, m.ToolCalls...)
		}
		if m.Role == "tool" {
			var r mockResult
			_ = json.Unmarshal([]byte(m.Content), &r)
			results[m.ToolCallID] = r
		}
	}
	output := ""
	if len(in.Outputs) > 0 {
		output = in.Outputs[0]
	}
	if len(calls) == 0 {
		return call(turn, "read_file", map[string]any{"paths": []string{output}})
	}
	last := calls[len(calls)-1]
	result := results[last.ID]
	if !result.OK {
		return call(turn, "finish", map[string]any{"result": "blocked", "explanation": "Simulated worker stopped: " + result.Error, "observations": []any{map[string]any{"description": "The simulated worker could not continue: " + result.Error, "requirement": "task", "question": "Adjust the configuration or ticket and approve again?"}}})
	}
	switch last.Function.Name {
	case "read_file":
		content := fmt.Sprintf("# %s\n\nFile generated in mock mode (YANAI_MOCK=1).\n", in.Title)
		args := map[string]any{"path": output, "content": content}
		if len(result.Files) > 0 && result.Files[0].Exists {
			args["expected_sha256"] = result.Files[0].SHA256
			content += "\nUpdated.\n"
			args["content"] = content
		} else {
			args["expected_absent"] = true
		}
		return call(turn, "write_file", args)
	case "write_file", "run_check":
		ran := map[string]bool{}
		for i := len(calls) - 1; i >= 0 && calls[i].Function.Name == "run_check"; i-- {
			var a struct {
				CheckID string `json:"check_id"`
			}
			_ = json.Unmarshal([]byte(calls[i].Function.Arguments), &a)
			ran[a.CheckID] = true
		}
		for _, id := range in.Checks {
			if !ran[id] {
				return call(turn, "run_check", map[string]any{"check_id": id})
			}
		}
		message := fmt.Sprintf("%s(%s): %s\n\nMock change (YANAI_MOCK=1).\n\nYanai-Ticket: %s\nYanai-Agent: %s", in.TicketType, in.CommitScope, strings.ToLower(in.Title), in.Slug, in.Role)
		return call(turn, "commit", map[string]any{"message": message})
	case "commit":
		return call(turn, "finish", map[string]any{"result": "completed", "explanation": "Mock change applied, checked, and committed."})
	}
	return call(turn, "finish", map[string]any{"result": "blocked", "explanation": "unexpected mock state"})
}
