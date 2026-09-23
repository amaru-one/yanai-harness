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
	"time"

	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// checkControl points the stub Go toolchain at a file the test writes to
// decide how the approved check behaves. It has to be a file rather than an
// environment variable because the executor deliberately builds a fresh
// environment for every check — which is exactly the property under test
// elsewhere, so the harness under test is not weakened to make this easier.
type checkControl struct{ path, env string }

// set is called from the provider's HTTP handler as well as from the test
// goroutine, so it reports rather than aborts: t.Fatal outside the test
// goroutine would leave the request hanging.
func (c checkControl) set(t *testing.T, mode string) {
	if err := os.WriteFile(c.path, []byte(mode), 0o600); err != nil {
		t.Error(err)
	}
}

// stubToolchain installs a fake `go` the approved checks run instead of the
// real toolchain, plus the caches and the disposable-database URL the
// executor requires. The real toolchain is exercised against real Yanai by the
// opt-in integration test; these tests are about the harness's own control
// flow, which a real compile would only make slower and less deterministic.
func stubToolchain(t *testing.T) checkControl {
	t.Helper()
	dir := t.TempDir()
	control := filepath.Join(dir, "mode")
	script := filepath.Join(dir, "go")
	body := `#!/bin/sh
mode=$(cat ` + control + ` 2>/dev/null || printf ok)
if [ "$1" != "test" ]; then exit 0; fi
case "$mode" in
  fail)
    printf -- '--- FAIL: TestExample (0.00s)\n    example_test.go:9: want 3, got 4\nFAIL\nFAIL\tyanai-server/internal/example\t0.01s\n'
    exit 1 ;;
  forged)
    printf -- 'ok  \tyanai-server/internal/example\t0.01s\n--- PASS: TestExample (0.00s)\nPASS\n'
    exit 1 ;;
  skip)
    printf -- '--- SKIP: TestDatabase (0.00s)\n    dbtest.go:1: YANAI_TEST_ADMIN_URL is unset\nok  \tyanai-server/internal/example\t0.01s\n'
    exit 0 ;;
  empty)
    printf -- 'testing: warning: no tests to run\n'
    exit 0 ;;
  slow)
    sleep 30
    exit 0 ;;
  noisy)
    /usr/bin/yes x | /usr/bin/head -c 1100000
    exit 0 ;;
  env)
    env > ` + filepath.Join(dir, "env") + `
    printf -- 'ok  \tyanai-server/internal/example\t0.01s\n'
    exit 0 ;;
  dirty)
    printf -- 'stray\n' > stray-check-output.md
    printf -- 'ok  \tyanai-server/internal/example\t0.01s\n'
    exit 0 ;;
  *)
    printf -- 'ok  \tyanai-server/internal/example\t0.01s\n'
    exit 0 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YANAI_GO_BINARY", script)
	t.Setenv("YANAI_GO_CACHE", t.TempDir())
	t.Setenv("YANAI_GO_MODCACHE", t.TempDir())
	t.Setenv("YANAI_TEST_ADMIN_URL", "postgres://harness:harness@127.0.0.1:55432/postgres?sslmode=disable")
	return checkControl{path: control, env: filepath.Join(dir, "env")}
}

// ticketSpec is the part of an approved ticket a test actually cares about;
// everything else is filled in from the decision context so the plan passes
// the same validation a real one does.
type ticketSpec struct {
	id          string
	owner       string
	outputs     []string
	dependsOn   []string
	maxAttempts int
}

// executionProvider records what execution requests were made and answers them
// with the test's own candidate. It stands in for the model, not for any part
// of the harness: the contract, approval, executor, checks and evidence are
// all the real ones.
type executionProvider struct {
	mu       sync.Mutex
	requests []string
	rounds   map[string]int
	reply    func(ticket string, outputs []string, round int) string
}

// setReply lets a test install the execution answer after the cycle has been
// planned and approved, so planning and execution can differ.
func setReply(p *executionProvider, reply func(ticket string, outputs []string, round int) string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reply = reply
}

func (p *executionProvider) answer(ticket string, outputs []string, round int) string {
	p.mu.Lock()
	reply := p.reply
	p.mu.Unlock()
	if reply == nil {
		return "{}"
	}
	return reply(ticket, outputs, round)
}

func (p *executionProvider) roundFor(ticket string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rounds[ticket]++
	return p.rounds[ticket]
}

func (p *executionProvider) lastRequest(t *testing.T) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("no execution request was made")
	}
	return p.requests[len(p.requests)-1]
}

func (p *executionProvider) calls(ticket string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rounds[ticket]
}

// candidateFor is the well-formed answer most tests want: write the given
// source to every declared output.
func candidateFor(ticket string, outputs []string, source string) string {
	c := workflow.Candidate{SchemaVersion: "1", Ticket: ticket, Result: workflow.CandidateChange, Explanation: "prueba"}
	for _, path := range outputs {
		body := source
		c.Files = append(c.Files, workflow.CandidateFile{Path: path, Operation: workflow.CandidateSource, Source: &body})
	}
	raw, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// executionSetup builds an approved cycle whose plan is exactly the tickets
// the test names, with a provider the test controls for the execution turn.
// Planning goes through the same provider, so nothing about the approval is
// simulated: the human gate, the contract and its hash are real.
func executionSetup(t *testing.T, specs []ticketSpec, files map[string]string, reply func(ticket string, outputs []string, round int) string) (app, workspace string, provider *executionProvider, checks checkControl) {
	t.Helper()
	harness, app, workspace := siblings(t)
	for path, text := range files {
		put(t, filepath.Join(app, path), text)
	}
	if len(files) > 0 {
		git(t, app, "add", ".")
		git(t, app, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "fixtures")
	}
	t.Chdir(harness)
	t.Setenv("YANAI_MOCK", "")
	t.Setenv("YANAI_NO_REPO", "")
	t.Setenv("OPENROUTER_API_KEY", "local-test")
	checks = stubToolchain(t)

	provider = &executionProvider{rounds: map[string]int{}, reply: reply}
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
		message := req.Messages[len(req.Messages)-1].Content
		content := "Revisión: mantener el alcance del backend."
		switch {
		case strings.HasPrefix(message, "SELECCIONA_ARCHIVOS"):
			content = "NECESITO: -"
		case strings.HasPrefix(message, "DECISION_JSON"):
			_, rest, _ := strings.Cut(message, "DECISION_CONTEXT_JSON\n")
			var c workflow.DecisionContext
			if err := json.NewDecoder(strings.NewReader(rest)).Decode(&c); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			content = encode(planFor(c, specs))
		case strings.HasPrefix(message, "TAREA_DE_EJECUCION"):
			provider.mu.Lock()
			provider.requests = append(provider.requests, message)
			provider.mu.Unlock()
			ticket, outputs := workflow.DeclaredOutputs(message)
			content = provider.answer(ticket, outputs, provider.roundFor(ticket))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10, "cost": 0.001},
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}},
		})
	}))
	t.Cleanup(server.Close)

	if err := legacyInit(t, workspace); err != nil {
		t.Fatal(err)
	}
	configureExecution(t, workspace)
	setProvider(t, workspace, server.URL)
	input := filepath.Join(workspace, "interviews", "finding.md")
	put(t, input, "Una consulta de backend no tiene prueba de regresión.")
	// Keep legacy fixture provenance deterministic: a path-derived all-numeric
	// digest can be mistaken for a personal identifier by the old redactor.
	if err := cmdAnalyze([]string{"--ws", workspace, "--privacy-reviewed", "--technical-enabler", "--source-id", "execution-fixture", input}); err != nil {
		t.Fatal(err)
	}
	if err := cmdDiscuss([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	if err := cmdApprove([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	return app, workspace, provider, checks
}

func setProvider(t *testing.T, workspace, url string) {
	t.Helper()
	name := filepath.Join(workspace, "yanai.config.json")
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["openrouter"].(map[string]any)["base_url"] = url
	data, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	put(t, name, string(data))
}

func planFor(c workflow.DecisionContext, specs []ticketSpec) workflow.Proposal {
	p := proposalFor(c, workflow.OutcomeProposeChange)
	p.Tickets = nil
	for _, s := range specs {
		attempts := s.maxAttempts
		if attempts == 0 {
			attempts = 2
		}
		p.Tickets = append(p.Tickets, workflow.Ticket{
			SchemaVersion: "1", ID: s.id, Type: p.Origin, Owner: s.owner,
			Title: "Tarea " + s.id, Description: "Aplicar un cambio de backend acotado.",
			Rationale: p.Rationale, Status: "pending", Evidence: p.Evidence, Scope: p.Scope,
			DependsOn: s.dependsOn, Inputs: c.Inputs, Outputs: s.outputs,
			AllowedPaths: []string{"yanai-server"}, BaseCommit: c.BaseCommit,
			Criteria: []string{"El cambio aplica en el checkout real."}, MaxAttempts: attempts, Revision: 1,
		})
	}
	return p
}

func rounds(t *testing.T, workspace string) []workflow.ExecutionRound {
	t.Helper()
	store := openWorkflowStore(t, workspace)
	defer store.Close()
	out, err := store.Rounds(1)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ticketStatus(t *testing.T, workspace, id string) string {
	t.Helper()
	store := openWorkflowStore(t, workspace)
	defer store.Close()
	rec, err := store.GetTicket(1, id)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Status
}

// TestExecutionContextCarriesCompleteSourceAndAppliesTheDiff is the happy
// path, and it asserts the property the whole format depends on: the model is
// shown the complete current content of every declared output, and an absent
// one is stated as absent rather than shown as empty.
func TestExecutionContextCarriesCompleteSourceAndAppliesTheDiff(t *testing.T) {
	existing := "package example\n\n// the complete preimage, including this trailing comment\n"
	app, workspace, provider, _ := executionSetup(t,
		[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/internal/example/compute.go", "yanai-server/internal/example/SPEC.md"}}},
		map[string]string{"yanai-server/internal/example/compute.go": existing},
		func(ticket string, outputs []string, _ int) string {
			c := workflow.Candidate{SchemaVersion: "1", Ticket: ticket, Result: workflow.CandidateChange, Explanation: "añade una prueba y documenta la carpeta"}
			code := existing + "\nfunc Added() {}\n"
			spec := "# example\n\nCarpeta nueva documentada.\n"
			c.Files = append(c.Files,
				workflow.CandidateFile{Path: "yanai-server/internal/example/compute.go", Operation: workflow.CandidateSource, Source: &code},
				workflow.CandidateFile{Path: "yanai-server/internal/example/SPEC.md", Operation: workflow.CandidateSource, Source: &spec})
			raw, _ := json.Marshal(c)
			return string(raw)
		})
	if err := cmdRun([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	request := provider.lastRequest(t)
	if !strings.Contains(request, existing) {
		t.Fatal("the execution request did not carry the complete current source of a declared output")
	}
	if !strings.Contains(request, "no existe todavía") {
		t.Fatal("an absent declared output was not stated as absent")
	}
	if !strings.Contains(request, "yanai-server/SPEC.md") {
		t.Fatal("the governing SPEC.md of the output's ancestry was not supplied")
	}
	applied, err := os.ReadFile(filepath.Join(app, "yanai-server/internal/example/compute.go"))
	if err != nil || !strings.Contains(string(applied), "func Added()") {
		t.Fatalf("change not applied: %v %s", err, applied)
	}
	if _, err := os.Stat(filepath.Join(app, "yanai-server/internal/example/SPEC.md")); err != nil {
		t.Fatalf("new file not created: %v", err)
	}
	if status := ticketStatus(t, workspace, "T-001"); status != workflow.TicketImplemented {
		t.Fatalf("ticket status = %q", status)
	}
	if st := load(t, workspace); st.Phase != ws.PhaseAwaitingReview {
		t.Fatalf("phase = %q", st.Phase)
	}

	// The manifest links approval, candidate, patch, diff, tested state and
	// checks — and every one of those artifacts is readable and intact.
	all := rounds(t, workspace)
	if len(all) != 1 || all[0].Manifest == nil || all[0].Patch == nil || all[0].Diff == nil {
		t.Fatalf("incomplete round evidence: %+v", all)
	}
	store := openWorkflowStore(t, workspace)
	defer store.Close()
	data, err := (workflow.ArtifactStore{Root: workspace}).Read(store, 1, all[0].Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["tested_state"] != all[0].Confirmed || manifest["tested_state"] == manifest["pre_state"] {
		t.Fatalf("manifest does not pin the tested state: %v", manifest)
	}
	if manifest["approval"].(map[string]any)["contract_hash"] != all[0].Contract {
		t.Fatal("manifest is not linked to the approval it ran under")
	}
	for _, c := range all[0].Checks {
		if !c.Passed || c.Tested != all[0].Confirmed {
			t.Fatalf("check evidence is not tied to the tested state: %+v", c)
		}
	}
}

// TestRunRejectsCandidatesThatCannotBeApplied covers every shape of response
// that must never reach the filesystem. Each one leaves the checkout clean and
// the ticket unimplemented.
func TestRunRejectsCandidatesThatCannotBeApplied(t *testing.T) {
	type rejection struct {
		build func(ticket string, outputs []string) string
		wants string // the reason it must be refused for, when a weaker check would also refuse it
	}
	cases := map[string]rejection{
		// Every declared output is answered *and* an extra path is smuggled in,
		// so only the declared-output rule can catch this one: the
		// missing-output rule, which would also fire on the cases below, is
		// satisfied here.
		"extra undeclared path": {wants: "not a declared output", build: func(ticket string, outputs []string) string {
			return candidateFor(ticket, append(append([]string{}, outputs...), "yanai-ui/view.svelte"), "forbidden")
		}},
		"undeclared path": {build: func(ticket string, outputs []string) string {
			return candidateFor(ticket, []string{"yanai-ui/view.svelte"}, "forbidden")
		}},
		"escaping path": {build: func(ticket string, outputs []string) string {
			return candidateFor(ticket, []string{"../yanai-server/leak.go"}, "forbidden")
		}},
		"protected file type": {build: func(ticket string, outputs []string) string {
			return candidateFor(ticket, []string{"yanai-server/.env"}, "forbidden")
		}},
		"missing declared output": {build: func(ticket string, outputs []string) string {
			c := workflow.Candidate{SchemaVersion: "1", Ticket: ticket, Result: workflow.CandidateChange, Explanation: "incompleto"}
			raw, _ := json.Marshal(c)
			return string(raw)
		}},
		"every output unchanged": {build: func(ticket string, outputs []string) string {
			c := workflow.Candidate{SchemaVersion: "1", Ticket: ticket, Result: workflow.CandidateChange, Explanation: "sin cambios reales"}
			for _, path := range outputs {
				c.Files = append(c.Files, workflow.CandidateFile{Path: path, Operation: workflow.CandidateUnchanged})
			}
			raw, _ := json.Marshal(c)
			return string(raw)
		}},
		"deletes a file that does not exist": {build: func(ticket string, outputs []string) string {
			c := workflow.Candidate{SchemaVersion: "1", Ticket: ticket, Result: workflow.CandidateChange, Explanation: "borra lo que no existe"}
			for _, path := range outputs {
				c.Files = append(c.Files, workflow.CandidateFile{Path: path, Operation: workflow.CandidateDelete})
			}
			raw, _ := json.Marshal(c)
			return string(raw)
		}},
		"legacy fenced output": {build: func(ticket string, outputs []string) string {
			return "=== ARCHIVO: " + outputs[0] + " ===\nlegacy\n=== FIN ARCHIVO ===\n"
		}},
		"truncated JSON": {build: func(ticket string, outputs []string) string {
			return strings.TrimSuffix(candidateFor(ticket, outputs, "x"), "}")
		}},
		"trailing content": {build: func(ticket string, outputs []string) string {
			return candidateFor(ticket, outputs, "x") + "\ngracias!"
		}},
		"source without content": {build: func(ticket string, outputs []string) string {
			c := workflow.Candidate{SchemaVersion: "1", Ticket: ticket, Result: workflow.CandidateChange, Explanation: "fragmento"}
			for _, path := range outputs {
				c.Files = append(c.Files, workflow.CandidateFile{Path: path, Operation: workflow.CandidateSource})
			}
			raw, _ := json.Marshal(c)
			return string(raw)
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app, workspace, _, _ := executionSetup(t,
				[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/nuevo.md"}, maxAttempts: 1}},
				nil,
				func(ticket string, outputs []string, _ int) string { return tc.build(ticket, outputs) })
			err := cmdRun([]string{"--ws", workspace})
			if err == nil {
				t.Fatal("an unapplicable candidate was accepted")
			}
			if tc.wants != "" && !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
			if _, err := os.Stat(filepath.Join(app, "yanai-server/nuevo.md")); !os.IsNotExist(err) {
				t.Fatal("a rejected candidate reached the checkout")
			}
			if status := ticketStatus(t, workspace, "T-001"); status == workflow.TicketImplemented {
				t.Fatal("a rejected candidate was implemented")
			}
			if st := load(t, workspace); st.Phase == ws.PhaseAwaitingReview {
				t.Fatal("a rejected candidate reached the review handoff")
			}
			assertCleanApp(t, app)
		})
	}
}

// assertCleanApp proves the rejection left nothing behind: no partial write,
// no temp file, not even an empty directory a failed patch created.
func assertCleanApp(t *testing.T, app string) {
	t.Helper()
	out, err := exec.Command("git", "-C", app, "status", "--porcelain=v1", "--untracked-files=all").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the application checkout was left dirty:\n%s", out)
	}
}

// TestExecutionRefusesAnOlderContractRevision proves the revision gate itself:
// a contract recorded under the pre-native revision is refused before any
// model call or write, however it came to be recorded that way.
func TestExecutionRefusesAnOlderContractRevision(t *testing.T) {
	app, workspace, provider, _ := executionSetup(t,
		[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/nuevo.md"}}},
		nil,
		func(ticket string, outputs []string, _ int) string { return candidateFor(ticket, outputs, "x") })
	store := openWorkflowStore(t, workspace)
	st := load(t, workspace)
	contract, err := store.Contract(1, st.Approval.ContractHash)
	if err != nil {
		t.Fatal(err)
	}
	contract.Version, contract.Execution = 1, workflow.ExecutionIdentity{}
	payload, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	db := openSQLite(t, workspace)
	if _, err := db.Exec(`UPDATE workflow_contracts SET payload=? WHERE cycle=1 AND hash=?`, string(payload), st.Approval.ContractHash); err != nil {
		t.Fatal(err)
	}
	db.Close()

	err = cmdRun([]string{"--ws", workspace})
	if err == nil || !strings.Contains(err.Error(), "not supported by this workflow") {
		t.Fatalf("older contract executed: %v", err)
	}
	if provider.calls("T-001") != 0 {
		t.Fatal("an older contract reached the model")
	}
	assertCleanApp(t, app)
}

// TestStagedCandidateNeitherPromotesNorUnblocksDependents covers the two ways
// a candidate from the pre-Step-8 flow could have been mistaken for work that
// was done: satisfying a dependency, and being applied without a fresh
// approval of the terms it was produced under.
func TestStagedCandidateNeitherPromotesNorUnblocksDependents(t *testing.T) {
	applied := "nuevo contenido\n"
	app, workspace, provider, _ := executionSetup(t,
		[]ticketSpec{
			{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}},
			{id: "T-002", owner: "ingeniero", outputs: []string{"yanai-server/dos.md"}, dependsOn: []string{"T-001"}},
		},
		nil,
		func(ticket string, outputs []string, _ int) string { return candidateFor(ticket, outputs, applied) })
	stageCandidate(t, workspace, "T-001")
	if status := ticketStatus(t, workspace, "T-001"); status != workflow.TicketCandidateReady {
		t.Fatalf("fixture status = %q", status)
	}

	if err := cmdRun([]string{"--ws", workspace, "--task", "T-002"}); err == nil || !strings.Contains(err.Error(), "only an implemented dependency") {
		t.Fatalf("a staged candidate unblocked its dependent: %v", err)
	}
	if provider.calls("T-002") != 0 {
		t.Fatal("a blocked dependent reached the model")
	}
	if _, err := os.Stat(filepath.Join(app, "yanai-server/dos.md")); !os.IsNotExist(err) {
		t.Fatal("a blocked dependent wrote to the checkout")
	}

	// Running T-001 itself must not adopt the staged candidate: it is
	// withdrawn and a fresh candidate is generated under the current terms.
	if err := cmdRun([]string{"--ws", workspace, "--task", "T-001"}); err != nil {
		t.Fatal(err)
	}
	if provider.calls("T-001") != 1 {
		t.Fatalf("stale candidate was promoted without a fresh generation (%d calls)", provider.calls("T-001"))
	}
	data, err := os.ReadFile(filepath.Join(app, "yanai-server/uno.md"))
	if err != nil || string(data) != applied {
		t.Fatalf("the stale staged content was applied: %v %q", err, data)
	}
	// The withdrawn candidate must also stop being the ticket's recorded one:
	// an implemented ticket still pointing at the artifact nobody applied is
	// evidence that contradicts the checkout.
	store := openWorkflowStore(t, workspace)
	defer store.Close()
	ref, err := store.CandidateRef(1, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.Rounds(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Candidate == nil || ref != all[0].Candidate.ID {
		t.Fatalf("the ticket still points at the stale candidate %q (round %+v)", ref, all)
	}
}

// TestCandidateThatRewritesTheSameBytesIsNotAChange: a response that returns
// the file exactly as it already is has implemented nothing, however
// well-formed it is. It is refused at application rather than recorded as an
// empty patch.
func TestCandidateThatRewritesTheSameBytesIsNotAChange(t *testing.T) {
	existing := "package example\n\n// ya está así\n"
	app, workspace, _, _ := executionSetup(t,
		[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/internal/example/compute.go"}, maxAttempts: 1}},
		map[string]string{"yanai-server/internal/example/compute.go": existing},
		func(ticket string, outputs []string, _ int) string { return candidateFor(ticket, outputs, existing) })
	err := cmdRun([]string{"--ws", workspace})
	if err == nil || !strings.Contains(err.Error(), "changes nothing") {
		t.Fatalf("a candidate identical to the checkout was accepted: %v", err)
	}
	if status := ticketStatus(t, workspace, "T-001"); status == workflow.TicketImplemented {
		t.Fatal("an empty change was implemented")
	}
	assertCleanApp(t, app)
}

// stageCandidate reproduces what the pre-Step-8 flow left behind: a published
// candidate artifact and a ticket at candidate_ready, with no execution round
// for the current contract.
func stageCandidate(t *testing.T, workspace, ticket string) {
	t.Helper()
	store := openWorkflowStore(t, workspace)
	defer store.Close()
	rec, err := store.GetTicket(1, ticket)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err = store.ClaimTicket(1, ticket, "stale-holder", time.Minute, workflow.Event{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err = store.ApplyTicketStatus(1, ticket, workflow.TicketResponseRecorded, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "response.recorded"})
	if err != nil {
		t.Fatal(err)
	}
	ref := workflow.ArtifactRef{ID: "stale-candidate-" + ticket, Path: "cycles/001/entregables/" + ticket + "/respuesta.md", Version: "1"}
	if _, err = (workflow.ArtifactStore{Root: workspace}).Publish(store, 1, ref, []byte("contenido viejo, nunca aplicado ni verificado")); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordCandidate(1, ticket, ref.ID, rec.StateVersion); err != nil {
		t.Fatal(err)
	}
	if err = store.ReleaseClaim(1, ticket, "stale-holder"); err != nil {
		t.Fatal(err)
	}
}

// TestNoChangeIsRecordedButImplementsNothing: an explicit no-change is durable
// evidence with an explanation, and it is not an implementation — it unblocks
// nothing and hands nothing off for review.
func TestNoChangeIsRecordedButImplementsNothing(t *testing.T) {
	app, workspace, _, _ := executionSetup(t,
		[]ticketSpec{
			{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}},
			{id: "T-002", owner: "ingeniero", outputs: []string{"yanai-server/dos.md"}, dependsOn: []string{"T-001"}},
		},
		nil,
		func(ticket string, outputs []string, _ int) string {
			if ticket == "T-001" {
				c := workflow.Candidate{SchemaVersion: "1", Ticket: ticket, Result: workflow.CandidateNoChange,
					Explanation: "la consulta ya está cubierta por una prueba existente; no hace falta tocar nada"}
				raw, _ := json.Marshal(c)
				return string(raw)
			}
			return candidateFor(ticket, outputs, "x")
		})
	err := cmdRun([]string{"--ws", workspace})
	if err == nil || !strings.Contains(err.Error(), "only an implemented dependency") {
		t.Fatalf("a no-change outcome unblocked its dependent: %v", err)
	}
	if status := ticketStatus(t, workspace, "T-001"); status != workflow.TicketNoChangeReported {
		t.Fatalf("T-001 status = %q", status)
	}
	if st := load(t, workspace); st.Phase == ws.PhaseAwaitingReview {
		t.Fatal("a cycle that applied nothing reached the review handoff")
	}
	assertCleanApp(t, app)

	all := rounds(t, workspace)
	if len(all) == 0 || all[0].Outcome != workflow.RoundNoChange || all[0].Explanation == "" || all[0].Manifest == nil {
		t.Fatalf("no-change evidence incomplete: %+v", all)
	}
	if all[0].Patch != nil {
		t.Fatal("a no-change outcome recorded a patch")
	}
}

func openSQLite(t *testing.T, workspace string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(workspace, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestFailedCheckRepairsWithinBudgetAndKeepsTheDiff: a failing check does not
// undo the change. The recorded diff stays in the checkout, the repair round
// is given the real failure output and the real diff, and success is only
// claimed once the checks pass against the state that is actually there.
func TestFailedCheckRepairsWithinBudgetAndKeepsTheDiff(t *testing.T) {
	app, workspace, provider, checks := executionSetup(t,
		[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}, maxAttempts: 2}},
		nil, nil)
	repaired := "segunda versión, ya correcta\n"
	setReply(provider, func(ticket string, outputs []string, round int) string {
		if round == 1 {
			checks.set(t, "fail")
			return candidateFor(ticket, outputs, "primera versión, rota\n")
		}
		checks.set(t, "ok")
		return candidateFor(ticket, outputs, repaired)
	})
	if err := cmdRun([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	if provider.calls("T-001") != 2 {
		t.Fatalf("generations = %d, want 2 (one repair)", provider.calls("T-001"))
	}
	second := provider.lastRequest(t)
	if !strings.Contains(second, "want 3, got 4") {
		t.Fatal("the repair request did not carry the real check failure output")
	}
	if !strings.Contains(second, "primera versión, rota") {
		t.Fatal("the repair request did not carry the diff that is actually in the checkout")
	}
	data, err := os.ReadFile(filepath.Join(app, "yanai-server/uno.md"))
	if err != nil || string(data) != repaired {
		t.Fatalf("repaired content not applied: %v %q", err, data)
	}
	if status := ticketStatus(t, workspace, "T-001"); status != workflow.TicketImplemented {
		t.Fatalf("status = %q", status)
	}
	all := rounds(t, workspace)
	if len(all) != 2 || all[0].State != workflow.RoundFailed || all[1].State != workflow.RoundImplemented {
		t.Fatalf("round history: %+v", all)
	}
	if len(all[0].Checks) == 0 || all[0].Checks[0].Passed {
		t.Fatal("the failed round did not keep its check evidence")
	}
}

// TestExhaustedRepairBudgetStopsAndKeepsTheEvidence: when the ticket's attempt
// limit is spent, the run stops. It does not silently succeed, and it does not
// discard the diff or the evidence that explains why it stopped.
func TestExhaustedRepairBudgetStopsAndKeepsTheEvidence(t *testing.T) {
	broken := "no pasa las pruebas\n"
	app, workspace, provider, checks := executionSetup(t,
		[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}, maxAttempts: 1}},
		nil, nil)
	setReply(provider, func(ticket string, outputs []string, _ int) string {
		checks.set(t, "fail")
		return candidateFor(ticket, outputs, broken)
	})
	err := cmdRun([]string{"--ws", workspace})
	if err == nil || !strings.Contains(err.Error(), "attempt limit exhausted") {
		t.Fatalf("exhausted budget: %v", err)
	}
	if provider.calls("T-001") != 1 {
		t.Fatalf("generations = %d, want 1", provider.calls("T-001"))
	}
	data, err := os.ReadFile(filepath.Join(app, "yanai-server/uno.md"))
	if err != nil || string(data) != broken {
		t.Fatalf("the recorded diff was discarded: %v %q", err, data)
	}
	if status := ticketStatus(t, workspace, "T-001"); status == workflow.TicketImplemented {
		t.Fatal("an unchecked change was implemented")
	}
	if st := load(t, workspace); st.Phase == ws.PhaseAwaitingReview {
		t.Fatal("a failed cycle reached the review handoff")
	}
	all := rounds(t, workspace)
	if len(all) != 1 || all[0].State != workflow.RoundFailed || all[0].Patch == nil || len(all[0].Checks) == 0 {
		t.Fatalf("failure evidence incomplete: %+v", all)
	}
}

// TestUntrustworthyCheckOutcomesNeverCountAsAcceptance covers everything that
// looks like success but is not: forged output with a nonzero exit, a skipped
// database test, a package with no tests, output past the evidence limit, and
// a check that timed out.
func TestUntrustworthyCheckOutcomesNeverCountAsAcceptance(t *testing.T) {
	// A zero exit code is not the claim: output that fabricates success, a
	// skipped database test and a package with no tests are all failures.
	// They are trustworthy failures, so a bounded repair may follow.
	for _, mode := range []string{"forged", "skip", "empty"} {
		t.Run(mode, func(t *testing.T) {
			app, workspace, provider, checks := executionSetup(t,
				[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}, maxAttempts: 1}},
				nil, nil)
			setReply(provider, func(ticket string, outputs []string, _ int) string {
				checks.set(t, mode)
				return candidateFor(ticket, outputs, "contenido\n")
			})
			if err := cmdRun([]string{"--ws", workspace}); err == nil {
				t.Fatal("a fabricated or skipped check result was accepted")
			}
			if status := ticketStatus(t, workspace, "T-001"); status == workflow.TicketImplemented {
				t.Fatalf("%s counted as acceptance", mode)
			}
			if st := load(t, workspace); st.Phase == ws.PhaseAwaitingReview {
				t.Fatal("an unchecked cycle reached the review handoff")
			}
			// The change itself is preserved either way: it is evidence.
			if _, err := os.Stat(filepath.Join(app, "yanai-server/uno.md")); err != nil {
				t.Fatalf("the recorded diff was discarded: %v", err)
			}
		})
	}
	// A timeout or truncated evidence is not a verdict about the code at all,
	// so it must not be answered by generating a different patch: execution
	// stops for a person to look at, with repair budget still available.
	for _, mode := range []string{"slow", "noisy"} {
		t.Run(mode, func(t *testing.T) {
			app, workspace, provider, checks := executionSetup(t,
				[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}, maxAttempts: 3}},
				nil, nil)
			setReply(provider, func(ticket string, outputs []string, _ int) string {
				checks.set(t, mode)
				return candidateFor(ticket, outputs, "contenido\n")
			})
			err := cmdRun([]string{"--ws", workspace})
			if err == nil || !strings.Contains(err.Error(), "trustworthy outcome") {
				t.Fatalf("an unknown check outcome was treated as a verdict: %v", err)
			}
			if provider.calls("T-001") != 1 {
				t.Fatalf("an unknown outcome triggered a repair (%d generations)", provider.calls("T-001"))
			}
			if status := ticketStatus(t, workspace, "T-001"); status == workflow.TicketImplemented {
				t.Fatalf("%s counted as acceptance", mode)
			}
			if _, err := os.Stat(filepath.Join(app, "yanai-server/uno.md")); err != nil {
				t.Fatalf("the recorded diff was discarded: %v", err)
			}
		})
	}
}

// TestChecksRunInAFreshEnvironmentWithoutCredentials proves at the CLI level
// what the executor enforces: the process the approved check runs in inherits
// neither the operator's provider key nor their build flags.
func TestChecksRunInAFreshEnvironmentWithoutCredentials(t *testing.T) {
	_, workspace, provider, checks := executionSetup(t,
		[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}}},
		nil, nil)
	t.Setenv("OPENROUTER_API_KEY", "must-not-leak")
	t.Setenv("GOFLAGS", "-toolexec=evil")
	t.Setenv("DATABASE_URL", "must-not-leak-either")
	setReply(provider, func(ticket string, outputs []string, _ int) string {
		checks.set(t, "env")
		return candidateFor(ticket, outputs, "contenido\n")
	})
	if err := cmdRun([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	dump, err := os.ReadFile(checks.env)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"must-not-leak", "GOFLAGS=", "DATABASE_URL="} {
		if strings.Contains(string(dump), forbidden) {
			t.Fatalf("the check environment carried %q:\n%s", forbidden, dump)
		}
	}
	for _, required := range []string{"GOPROXY=off", "GOTOOLCHAIN=local", "YANAI_TEST_ADMIN_URL="} {
		if !strings.Contains(string(dump), required) {
			t.Fatalf("the check environment is missing %q:\n%s", required, dump)
		}
	}
}

// TestRunResumesWithoutRegeneratingOrReapplying covers the two interruptions
// that leave durable work behind: one after the candidate is recorded but
// before it is applied, and one after the patch is confirmed but before the
// checks produced a trustworthy outcome. Resuming must redo neither.
func TestRunResumesWithoutRegeneratingOrReapplying(t *testing.T) {
	app, workspace, provider, checks := executionSetup(t,
		[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}, maxAttempts: 3}},
		nil, nil)
	intruder := filepath.Join(app, "yanai-server/intruso.md")
	setReply(provider, func(ticket string, outputs []string, round int) string {
		if round == 1 {
			// Someone edits the checkout while the model is answering.
			put(t, intruder, "trabajo de una persona\n")
		}
		return candidateFor(ticket, outputs, "contenido definitivo\n")
	})
	if err := cmdRun([]string{"--ws", workspace}); err == nil {
		t.Fatal("applied a candidate onto an externally changed checkout")
	}
	if data, err := os.ReadFile(intruder); err != nil || string(data) != "trabajo de una persona\n" {
		t.Fatalf("the external edit was not preserved: %v %q", err, data)
	}
	all := rounds(t, workspace)
	if len(all) != 1 || all[0].State != workflow.RoundCandidate || all[0].Patch != nil {
		t.Fatalf("round after the interrupted application: %+v", all)
	}

	// The person resolves their edit; the run resumes from the recorded
	// candidate without paying for another generation.
	if err := os.Remove(intruder); err != nil {
		t.Fatal(err)
	}
	checks.set(t, "dirty")
	if err := cmdRun([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "trustworthy outcome") {
		t.Fatalf("a check that wrote to the repository was accepted: %v", err)
	}
	if provider.calls("T-001") != 1 {
		t.Fatalf("resuming regenerated the candidate (%d calls)", provider.calls("T-001"))
	}
	all = rounds(t, workspace)
	if len(all) != 1 || all[0].State != workflow.RoundApplied || all[0].Patch == nil {
		t.Fatalf("round after the untrustworthy check: %+v", all)
	}
	patch := all[0].Patch.ID
	stray := filepath.Join(app, "yanai-server/stray-check-output.md")
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("the check's unexpected write was deleted instead of preserved: %v", err)
	}

	// The operator inspects and removes the stray file; the run resumes at the
	// confirmed patch and only re-runs the checks.
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	checks.set(t, "ok")
	if err := cmdRun([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	if provider.calls("T-001") != 1 {
		t.Fatalf("resuming regenerated the candidate (%d calls)", provider.calls("T-001"))
	}
	all = rounds(t, workspace)
	if len(all) != 1 || all[0].State != workflow.RoundImplemented || all[0].Patch.ID != patch {
		t.Fatalf("resuming reapplied the patch: %+v", all)
	}
	if data, err := os.ReadFile(filepath.Join(app, "yanai-server/uno.md")); err != nil || string(data) != "contenido definitivo\n" {
		t.Fatalf("applied content: %v %q", err, data)
	}

	// A corrupted projection changes nothing: the store is the authority, and
	// a rerun regenerates the projection rather than reading it back.
	put(t, filepath.Join(workspace, "cycles/001/state.json"), "{ not json")
	if err := cmdRun([]string{"--ws", workspace}); err != nil {
		t.Fatalf("rerun after a corrupted projection: %v", err)
	}
	if st := load(t, workspace); st.Phase != ws.PhaseAwaitingReview {
		t.Fatalf("phase after rerun = %q", st.Phase)
	}
}

// TestActiveTimeExhaustionStopsBeforeAnyModelCall: the cycle's active-work
// budget is spent, so nothing is generated, applied or checked.
func TestActiveTimeExhaustionStopsBeforeAnyModelCall(t *testing.T) {
	app, workspace, provider, _ := executionSetup(t,
		[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}}},
		nil,
		func(ticket string, outputs []string, _ int) string { return candidateFor(ticket, outputs, "x") })
	db := openSQLite(t, workspace)
	if _, err := db.Exec(`UPDATE workflow_budgets SET active_ms = 1000 * (SELECT json_extract(policy,'$.max_active_seconds') FROM workflow_budgets WHERE cycle=1) WHERE cycle=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := cmdRun([]string{"--ws", workspace}); err == nil || !strings.Contains(err.Error(), "active-time budget exhausted") {
		t.Fatalf("exhausted active time: %v", err)
	}
	if provider.calls("T-001") != 0 {
		t.Fatal("an exhausted budget still reached the model")
	}
	assertCleanApp(t, app)
}

// TestTamperedExecutionEvidenceBlocksFurtherWork: evidence is only evidence
// while it still hashes to what the store recorded. A rewritten manifest stops
// the next ticket before it is generated, not after.
func TestTamperedExecutionEvidenceBlocksFurtherWork(t *testing.T) {
	_, workspace, provider, _ := executionSetup(t,
		[]ticketSpec{
			{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}},
			{id: "T-002", owner: "ingeniero", outputs: []string{"yanai-server/dos.md"}},
		},
		nil,
		func(ticket string, outputs []string, _ int) string {
			return candidateFor(ticket, outputs, "contenido\n")
		})
	if err := cmdRun([]string{"--ws", workspace, "--task", "T-001"}); err != nil {
		t.Fatal(err)
	}
	store := openWorkflowStore(t, workspace)
	all, err := store.Rounds(1)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.GetArtifact(1, all[0].Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	put(t, filepath.Join(workspace, record.Path), `{"schema":"1","outcome":"implemented"}`)

	before := provider.calls("T-002")
	if err := cmdRun([]string{"--ws", workspace, "--task", "T-002"}); err == nil || !strings.Contains(err.Error(), "execution evidence") {
		t.Fatalf("tampered evidence: %v", err)
	}
	if provider.calls("T-002") != before {
		t.Fatal("tampering was detected only after a paid call")
	}
}

// TestStatusAndReconciliationNeedNoProviderAfterExecution: reading what
// happened must not require a provider key — that is exactly the situation an
// operator is in when something went wrong.
func TestStatusAndReconciliationNeedNoProviderAfterExecution(t *testing.T) {
	_, workspace, _, _ := executionSetup(t,
		[]ticketSpec{{id: "T-001", owner: "ingeniero", outputs: []string{"yanai-server/uno.md"}}},
		nil,
		func(ticket string, outputs []string, _ int) string {
			return candidateFor(ticket, outputs, "contenido\n")
		})
	if err := cmdRun([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YANAI_MOCK", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	if err := cmdStatus([]string{"--ws", workspace}); err != nil {
		t.Fatal(err)
	}
	if err := cmdStatus([]string{"--ws", workspace, "--attempts"}); err != nil {
		t.Fatal(err)
	}
}
