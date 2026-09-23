package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/yanai/yanai-harness/internal/repository"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/yanai/yanai-harness/internal/workflow"
)

func configureExecution(t *testing.T, workspace string) {
	t.Helper()
	name := filepath.Join(workspace, "yanai.config.json")
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["discussion_order"] = []string{"arquitecto-bd", "disenador", "ingeniero"}
	// Only the legacy interview regression fixtures enable the retired role.
	doc["agents"].(map[string]any)["product-owner"] = map[string]any{"model": "legacy/mock-po", "prompt": "prompts/product-owner.md", "name": "Historical PO", "max_tokens": 12000}
	prices := map[string]workflow.ModelPrice{}
	for _, raw := range doc["agents"].(map[string]any) {
		a := raw.(map[string]any)
		prices[a["model"].(string)] = workflow.ModelPrice{Input: 1, Output: 5}
	}
	doc["execution"] = workflow.ExecutionPolicy{MaxTokens: 10000000, MaxCostUSD: 100, MaxActiveSeconds: 3600, MaxCalls: 100, MaxRepairs: 10, Prices: prices, Checks: []workflow.Check{{ID: "backend-test", Args: []string{"go", "test", "./internal/example"}, Dir: "yanai-server", TimeoutSeconds: 5}}}
	data, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	put(t, name, string(data))
}

func TestApprovalTamperingBlocksBeforeAnyModelCall(t *testing.T) {
	cases := map[string]func(*testing.T, string, string){
		"source": func(t *testing.T, app, ws string) {
			put(t, filepath.Join(ws, "cycles/001/00-entrada.md"), "changed source")
		},
		"prompt": func(t *testing.T, app, ws string) {
			put(t, filepath.Join(ws, "prompts/ingeniero.md"), "changed instructions")
		},
		"discussion": func(t *testing.T, app, ws string) {
			put(t, filepath.Join(ws, "cycles/001/03-discusion.md"), "changed discussion")
		},
		"source without commit": func(t *testing.T, app, ws string) {
			put(t, filepath.Join(app, "yanai-server/go.mod"), "module yanai-server\n// external change\n")
		},
		"canonical ticket": func(t *testing.T, app, ws string) {
			db, err := sql.Open("sqlite", filepath.Join(ws, "workflow.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec(`UPDATE workflow_tickets SET payload=replace(payload,'Tarea simulada','Changed task') WHERE id='T-001'`); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			app, ws := approvedCycle(t)
			s := openWorkflowStore(t, ws)
			before, err := s.Budget(1)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			tamper(t, app, ws)
			if err := cmdRun([]string{"--ws", ws}); err == nil {
				t.Fatal("tampering authorized execution")
			}
			s = openWorkflowStore(t, ws)
			defer s.Close()
			after, _ := s.Budget(1)
			if before.Calls != after.Calls {
				t.Fatalf("paid call before rejection: %d -> %d", before.Calls, after.Calls)
			}
		})
	}
}
func TestCandidateTamperingBlocksDependentCalls(t *testing.T) {
	_, ws := approvedCycle(t)
	if err := cmdRun([]string{"--ws", ws, "--task", "T-001"}); err != nil {
		t.Fatal(err)
	}
	s := openWorkflowStore(t, ws)
	ref, err := s.CandidateRef(1, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.GetArtifact(1, ref)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Budget(1)
	s.Close()
	put(t, filepath.Join(ws, record.Path), "forged candidate")
	if err := cmdRun([]string{"--ws", ws, "--task", "T-002"}); err == nil {
		t.Fatal("forged candidate accepted")
	}
	s = openWorkflowStore(t, ws)
	defer s.Close()
	after, _ := s.Budget(1)
	if before.Calls != after.Calls {
		t.Fatal("tampering detected after a paid call")
	}
}
func TestCycleCallLimitBlocksCLIWithoutReset(t *testing.T) {
	_, ws := approvedCycle(t)
	s := openWorkflowStore(t, ws)
	b, _ := s.Budget(1)
	s.Close()
	name := filepath.Join(ws, "yanai.config.json")
	raw, _ := os.ReadFile(name)
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	doc["execution"].(map[string]any)["max_calls"] = b.Calls
	raw, _ = json.Marshal(doc)
	put(t, name, string(raw))
	if err := cmdRun([]string{"--ws", ws}); err == nil {
		t.Fatal("policy change silently adopted")
	}
	if err := cmdPolicy([]string{"--ws", ws, "--note", "adopt reduced allowance"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdApprove([]string{"--ws", ws}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := cmdRun([]string{"--ws", ws}); err == nil {
			t.Fatal("exhausted calls authorized run")
		}
	}
	s = openWorkflowStore(t, ws)
	defer s.Close()
	after, _ := s.Budget(1)
	if after.Calls != b.Calls {
		t.Fatal("limit dispatched or reset calls")
	}
}
func TestEnginePatchStateAllowsOwnChangesButRejectsExternalEdits(t *testing.T) {
	app, ws := approvedCycle(t)
	r, cleanup, err := bindWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Workspace.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	target, err := repository.Open(r.Cfg.Repo, ws)
	if err != nil {
		t.Fatal(err)
	}
	before, revision, err := r.Workspace.Store.PatchState(1, st.Approval.ContractHash)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the Step 8 tool's exact expected snapshot without mutating real Yanai.
	file := filepath.Join(app, "yanai-server/ejemplo-arquitecto-bd.md")
	put(t, file, "engine patch")
	after, err := target.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(file)
	if err = r.Workspace.Store.PreparePatch(1, st.Approval.ContractHash, "T-001", after, revision); err != nil {
		t.Fatal(err)
	}
	put(t, file, "engine patch")
	actual, err := target.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if actual.Baseline() == before.Baseline() {
		t.Fatal("content change not fingerprinted")
	}
	if err = r.Workspace.Store.ReconcilePatch(1, st.Approval.ContractHash, actual); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if err := cmdRun([]string{"--ws", ws, "--task", "T-001"}); err != nil {
		t.Fatalf("own patch invalidated approval: %v", err)
	}
	put(t, file, "external second change")
	if err := cmdRun([]string{"--ws", ws, "--task", "T-002"}); err == nil {
		t.Fatal("external dirty-to-dirty change allowed")
	}
}
func TestReviewAndReconciliationNeedNoProviderKey(t *testing.T) {
	_, ws := approvedCycle(t)
	t.Setenv("YANAI_MOCK", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	if err := cmdReview([]string{"--ws", ws}); err != nil {
		t.Fatal(err)
	}
	if err := cmdPolicy([]string{"--ws", ws}); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownBillingBlocksAnalysisRestartWithoutOpeningFreshBudget(t *testing.T) {
	ws, input, _ := decisionSetup(t, "Teachers repeat the same description.", func(c workflow.DecisionContext, _ int) string {
		raw, _ := json.Marshal(proposalFor(c, workflow.OutcomeProposeChange))
		return string(raw)
	})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"choices":[{"finish_reason":"stop","message":{"content":"NECESITO: -"}}]}`)
	}))
	defer server.Close()
	name := filepath.Join(ws, "yanai.config.json")
	raw, _ := os.ReadFile(name)
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	doc["openrouter"].(map[string]any)["base_url"] = server.URL
	raw, _ = json.Marshal(doc)
	put(t, name, string(raw))
	for i := 0; i < 2; i++ {
		if err := cmdAnalyze([]string{"--ws", ws, "--privacy-reviewed", input}); err == nil {
			t.Fatal("analysis proceeded with unknown billing")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("unknown billing dispatched %d calls", calls.Load())
	}
	s := openWorkflowStore(t, ws)
	defer s.Close()
	n, err := s.MaxCycle()
	if err != nil || n != 1 {
		t.Fatalf("restart opened another budget: cycle %d %v", n, err)
	}
	b, _ := s.Budget(1)
	if b.Calls != 1 || b.Unknown != 1 {
		t.Fatalf("lost usage: %+v", b)
	}
}
func TestAllPlanningCallsAreAccountedAndApprovalHashCanBePinned(t *testing.T) {
	_, ws := approvedCycle(t)
	s := openWorkflowStore(t, ws)
	b, err := s.Budget(1)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Analysis: file selection + decision. Discussion: three selections, three
	// specialist turns, and the consolidated decision.
	if b.Calls != 9 {
		t.Fatalf("planning calls = %d, want 9", b.Calls)
	}
	if err := cmdApprove([]string{"--ws", ws, "--contract", "different-reviewed-hash"}); err == nil {
		t.Fatal("different review hash approved")
	}
}
