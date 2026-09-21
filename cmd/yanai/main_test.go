package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/ws"
)

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func siblings(t *testing.T) (harness, app, workspace string) {
	t.Helper()
	parent := t.TempDir()
	harness, app = filepath.Join(parent, "yanai-harness"), filepath.Join(parent, "yanai")
	workspace = filepath.Join(harness, "yanai-workspace")
	put(t, filepath.Join(harness, "go.mod"), "module github.com/yanai/yanai-harness\n")
	put(t, filepath.Join(app, "yanai-server/go.mod"), "module yanai-server\n\ngo 1.24\n")
	put(t, filepath.Join(app, "yanai-server/SPEC.md"), "# Backend\n")
	git(t, app, "init", "-q")
	git(t, app, "add", ".")
	git(t, app, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "baseline")
	return
}

func TestInitSiblingAndExistingConfigurationAcrossWorkingDirectories(t *testing.T) {
	harness, app, workspace := siblings(t)
	t.Chdir(harness)
	if err := cmdInit([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	configureExecution(t, workspace)
	canonical, err := repository.Canonical(app)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(workspace)
	if err != nil || cfg.Repo.Path != canonical {
		t.Fatalf("sibling target: %v %+v", err, cfg)
	}

	// A compact JSON config with arbitrary custom fields must survive --repo.
	path := filepath.Join(workspace, "yanai.config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["custom"] = map[string]any{"preserve": true}
	document["repo"].(map[string]any)["path"] = "../../yanai"
	document["agents"].(map[string]any)["ingeniero"].(map[string]any)["model"] = "my/custom-model"
	data, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	put(t, path, string(data))
	t.Chdir(t.TempDir())
	cfg, err = config.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repository.Open(cfg.Repo, workspace)
	if err != nil || target.Root != canonical {
		t.Fatalf("relative config depends on cwd: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	aliasConfig, err := config.Load(alias)
	if err != nil {
		t.Fatal(err)
	}
	aliasTarget, err := repository.Open(aliasConfig.Repo, alias)
	if err != nil || aliasTarget.Root != canonical {
		t.Fatalf("workspace alias changed relative binding: %v", err)
	}
	if err := cmdContext([]string{"--ws", workspace, "--files", "yanai-server/go.mod"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdInit([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	configureExecution(t, workspace)
	t.Chdir(harness)
	if err := cmdInit([]string{"--ws", workspace, "--repo", "../yanai"}); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(workspace)
	if err != nil || cfg.Repo.Path != canonical || cfg.Agents["ingeniero"].Model != "my/custom-model" {
		t.Fatalf("config lost customization: %v %+v", err, cfg)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"preserve": true`)) {
		t.Fatal("unknown fields lost")
	}
}

func TestInitRepairsLegacyBindingOnlyWhenExplicit(t *testing.T) {
	harness, _, workspace := siblings(t)
	t.Chdir(harness)
	if err := cmdInit([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	configureExecution(t, workspace)
	if err := config.SetRepoPath(workspace, "../app-docente"); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(workspace, "yanai.config.json")
	before, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdInit([]string{"--ws", workspace}); err == nil {
		t.Fatal("guessed a replacement for invalid legacy path")
	}
	after, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed init changed configuration")
	}
	if err := cmdInit([]string{"--ws", workspace, "--repo", "../yanai"}); err != nil {
		t.Fatal(err)
	}
}

func TestInitRejectsOverlapAndWrongRepoBeforeWriting(t *testing.T) {
	harness, app, _ := siblings(t)
	t.Chdir(harness)
	inside := filepath.Join(app, "workspace")
	if err := cmdInit([]string{"--ws", inside}); err == nil {
		t.Fatal("accepted workspace in application")
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatal("invalid init created workspace")
	}
	workspace := filepath.Join(t.TempDir(), "new")
	if err := cmdInit([]string{"--ws", workspace, "--repo", harness}); err == nil {
		t.Fatal("accepted harness target")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("invalid target created workspace")
	}
}

func approvedCycle(t *testing.T) (app, workspace string) {
	t.Helper()
	harness, app, workspace := siblings(t)
	t.Chdir(harness)
	t.Setenv("YANAI_MOCK", "1")
	t.Setenv("YANAI_NO_REPO", "")
	if err := cmdInit([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	configureExecution(t, workspace)
	interview := filepath.Join(workspace, "interviews/test.md")
	put(t, interview, "A teacher wants less repeated writing.")
	if err := cmdAnalyze([]string{"--ws", workspace, "--privacy-reviewed", interview}); err != nil {
		t.Fatal(err)
	}
	if err := cmdDiscuss([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	if err := cmdApprove([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	return app, workspace
}

func TestMockCLIUsesBindingAndGuardsRun(t *testing.T) {
	app, workspace := approvedCycle(t)
	statePath := filepath.Join(workspace, "cycles/001/state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(app, "user-work.txt"), "preserve me")
	if err := cmdRun([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "clean checkout") {
		t.Fatalf("dirty run: %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected run mutated state")
	}
	if err := os.Remove(filepath.Join(app, "user-work.txt")); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repository.Open(cfg.Repo, workspace)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := target.LockWriter()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdRun([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "active writer") {
		t.Errorf("concurrent run: %v", err)
	}
	unlock()
	t.Setenv("YANAI_NO_REPO", "1")
	if err := cmdRun([]string{"--ws", workspace}); err == nil {
		t.Fatal("no-repo bypassed run preflight")
	}
	t.Setenv("YANAI_NO_REPO", "")
	if err := cmdRun([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	st := load(t, workspace)
	// Step 3 still stages candidates. Step 8 will apply verified patches.
	if _, err := os.Stat(filepath.Join(workspace, "cycles/001", st.Tasks[2].Deliverable, "archivos/yanai-server/ejemplo.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app, "yanai-server/ejemplo.md")); !os.IsNotExist(err) {
		t.Fatal("Step 3 unexpectedly applied a candidate")
	}
}

func TestRunRejectsDifferentCheckoutWithSameHEAD(t *testing.T) {
	app, workspace := approvedCycle(t)
	clone := filepath.Join(t.TempDir(), "clone")
	git(t, app, "clone", "-q", app, clone)
	if err := config.SetRepoPath(workspace, clone); err != nil {
		t.Fatal(err)
	}
	if err := cmdRun([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("approval reused for another checkout: %v", err)
	}
}

func TestRunRejectsForbiddenModelOutputBeforeStaging(t *testing.T) {
	t.Setenv("YANAI_MOCK", "")
	t.Setenv("OPENROUTER_API_KEY", "local-test")
	for _, path := range []string{"yanai-ui/view.svelte", "../yanai-server/leak.go", "yanai-server/.env", "yanai-server/../../outside"} {
		t.Run(path, func(t *testing.T) {
			_, workspace := approvedCycle(t)
			t.Setenv("YANAI_MOCK", "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				var req struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				content := "NECESITO: -"
				if strings.Contains(req.Messages[len(req.Messages)-1].Content, "TAREA_DE_EJECUCION") {
					content = "=== ARCHIVO: " + path + " ===\nforbidden\n=== FIN ARCHIVO ===\n"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10, "cost": 0.001}, "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}}})
			}))
			defer server.Close()
			name := filepath.Join(workspace, "yanai.config.json")
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			doc["openrouter"].(map[string]any)["base_url"] = server.URL
			data, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			put(t, name, string(data))
			if err := cmdPolicy([]string{"--ws", workspace, "--note", "review changed provider"}); err != nil {
				t.Fatal(err)
			}
			if err := cmdApprove([]string{"--ws", workspace}); err != nil {
				t.Fatal(err)
			}
			if err := cmdRun([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "output:") {
				t.Fatalf("forbidden output: %v", err)
			}
			w, err := ws.Open(workspace)
			if err != nil {
				t.Fatal(err)
			}
			st, err := w.LoadState()
			if err != nil {
				t.Fatal(err)
			}
			if st.Tasks[0].Status == "done" {
				t.Fatal("forbidden output marked done")
			}
			if _, err := os.Stat(filepath.Join(workspace, "cycles/001/entregables")); !os.IsNotExist(err) {
				t.Fatal("forbidden output was staged")
			}
		})
	}
}
