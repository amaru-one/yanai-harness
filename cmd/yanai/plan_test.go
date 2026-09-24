package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repoctx"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

const markdownFixture = "# Document the package\n\n## Task\nAdd a short usage document at guide.md.\n\n## Acceptance criteria\n- guide.md describes how to call the package.\n\n## Constraints\nDo not change the API.\n"

func TestMissingCheckInputPausesBeforeModelCall(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	workspace := filepath.Join(root, "workspace")
	put(t, filepath.Join(app, "go.mod"), "module example.test/app\n\ngo 1.26.6\n")
	put(t, filepath.Join(app, "app.go"), "package app\n")
	git(t, app, "init", "-q")
	git(t, app, "add", ".")
	git(t, app, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "baseline")
	if err := cmdInit([]string{"--ws", workspace, "--repo", app}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Execution = workflow.ExecutionPolicy{MaxTokens: 100, MaxCostUSD: 1, MaxActiveSeconds: 60, MaxCalls: 1, Prices: map[string]workflow.ModelPrice{}, Checks: []workflow.Check{{ID: "unit", Args: []string{"go", "build", "./..."}, Dir: ".", TimeoutSeconds: 30, RequiredEnv: []string{"PROJECT_CHECK_INPUT"}}}}
	for _, a := range cfg.Agents {
		cfg.Execution.Prices[a.Model] = workflow.ModelPrice{Input: 1, Output: 1}
	}
	data, _ := json.Marshal(cfg)
	put(t, filepath.Join(workspace, "yanai.config.json"), string(data))
	ticket := filepath.Join(root, "ticket.md")
	put(t, ticket, markdownFixture)
	t.Setenv("OPENROUTER_API_KEY", "test")
	t.Setenv("YANAI_MOCK", "1")
	if err := cmdPlan([]string{"--ws", workspace, ticket}); err == nil || !strings.Contains(err.Error(), "PROJECT_CHECK_INPUT") {
		t.Fatalf("missing prerequisite was not reported: %v", err)
	}
	store := openWorkflowStore(t, workspace)
	defer store.Close()
	obs, err := store.Observations(1)
	if err != nil || len(obs) != 1 || obs[0].Role != "engine" || !strings.Contains(obs[0].Detail.Description, "PROJECT_CHECK_INPUT") {
		t.Fatalf("missing durable blocker: %+v %v", obs, err)
	}
	state := load(t, workspace)
	if state.Planning == nil || len(state.Planning.Turns) != 0 {
		t.Fatalf("model was called despite missing prerequisite: %+v", state.Planning)
	}
}

func TestMarkdownCycleRoutingPauseAndExecution(t *testing.T) {
	for _, scenario := range []string{"simple", "specialist_observation", "execution_observation", "crash_response", "unknown_billing", "real_checks", "check_inputs", "python_checks"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			app := filepath.Join(root, "custom-app")
			workspace := filepath.Join(root, "workspace")
			if scenario == "python_checks" {
				put(t, filepath.Join(app, "test_guide.py"), `import pathlib, unittest, os
class Guide(unittest.TestCase):
    def test_generated_content(self):
        self.assertIn("Call Value() to obtain 42.", pathlib.Path("guide.md").read_text())
        self.assertNotIn("OPENROUTER_API_KEY", os.environ)
`)
			} else {
				put(t, filepath.Join(app, "go.mod"), "module example.test/custom\n\ngo 1.26.6\n")
				put(t, filepath.Join(app, "library.go"), "package custom\nfunc Value() int { return 42 }\n")
			}
			if scenario == "real_checks" || scenario == "check_inputs" {
				testSource := `package custom
import("os";"strings";"testing")
func TestGuide(t *testing.T){data,err:=os.ReadFile("guide.md");if err!=nil{t.Fatal(err)};if !strings.Contains(string(data),"Call Value() to obtain 42."){t.Fatal("guide does not describe the existing API")};if os.Getenv("OPENROUTER_API_KEY") != "" {t.Fatal("provider credential leaked")}}
`
				if scenario == "check_inputs" {
					testSource += `func TestProjectInput(t *testing.T){if os.Getenv("PROJECT_CHECK_INPUT") != "expected-private-value" {t.Fatal("approved ticket input was not supplied")}}
`
				}
				put(t, filepath.Join(app, "guide_test.go"), testSource)
			}
			git(t, app, "init", "-q")
			git(t, app, "add", ".")
			git(t, app, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "baseline")
			if err := cmdInit([]string{"--ws", workspace, "--repo", app}); err != nil {
				t.Fatal(err)
			}
			if scenario == "python_checks" {
				t.Setenv("YANAI_GO_BINARY", "/no-go-toolchain")
			} else if scenario != "real_checks" && scenario != "check_inputs" {
				stubToolchain(t)
			} else {
				t.Setenv("YANAI_GO_BINARY", "")
				t.Setenv("YANAI_GO_CACHE", "")
				t.Setenv("YANAI_GO_MODCACHE", "")
			}
			t.Setenv("YANAI_TEST_ADMIN_URL", "")
			t.Setenv("YANAI_MOCK", "")
			t.Setenv("YANAI_NO_REPO", "")
			t.Setenv("OPENROUTER_API_KEY", "test")
			var mu sync.Mutex
			plans, executions, architects, designers := 0, 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model    string `json:"model"`
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				message := req.Messages[len(req.Messages)-1].Content
				if scenario == "check_inputs" && strings.Contains(message, "expected-private-value") {
					t.Error("check input leaked to model")
				}
				content := "NECESITO: -"
				mu.Lock()
				defer mu.Unlock()
				if strings.HasPrefix(message, "TICKET_PLAN_JSON") {
					plans++
					var input struct {
						Role  string
						Stage int
					}
					_, rest, _ := strings.Cut(message, "\nINPUT:\n")
					raw, _, _ := strings.Cut(rest, "\nPRIOR TURNS:")
					if err := json.Unmarshal([]byte(raw), &input); err != nil {
						t.Error(err)
					}
					specialists := []any{}
					observations := []workflow.ObservationDetail{}
					tasks := []workflow.Ticket{}
					if input.Role == config.RoleArchitect {
						architects++
					}
					if input.Role == config.RoleDesigner {
						designers++
					}
					if input.Role == config.RoleEngineer {
						tasks = []workflow.Ticket{{ID: "T-001", Title: "Document", Description: "Write guide.md", Outputs: []string{"guide.md"}, AllowedPaths: []string{"guide.md"}, Scope: []string{"AC-001"}, MaxAttempts: 3}}
						if scenario == "specialist_observation" && input.Stage == 0 {
							specialists = []any{map[string]string{"role": config.RoleArchitect, "reason": "Review the task's compatibility constraint"}, map[string]string{"role": config.RoleDesigner, "reason": "Review usage wording"}}
						}
					} else if input.Role == config.RoleArchitect {
						observations = []workflow.ObservationDetail{{Description: "Advisory naming concern", Requirement: "AC-001", Question: "Keep the current name?"}}
					}
					rawReply, _ := json.Marshal(map[string]any{"summary": "Bounded planning turn; skipped roles unnecessary.", "specialists": specialists, "observations": observations, "tasks": tasks})
					content = string(rawReply)
				} else if strings.HasPrefix(message, "TAREA_DE_EJECUCION") {
					executions++
					ticket, _ := workflow.DeclaredOutputs(message)
					candidate := workflow.Candidate{SchemaVersion: workflow.CandidateSchemaVersion, Ticket: ticket, Result: workflow.CandidateChange, Explanation: "Document existing behavior"}
					source := "# Usage\n\nCall Value() to obtain 42.\n"
					candidate.Files = []workflow.CandidateFile{{Path: "guide.md", Operation: workflow.CandidateSource, Source: &source}}
					if scenario == "execution_observation" && executions == 1 {
						candidate.Result = workflow.CandidateObservation
						candidate.Files = nil
						candidate.Observations = []workflow.ObservationDetail{{Description: "Clarify wording", Requirement: "AC-001", Question: "May I describe Value() as returning 42?"}}
					}
					if scenario == "execution_observation" && executions == 2 && !strings.Contains(message, "Use the existing value.") {
						t.Error("human resolution missing from execution context")
					}
					rawReply, _ := json.Marshal(candidate)
					content = string(rawReply)
				}
				usage := map[string]any{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10, "cost": 0.001}
				if scenario == "unknown_billing" && strings.HasPrefix(message, "TICKET_PLAN_JSON") {
					delete(usage, "cost")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"usage": usage, "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
			}))
			defer server.Close()
			cfg, err := config.Load(workspace)
			if err != nil {
				t.Fatal(err)
			}
			cfg.OpenRouter.BaseURL = server.URL
			cfg.Execution = workflow.ExecutionPolicy{MaxTokens: 10000000, MaxCostUSD: 100, MaxActiveSeconds: 600, MaxCalls: 30, MaxRepairs: 5, Prices: map[string]workflow.ModelPrice{}, Checks: []workflow.Check{{ID: "unit", Args: []string{"go", "test", "-race", "-shuffle=on", "-count=1", "./..."}, Dir: ".", TimeoutSeconds: 60}}}
			if scenario == "check_inputs" {
				cfg.Execution.Checks[0].RequiredEnv = []string{"PROJECT_CHECK_INPUT"}
			}
			if scenario == "python_checks" {
				binary, err := exec.LookPath("python3")
				if err != nil {
					t.Skip("real Python check needs python3")
				}
				binary, err = filepath.Abs(binary)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Execution.Tools = map[string]workflow.CheckTool{"python": {Binary: binary}}
				cfg.Execution.Checks = []workflow.Check{{ID: "unit", Args: []string{"python", "-B", "-m", "unittest", "discover", "-v"}, Dir: ".", TimeoutSeconds: 60, Evidence: "output", SuccessPattern: `(?s)Ran 1 test.*OK`, FailurePattern: "FAILED|skipped"}}
				index, err := repoctx.Index(cfg.Repo)
				if err != nil || !strings.Contains(index, "test_guide.py") {
					t.Fatalf("Python absent from context: %v %s", err, index)
				}

				if cfg.Repo.ModuleDir != "" {
					t.Fatal("non-Go init acquired a Go module requirement")
				}
			}

			for _, a := range cfg.Agents {
				cfg.Execution.Prices[a.Model] = workflow.ModelPrice{Input: 1, Output: 5}
			}
			data, _ := json.Marshal(cfg)
			put(t, filepath.Join(workspace, "yanai.config.json"), string(data))
			ticketFile := filepath.Join(root, "ticket.md")
			ticket := markdownFixture
			if scenario == "check_inputs" {
				ticket += "\n## Check inputs\n- PROJECT_CHECK_INPUT=expected-private-value\n"
			}
			put(t, ticketFile, ticket)
			args := []string{"--ws", workspace, ticketFile}
			if scenario == "unknown_billing" {
				if e := cmdPlan(args); e == nil || !strings.Contains(e.Error(), "billing") {
					t.Fatalf("missing billing did not block: %v", e)
				}
				if e := cmdPlan(args); e == nil {
					t.Fatal("cached response bypassed unresolved billing")
				}
				store := openWorkflowStore(t, workspace)
				pending, e := store.UnreconciledAttempts()
				store.Close()
				if e != nil || len(pending) != 1 {
					t.Fatalf("pending accounting: %v %v", pending, e)
				}
				if e = cmdReconcileAttempt([]string{"--ws", workspace, "--id", pending[0].ID, "--cost-usd", "0.001", "--tokens", "10", "--reference", "controlled provider fixture"}); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "specialist_observation" {
				if err = cmdPlan(args); err == nil || !strings.Contains(err.Error(), "observation") {
					t.Fatalf("expected observation pause: %v", err)
				}
				mu.Lock()
				before := plans
				if architects != 1 || designers != 0 || plans != 2 {
					t.Errorf("did not stop on first observation: %d %d %d", plans, architects, designers)
				}
				mu.Unlock()
				// Simulate death after the durable turn checkpoint but before
				// observation publication. Replay must recover the gate first.
				db, e := sql.Open("sqlite", filepath.Join(workspace, "workflow.db"))
				if e != nil {
					t.Fatal(e)
				}
				if _, e = db.Exec(`DELETE FROM workflow_observations`); e != nil {
					t.Fatal(e)
				}
				db.Close()
				if cmdPlan(args) == nil {
					t.Fatal("unresolved observation did not block resume")
				}
				revised := filepath.Join(root, "revision.md")
				put(t, revised, markdownFixture+"\n")
				if cmdPlan([]string{"--ws", workspace, revised}) == nil {
					t.Fatal("ticket revision bypassed unresolved observation")
				}
				if cmdApprove([]string{"--ws", workspace}) == nil {
					t.Fatal("approved unresolved observation")
				}
				mu.Lock()
				if plans != before {
					t.Error("replayed completed turns")
				}
				mu.Unlock()
				store := openWorkflowStore(t, workspace)
				obs, e := store.Observations(1)
				store.Close()
				if e != nil || len(obs) != 1 {
					t.Fatalf("observations: %v %v", obs, e)
				}
				if err = cmdResolve([]string{"--ws", workspace, "--observation", obs[0].ID, "--note", "Keep the existing name."}); err != nil {
					t.Fatal(err)
				}
			}
			if err = cmdPlan(args); err != nil {
				t.Fatal(err)
			}
			if scenario == "crash_response" {
				// Restore the checkpoint immediately before the first response
				// was consumed; immutable provider output remains published.
				state := load(t, workspace)
				state.Phase = ws.PhaseAnalyzed
				state.Planning.Stage = 0
				state.Planning.Complete = false
				state.Planning.Turns = nil
				state.Planning.Draft = nil
				state.Plan = nil
				state.Tasks = nil
				payload, _ := json.Marshal(state)
				db, e := sql.Open("sqlite", filepath.Join(workspace, "workflow.db"))
				if e != nil {
					t.Fatal(e)
				}
				if _, e = db.Exec(`UPDATE workflow_cycles SET phase='analyzed',payload=?,state_version=state_version+1 WHERE cycle=1`, string(payload)); e != nil {
					t.Fatal(e)
				}
				db.Close()
			}
			mu.Lock()
			before := plans
			mu.Unlock()
			if err = cmdPlan(args); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			if plans != before {
				t.Error("finished planning repeated calls")
			}
			if (scenario == "simple" || scenario == "unknown_billing" || scenario == "crash_response") && plans != 1 {
				t.Error("simple ticket called more than the engineer")
			}
			if scenario == "specialist_observation" && (plans != 4 || architects != 1 || designers != 1) {
				t.Errorf("wrong consultation sequence: %d %d %d", plans, architects, designers)
			}
			mu.Unlock()
			if cmdRun([]string{"--ws", workspace}) == nil {
				t.Fatal("unapproved ticket executed")
			}
			if err = cmdApprove([]string{"--ws", workspace}); err != nil {
				t.Fatal(err)
			}
			if scenario == "execution_observation" {
				if err = cmdRun([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "observation") {
					t.Fatalf("execution did not pause: %v", err)
				}
				if _, err = os.Stat(filepath.Join(app, "guide.md")); !os.IsNotExist(err) {
					t.Fatal("observation candidate wrote files")
				}
				store := openWorkflowStore(t, workspace)
				obs, e := store.Observations(1)
				store.Close()
				if e != nil || len(obs) != 1 {
					t.Fatalf("observations: %v %v", obs, e)
				}
				if err = cmdResolve([]string{"--ws", workspace, "--observation", obs[0].ID, "--note", "Use the existing value."}); err != nil {
					t.Fatal(err)
				}
				if cmdRun([]string{"--ws", workspace}) == nil {
					t.Fatal("resolution silently authorized execution")
				}
				if err = cmdApprove([]string{"--ws", workspace}); err != nil {
					t.Fatal(err)
				}
			}
			if err = cmdRun([]string{"--ws", workspace}); err != nil {
				t.Fatal(err)
			}
			state := load(t, workspace)
			if state.Phase != ws.PhaseAwaitingReview || state.Tasks[0].Status != workflow.TicketImplemented {
				t.Fatalf("wrong terminal state: %+v", state)
			}
			actual, err := os.ReadFile(filepath.Join(app, "guide.md"))
			if err != nil || !strings.Contains(string(actual), "Call Value()") {
				t.Fatal("missing intended real diff", err)
			}
			store := openWorkflowStore(t, workspace)
			rounds, err := store.Rounds(1)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, round := range rounds {
				if round.Manifest != nil && len(round.Checks) > 0 {
					found = true
				}
			}
			if !found {
				t.Fatal("missing machine-recorded validation")
			}
			store.Close()
			// Removing the application must not prevent local history inspection.
			if err = os.Rename(app, app+"-gone"); err != nil {
				t.Fatal(err)
			}
			if err = cmdStatus([]string{"--ws", workspace, "--json"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
