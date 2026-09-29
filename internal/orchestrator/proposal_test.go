package orchestrator

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yanai/yanai-harness/internal/config"
)

func TestMergePatch(t *testing.T) {
	target := map[string]any{"execution": map[string]any{"commit": false, "max_calls": 10.0}, "keep": "x", "drop": "y"}
	patch := map[string]any{"execution": map[string]any{"commit": true}, "drop": nil}
	got := mergePatch(target, patch)
	execution := got["execution"].(map[string]any)
	if execution["commit"] != true || execution["max_calls"] != 10.0 || got["keep"] != "x" {
		t.Fatalf("merge lost or ignored values: %v", got)
	}
	if _, ok := got["drop"]; ok {
		t.Fatalf("null did not remove the key: %v", got)
	}
}

func TestMergedConfigBuildsAgentsAndRejectsThem(t *testing.T) {
	live := json.RawMessage(`{"project":"p","agents":{"old":{"id":"old"}}}`)
	agent := config.Agent{ID: "backend", Name: "Backend", Model: "m", MaxTokens: 32000, Prompt: "prompts/backend.md"}
	for _, changes := range []string{`{}`, `null`, ``} {
		out, err := mergedConfig(live, json.RawMessage(changes), agent)
		if err != nil {
			t.Fatalf("changes %q: %v", changes, err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(out, &cfg); err != nil {
			t.Fatal(err)
		}
		agents := cfg["agents"].(map[string]any)
		if len(agents) != 1 || agents["backend"] == nil || cfg["project"] != "p" {
			t.Fatalf("changes %q: agents must be exactly the worker: %v", changes, cfg)
		}
	}
	if _, err := mergedConfig(live, json.RawMessage(`{"agents":{}}`), agent); err == nil {
		t.Fatal("accepted agents in config_changes")
	}
}

func TestValidateReportsIndependentErrorsTogether(t *testing.T) {
	c := Context{Config: &config.Config{}, ConfigRaw: json.RawMessage(`not json`)}
	p := Proposal{
		Summary:     "s",
		Worker:      WorkerSpec{ID: "backend", Name: "n", Purpose: "p", Prompt: "# Worker\n\nDo it.", MaxTokens: 16000, MaxSteps: 10, TaskComplexity: "medium", ComplexityReason: "r", ModelReason: "r"},
		Task:        TaskSpec{ID: "T-1", Title: "t", Description: "d", Outputs: []string{"cmd/api/main.go"}, Checks: []string{"test"}, MaxAttempts: 2},
		CommitScope: CommitScope{Scope: "Backend/Logging", Module: "internal/logging"},
	}
	_, err := Validate(p, c, t.TempDir(), nil)
	if err == nil {
		t.Fatal("invalid proposal accepted")
	}
	for _, want := range []string{"max_tokens must be at least", "commit scope", "outside the commit module", "model_category"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection does not mention %q:\n%v", want, err)
		}
	}
}

func TestHighDifficultyNeedsFiveAttempts(t *testing.T) {
	c := Context{Config: &config.Config{}, ConfigRaw: json.RawMessage(`not json`)}
	p := Proposal{
		Summary:     "s",
		Worker:      WorkerSpec{ID: "backend", Name: "n", Purpose: "p", Prompt: "# Worker\n\nDo it.", MaxTokens: 32000, MaxSteps: 10, TaskComplexity: "high", ComplexityReason: "r", ModelReason: "r"},
		Task:        TaskSpec{ID: "T-1", Title: "t", Description: "d", Outputs: []string{"a/b.go"}, Checks: []string{"test"}, MaxAttempts: 3},
		CommitScope: CommitScope{Scope: "backend", Module: "a"},
	}
	_, err := Validate(p, c, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "max_attempts must be at least 5 for high") {
		t.Fatalf("3 attempts for a high task must be rejected: %v", err)
	}
	p.Task.MaxAttempts = 5
	if _, err = Validate(p, c, t.TempDir(), nil); err != nil && strings.Contains(err.Error(), "max_attempts") {
		t.Fatalf("5 attempts for a high task must be accepted: %v", err)
	}
	p.Worker.TaskComplexity, p.Task.MaxAttempts = "medium", 3
	if _, err = Validate(p, c, t.TempDir(), nil); err != nil && strings.Contains(err.Error(), "max_attempts") {
		t.Fatalf("medium tasks keep their current minimum: %v", err)
	}
}

func TestResolveCheckToolsBindsProgramsFromPath(t *testing.T) {
	saved := lookPath
	defer func() { lookPath = saved }()
	lookPath = func(name string) (string, error) {
		if name == "docker" {
			return "/usr/local/bin/docker", nil
		}
		return "", errors.New("not found")
	}
	cfg := map[string]any{"execution": map[string]any{"checks": []any{
		map[string]any{"id": "compose", "args": []any{"docker", "compose", "config", "--quiet"}, "dir": "."},
		map[string]any{"id": "smoke", "args": []any{"./scripts/smoke.sh"}, "dir": ".", "evidence": "output"},
		map[string]any{"id": "vet", "args": []any{"go", "vet", "./..."}, "dir": "app"},
		map[string]any{"id": "missing", "args": []any{"nosuchtool"}, "dir": "."},
	}}}
	resolveCheckTools(cfg)
	execution := cfg["execution"].(map[string]any)
	tools, _ := execution["tools"].(map[string]any)
	if len(tools) != 1 || tools["docker"].(map[string]any)["binary"] != "/usr/local/bin/docker" {
		t.Fatalf("tools = %v, want only docker bound to its PATH binary", tools)
	}
	checks := execution["checks"].([]any)
	evidence := func(i int) any { return checks[i].(map[string]any)["evidence"] }
	if evidence(0) != "exit_code" || evidence(1) != "output" || evidence(2) != nil {
		t.Fatalf("evidence = %v %v %v; want exit_code default, explicit output kept, none for go", evidence(0), evidence(1), evidence(2))
	}
}
