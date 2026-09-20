package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

type decisionProvider struct {
	mu        sync.Mutex
	requests  []string
	decisions int
}

func decisionSetup(t *testing.T, raw string, reply func(workflow.DecisionContext, int) string) (string, string, *decisionProvider) {
	t.Helper()
	harness, _, workspace := siblings(t)
	t.Chdir(harness)
	t.Setenv("YANAI_NO_REPO", "")
	t.Setenv("YANAI_MOCK", "")
	t.Setenv("OPENROUTER_API_KEY", "local-test")
	if err := cmdInit([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(workspace, "interviews", "private-source.md")
	put(t, input, raw)
	provider := &decisionProvider{}
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
		provider.mu.Lock()
		defer provider.mu.Unlock()
		for _, m := range req.Messages {
			provider.requests = append(provider.requests, m.Content)
		}
		message := req.Messages[len(req.Messages)-1].Content
		content := "Specialist review: keep the backend scope."
		if strings.HasPrefix(message, "SELECCIONA_ARCHIVOS") {
			content = "NECESITO: -"
		}
		if strings.HasPrefix(message, "DECISION_JSON") {
			_, rest, _ := strings.Cut(message, "DECISION_CONTEXT_JSON\n")
			var c workflow.DecisionContext
			if err := json.NewDecoder(strings.NewReader(rest)).Decode(&c); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			provider.decisions++
			content = reply(c, provider.decisions)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}}})
	}))
	t.Cleanup(server.Close)
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
	return workspace, input, provider
}

func proposalFor(c workflow.DecisionContext, outcome workflow.Outcome) workflow.Proposal {
	p := workflow.Proposal{SchemaVersion: "1", ID: "P-1", Origin: c.Source.Origin, Outcome: outcome, Summary: "Test decision", Scope: []string{c.Scope.Requirements[0].ID}, Inputs: c.Inputs}
	for i, e := range c.Source.Excerpts {
		id := "C-" + e.ID
		p.Citations = append(p.Citations, workflow.Citation{ID: id, SourceID: c.Source.ID, Revision: c.Source.Revision, ExcerptID: e.ID, Quote: e.Text})
		if i == 0 {
			p.Evidence = []string{id}
		}
	}
	if p.Origin == workflow.TechnicalEnabler {
		p.Evidence = nil
		p.Citations = nil
		p.Rationale = "A reproducible baseline defect needs characterization."
	}
	if outcome == workflow.OutcomeNeedsEvidence {
		p.Questions = []string{"Which account of the need is supported?"}
	}
	if outcome == workflow.OutcomeProposeChange {
		p.Tickets = []workflow.Ticket{{SchemaVersion: "1", ID: "T-001", Type: p.Origin, Owner: "ingeniero", Title: "Characterize behavior", Description: "Add a focused check.", Rationale: p.Rationale, Status: "pending", Evidence: p.Evidence, Scope: p.Scope, Inputs: c.Inputs, Outputs: []string{"yanai-server/example_test.go"}, AllowedPaths: []string{"yanai-server"}, Criteria: []string{"The regression check fails on the observed defect."}, BaseCommit: c.BaseCommit, MaxAttempts: 2, Revision: 1}}
	}
	return p
}

func encode(p workflow.Proposal) string {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func load(t *testing.T, workspace string) *ws.State {
	t.Helper()
	w, err := ws.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	s, err := w.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func analyzeArgs(workspace, input string) []string {
	return []string{"--ws", workspace, "--privacy-reviewed", input}
}

func TestDistinctDecisionStatesThroughCLI(t *testing.T) {
	cases := map[workflow.Outcome]string{workflow.OutcomeNoChange: ws.PhaseNoChange, workflow.OutcomeNeedsEvidence: ws.PhaseNeedsEvidence, workflow.OutcomeOutOfScope: ws.PhaseOutOfScope, workflow.OutcomeBlockedBaseline: ws.PhaseBlockedBaseline}
	for outcome, want := range cases {
		t.Run(string(outcome), func(t *testing.T) {
			workspace, input, _ := decisionSetup(t, "Synthetic interview statement.", func(c workflow.DecisionContext, _ int) string { return encode(proposalFor(c, outcome)) })
			if err := cmdAnalyze(analyzeArgs(workspace, input)); err != nil {
				t.Fatal(err)
			}
			st := load(t, workspace)
			if st.Phase != want || st.Verdict != string(outcome) || len(st.Tasks) != 0 {
				t.Fatalf("wrong terminal state: %+v", st)
			}
			if st.Proposal == nil {
				t.Fatal("typed proposal not persisted")
			}
			if err := cmdApprove([]string{"--ws", workspace}); err == nil {
				t.Fatal("terminal decision could be approved")
			}
			if err := cmdDiscuss([]string{"--ws", workspace}); err == nil {
				t.Fatal("terminal decision opened discussion")
			}
			if suggestion(st) == suggestion(&ws.State{Phase: ws.PhaseSufficient}) {
				t.Fatal("terminal decision reused legacy generic next action")
			}
		})
	}
}

func TestAbsentEvidenceNeedsResearchWithoutProviderCall(t *testing.T) {
	workspace, input, provider := decisionSetup(t, "", func(workflow.DecisionContext, int) string { t.Error("unexpected provider call"); return "{}" })
	if err := cmdAnalyze(analyzeArgs(workspace, input)); err != nil {
		t.Fatal(err)
	}
	st := load(t, workspace)
	if st.Phase != ws.PhaseNeedsEvidence || len(st.Proposal.Questions) == 0 {
		t.Fatal("empty evidence became sufficiency")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 0 {
		t.Fatal("empty intake called provider")
	}
}

func TestPrivacyAndProvenanceThroughAnalyzeAndDiscuss(t *testing.T) {
	raw := "Ana dijo: pierdo tiempo copiando registros.\nContacto ana@example.invalid, DNI 12345678, teléfono 987654321."
	workspace, input, provider := decisionSetup(t, raw, func(c workflow.DecisionContext, _ int) string {
		return encode(proposalFor(c, workflow.OutcomeProposeChange))
	})
	if err := cmdAnalyze([]string{"--ws", workspace, input}); err == nil {
		t.Fatal("unreviewed interview was accepted")
	}
	if err := cmdAnalyze([]string{"--ws", workspace, "--privacy-reviewed", "--source-id", "interview-01", "--date", "2026-09-20", "--redact", "Ana", input}); err != nil {
		t.Fatal(err)
	}
	if err := cmdDiscuss([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	if err := cmdApprove([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	st := load(t, workspace)
	if st.Intake.ID != "interview-01" || st.Intake.Date != "2026-09-20" || st.Intake.OriginalHash != workflow.Digest(raw) || st.Intake.Revision == st.Intake.OriginalHash {
		t.Fatal("lost provenance")
	}
	if st.Intake.Excerpts[1].Line != 2 || st.Plan == nil || len(st.Plan.Tickets) != 1 {
		t.Fatal("missing excerpt or typed plan")
	}
	for _, path := range []string{"00-entrada.md", "state.json"} {
		info, err := os.Stat(filepath.Join(workspace, "cycles/001", path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("private input permissions: %s %v", path, info.Mode())
		}
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	for _, message := range provider.requests {
		for _, private := range []string{"Ana dijo", "ana@example.invalid", "12345678", "987654321", "private-source.md", raw} {
			if strings.Contains(message, private) {
				t.Fatalf("private source leaked: %s", private)
			}
		}
	}
}

func TestContradictoryEvidenceCannotBecomeSufficiency(t *testing.T) {
	workspace, input, provider := decisionSetup(t, "The current export solves my task.\nThe current export cannot solve my task.", func(c workflow.DecisionContext, attempt int) string {
		outcome := workflow.OutcomeNoChange
		if attempt == 2 {
			outcome = workflow.OutcomeNeedsEvidence
		}
		p := proposalFor(c, outcome)
		p.Findings = []workflow.Finding{{Kind: "conflict", Text: "Accounts conflict.", Evidence: []string{p.Citations[0].ID, p.Citations[1].ID}}}
		return encode(p)
	})
	if err := cmdAnalyze(analyzeArgs(workspace, input)); err != nil {
		t.Fatal(err)
	}
	if load(t, workspace).Phase != ws.PhaseNeedsEvidence {
		t.Fatal("conflict was treated as sufficiency")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.decisions != 2 {
		t.Fatalf("correction count: %d", provider.decisions)
	}
}

func TestInvalidDecisionsNeverPersistAnExecutablePlan(t *testing.T) {
	cases := map[string]func(workflow.Proposal) string{
		"invented quote":      func(p workflow.Proposal) string { p.Citations[0].Quote = "a teacher never said this"; return encode(p) },
		"unknown scope":       func(p workflow.Proposal) string { p.Scope = []string{"invented"}; return encode(p) },
		"unknown owner":       func(p workflow.Proposal) string { p.Tickets[0].Owner = "ghost"; return encode(p) },
		"unsupported version": func(p workflow.Proposal) string { p.SchemaVersion = "99"; return encode(p) },
		"blank criterion":     func(p workflow.Proposal) string { p.Tickets[0].Criteria = []string{" "}; return encode(p) },
		"missing inputs":      func(p workflow.Proposal) string { p.Tickets[0].Inputs = nil; return encode(p) },
		"missing outputs":     func(p workflow.Proposal) string { p.Tickets[0].Outputs = nil; return encode(p) },
		"duplicate IDs":       func(p workflow.Proposal) string { p.Tickets = append(p.Tickets, p.Tickets[0]); return encode(p) },
		"cycle":               func(p workflow.Proposal) string { p.Tickets[0].DependsOn = []string{"T-001"}; return encode(p) },
		"missing dependency":  func(p workflow.Proposal) string { p.Tickets[0].DependsOn = []string{"missing"}; return encode(p) },
		"UI output": func(p workflow.Proposal) string {
			p.Tickets[0].Outputs = []string{"yanai-ui/a.svelte"}
			return encode(p)
		},
		"scope escalation": func(p workflow.Proposal) string { p.Origin = workflow.TechnicalEnabler; return encode(p) },
		"malformed":        func(workflow.Proposal) string { return "{invalid JSON" },
		"legacy verdict":   func(workflow.Proposal) string { return "VEREDICTO: PROPOSE_CHANGE\n### TAREA: T-001" },
		"duplicate JSON key": func(p workflow.Proposal) string {
			return strings.Replace(encode(p), `"outcome":`, `"outcome":"NO_CHANGE_NEEDED","outcome":`, 1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			workspace, input, provider := decisionSetup(t, "A synthetic teacher need.", func(c workflow.DecisionContext, _ int) string {
				return mutate(proposalFor(c, workflow.OutcomeProposeChange))
			})
			if err := cmdAnalyze(analyzeArgs(workspace, input)); err == nil || !strings.Contains(err.Error(), "one correction") {
				t.Fatalf("invalid decision: %v", err)
			}
			st := load(t, workspace)
			if st.Proposal != nil || st.Plan != nil || len(st.Tasks) != 0 || st.Approval != nil {
				t.Fatal("invalid response advanced state")
			}
			provider.mu.Lock()
			defer provider.mu.Unlock()
			if provider.decisions != 2 {
				t.Fatalf("unbounded retries: %d", provider.decisions)
			}
		})
	}
}

func TestTechnicalEnablerUsesEngineeringProvenance(t *testing.T) {
	workspace, input, _ := decisionSetup(t, "A query is missing a characterization check.", func(c workflow.DecisionContext, _ int) string {
		return encode(proposalFor(c, workflow.OutcomeProposeChange))
	})
	if err := cmdAnalyze([]string{"--ws", workspace, "--privacy-reviewed", "--technical-enabler", input}); err != nil {
		t.Fatal(err)
	}
	if err := cmdDiscuss([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	if err := cmdApprove([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	st := load(t, workspace)
	if st.Plan.Origin != workflow.TechnicalEnabler || len(st.Plan.Citations) != 0 || st.Plan.Tickets[0].Rationale == "" {
		t.Fatal("engineering work fabricated teacher demand")
	}
}

func TestDiscussRejectsMalformedPlanAndChangedSource(t *testing.T) {
	workspace, input, _ := decisionSetup(t, "A synthetic teacher need.", func(c workflow.DecisionContext, attempt int) string {
		if attempt > 1 {
			return "VEREDICTO: SUFICIENTE"
		}
		return encode(proposalFor(c, workflow.OutcomeProposeChange))
	})
	if err := cmdAnalyze(analyzeArgs(workspace, input)); err != nil {
		t.Fatal(err)
	}
	if err := cmdDiscuss([]string{"--ws", workspace}); err == nil {
		t.Fatal("discussion accepted regex plan")
	}
	st := load(t, workspace)
	if st.Phase != ws.PhaseAnalyzed || st.Plan != nil {
		t.Fatal("failed discussion advanced persisted state")
	}
	put(t, filepath.Join(workspace, "cycles/001/00-entrada.md"), "changed source")
	if err := cmdDiscuss([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "source snapshot changed") {
		t.Fatalf("changed source: %v", err)
	}
}

func TestLegacyCyclesAreInspectibleButNeedExplicitReintake(t *testing.T) {
	harness, _, workspace := siblings(t)
	t.Chdir(harness)
	t.Setenv("YANAI_MOCK", "1")
	if err := cmdInit([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	st, err := w.NewCycle()
	if err != nil {
		t.Fatal(err)
	}
	st.Phase = ws.PhaseWaiting
	st.Verdict = "NUEVO_PLAN"
	if err := w.SaveState(st); err != nil {
		t.Fatal(err)
	}
	if err := cmdStatus([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	if err := cmdApprove([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy approval: %v", err)
	}
	put(t, filepath.Join(workspace, "cycles/001/00-entrada.md"), "Reviewed imported need.")
	if err := cmdAnalyze(analyzeArgs(workspace, filepath.Join(workspace, "cycles/001/00-entrada.md"))); err != nil {
		t.Fatal(err)
	}
	if got := load(t, workspace); got.Cycle != 2 || got.SchemaVersion != "1" {
		t.Fatal("legacy input was not explicitly imported into a new cycle")
	}
}
