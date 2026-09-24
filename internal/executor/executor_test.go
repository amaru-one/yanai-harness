package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func put(t *testing.T, root, path, text string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755))
	must(t, os.WriteFile(filepath.Join(root, path), []byte(text), 0o644))
}
func git(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, b)
	}
	return b
}
func source(s string) *File { return &File{Data: []byte(s), Mode: 0o644} }
func fixture(t *testing.T, checks []workflow.Check, tools ...map[string]workflow.CheckTool) (Options, *Native) {
	t.Helper()
	parent := t.TempDir()
	repo := filepath.Join(parent, "yanai")
	workspace := filepath.Join(parent, "harness")
	must(t, os.MkdirAll(workspace, 0o700))
	for path, text := range map[string]string{"yanai-server/go.mod": "module yanai-server\n\ngo 1.26.6\n", "yanai-server/a.go": "package sample\n", "yanai-server/b.md": "delete me\n", "yanai-server/vendor/kept.md": "fingerprint includes excluded context\n", "AGENTS.md": "fixture\n"} {
		put(t, repo, path, text)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "baseline")
	s, err := workflow.OpenStore(filepath.Join(workspace, "workflow.db"), "test")
	must(t, err)
	t.Cleanup(func() { s.Close() })
	o := Options{Repo: config.Repo{Path: repo, AllowedPaths: []string{"yanai-server"}, ExcludeDirs: []string{"vendor"}}, Store: s, Artifacts: workflow.ArtifactStore{Root: workspace}, Cycle: 1, GoCache: t.TempDir(), ModuleCache: t.TempDir(), CheckInputs: map[string]string{"TEST_DATABASE_URL": "postgres://test:test@127.0.0.1:55432/postgres?sslmode=disable"}}
	approve(t, &o, checks, []string{"yanai-server/a.go", "yanai-server/b.md", "yanai-server/new/deep/c.md", "yanai-server/0 new.md", "yanai-server/tab\t.md", "yanai-server/é.md"}, tools...)
	n, err := Open(o)
	must(t, err)
	t.Cleanup(func() { n.Close() })
	return o, n
}
func approve(t *testing.T, o *Options, checks []workflow.Check, outputs []string, tools ...map[string]workflow.CheckTool) {
	t.Helper()
	target, err := repository.Open(o.Repo, o.Artifacts.Root)
	must(t, err)
	snapshot, err := target.Snapshot()
	must(t, err)
	c, err := o.Store.CreateCycle(1, workflow.TechnicalEnabler)
	must(t, err)
	c, err = o.Store.ApplyCyclePhase(1, workflow.PhaseAnalyzed, workflow.ActorEngine, c.StateVersion, workflow.CycleFields{}, workflow.Event{Type: "analyzed"})
	must(t, err)
	c, err = o.Store.ApplyCyclePhase(1, workflow.PhaseAwaitingApproval, workflow.ActorEngine, c.StateVersion, workflow.CycleFields{}, workflow.Event{Type: "planned"})
	must(t, err)
	raw, err := json.Marshal(snapshot)
	must(t, err)
	branch, err := target.Branch()
	must(t, err)
	identity := workflow.ExecutionIdentity{Backend: workflow.ExecutionBackendNative, CandidateSchema: workflow.CandidateSchemaVersion, Root: snapshot.Root, CommonDir: snapshot.CommonDir, Head: snapshot.Head, Branch: branch}
	contract := workflow.ExecutionContract{Version: workflow.ContractVersion, Execution: identity, Plan: workflow.Proposal{ID: "p", Tickets: []workflow.Ticket{{ID: "T-1", AllowedPaths: []string{"yanai-server"}, Outputs: outputs}}}, ContextHash: "context", Baseline: snapshot.Baseline(), Repository: raw, Policy: workflow.ExecutionPolicy{MaxTokens: 10000, MaxCostUSD: 10, MaxActiveSeconds: 900, MaxCalls: 5, MaxRepairs: 2, Checks: checks}}
	if len(tools) > 0 {
		contract.Policy.Tools = tools[0]
	}
	o.Contract, err = o.Store.SaveContract(1, contract)
	must(t, err)
	ph, err := workflow.Hash(contract.Plan)
	must(t, err)
	must(t, o.Store.ApproveContract(workflow.Approval{ID: "A-1", Actor: workflow.ActorHuman, Cycle: 1, ContractHash: o.Contract, PlanHash: ph, ScopeHash: contract.ContextHash, Baseline: snapshot.Baseline(), ApprovedAt: time.Now()}, c.StateVersion, "{}"))
}
func edits() []Edit {
	return []Edit{{"yanai-server/a.go", source("package sample\n"), source("package sample\n// changed\n")}, {"yanai-server/b.md", source("delete me\n"), nil}, {"yanai-server/new/deep/c.md", nil, source("new\n")}}
}

func TestNativePatchOracleAndTools(t *testing.T) {
	o, n := fixture(t, nil)
	before, err := n.Read("yanai-server/a.go")
	must(t, err)
	if !same(before, edits()[0].Before) {
		t.Fatal("read source differs")
	}
	matches, err := n.Search([]string{"yanai-server/a.go", "yanai-server/b.md"}, "sample")
	must(t, err)
	if len(matches) != 1 || matches[0].Line != 1 {
		t.Fatalf("search: %+v", matches)
	}
	patch := append(edits(), Edit{"yanai-server/0 new.md", nil, source("")}, Edit{"yanai-server/tab\t.md", nil, source("tab")}, Edit{"yanai-server/é.md", nil, source("utf8")})
	receipt, err := n.Apply("T-1", patch)
	must(t, err)
	status := git(t, o.Repo.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	expected := " M yanai-server/a.go\x00 D yanai-server/b.md\x00?? yanai-server/0 new.md\x00?? yanai-server/new/deep/c.md\x00?? yanai-server/tab\t.md\x00?? yanai-server/é.md\x00"
	if string(status) != expected {
		t.Fatalf("oracle status %q", status)
	}
	recorded, _, err := o.Store.PatchState(1, o.Contract)
	must(t, err)
	if recorded.StatusHash != fmt.Sprintf("%x", sha256.Sum256(status)) || recorded.Baseline() != receipt.After {
		t.Fatal("wrong recorded successor")
	}
	if _, ok := recorded.Content["yanai-server/vendor/kept.md"]; !ok {
		t.Fatal("fingerprint incorrectly narrowed to read policy")
	}
	data, err := o.Artifacts.Read(o.Store, 1, receipt.Patch.ID)
	must(t, err)
	var j journal
	must(t, json.Unmarshal(data, &j))
	if patch[0].Path != "yanai-server/a.go" {
		t.Fatal("executor reordered caller input")
	}
	ordered := append([]Edit(nil), patch...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	if !reflect.DeepEqual(j.Edits, ordered) {
		t.Fatal("durable full sources differ")
	}
	d, err := n.Inspect()
	must(t, err)
	if !reflect.DeepEqual(d.Edits, ordered) {
		t.Fatalf("diff: %+v", d)
	}
	// A second own edit works from a dirty but exactly recorded predecessor;
	// returning one file to HEAD must REMOVE its status record.
	_, err = n.Apply("T-1", []Edit{{patch[0].Path, patch[0].After, patch[0].Before}})
	must(t, err)
	if strings.Contains(string(git(t, o.Repo.Path, "status", "--porcelain=v1")), "a.go") {
		t.Fatal("reverted file still dirty")
	}
	if _, err = Open(o); err == nil {
		t.Fatal("second writer acquired repository")
	}
	must(t, n.Close())
	reopened, err := Open(o)
	must(t, err)
	defer reopened.Close()
	if _, err = reopened.Inspect(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRejectsUnsafeInputs(t *testing.T) {
	o, n := fixture(t, nil)
	tests := []struct {
		name string
		edit Edit
	}{
		{"truncated", Edit{"yanai-server/a.go", source("package"), source("replacement")}},
		{"undeclared", Edit{"yanai-server/other.go", nil, source("bad")}},
		{"escape", Edit{"../outside", nil, source("bad")}},
		{"ui", Edit{"yanai-ui/a.go", nil, source("bad")}},
		{"secret", Edit{"yanai-server/secret.txt", nil, source("bad")}},
		{"empty", Edit{"yanai-server/a.go", source("package sample\n"), source("package sample\n")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := n.Apply("T-1", []Edit{tc.edit}); err == nil {
				t.Fatal("unsafe edit accepted")
			}
		})
	}
	// Internal as well as escaping symlinks are refused at the actual I/O layer.
	for _, dest := range []string{t.TempDir(), "yanai-server"} {
		must(t, os.Symlink(dest, filepath.Join(o.Repo.Path, "link")))
		if _, err := n.parent("link/file"); err == nil {
			t.Fatal("symlink directory opened")
		}
		must(t, os.Remove(filepath.Join(o.Repo.Path, "link")))
	}
	if _, err := n.Read("yanai-server/vendor/kept.md"); err == nil {
		t.Fatal("read policy widened")
	}
	if _, err := n.Apply("T-1", nil); err == nil {
		t.Fatal("empty patch accepted")
	}
	put(t, o.Repo.Path, "yanai-server/b.md", "external")
	if _, err := n.Apply("T-1", edits()); err == nil {
		t.Fatal("external edit accepted")
	}
	b, err := os.ReadFile(filepath.Join(o.Repo.Path, "yanai-server/b.md"))
	must(t, err)
	if string(b) != "external" {
		t.Fatal("external edit lost")
	}
}

func TestNativeRollbackOnWriteFailureAndWrongPrediction(t *testing.T) {
	for _, stage := range []string{"prepared", "staged", "write:0", "write:1", "observed", "prediction"} {
		t.Run(stage, func(t *testing.T) {
			o, n := fixture(t, nil)
			before := n.base.Baseline()
			n.fault = func(at string) error {
				if at == stage {
					return errors.New("injected I/O failure")
				}
				return nil
			}
			if stage == "prediction" {
				// Git ignores executable-bit changes with core.filemode=false. Our
				// conservative prediction says M; the real oracle says clean.
				git(t, o.Repo.Path, "config", "core.filemode", "false")
				f := source("package sample\n")
				f.Mode = 0o755
				_, err := n.Apply("T-1", []Edit{{"yanai-server/a.go", source("package sample\n"), f}})
				if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
					t.Fatalf("prediction mismatch accepted: %v", err)
				}
			} else {
				if _, err := n.Apply("T-1", edits()); err == nil {
					t.Fatal("injected failure ignored")
				}
			}
			snapshot, err := n.target.Snapshot()
			must(t, err)
			if snapshot.Baseline() != before {
				t.Fatal("partial patch remains")
			}
			recorded, _, err := o.Store.PatchState(1, o.Contract)
			must(t, err)
			if recorded.Baseline() != before {
				t.Fatal("rollback not reconciled")
			}
			if _, err = os.Stat(filepath.Join(o.Repo.Path, "yanai-server/new")); !os.IsNotExist(err) {
				t.Fatal("new directory leaked")
			}
		})
	}
}

type childConfig struct {
	Repo                       config.Repo
	Workspace, Contract, Stage string
}

func TestNativeCrashRecovery(t *testing.T) {
	if path := os.Getenv("YANAI_EXECUTOR_CRASH_HELPER"); path != "" {
		data, err := os.ReadFile(path)
		must(t, err)
		var cfg childConfig
		must(t, json.Unmarshal(data, &cfg))
		store, err := workflow.OpenStore(filepath.Join(cfg.Workspace, "workflow.db"), "test")
		must(t, err)
		n, err := Open(Options{Repo: cfg.Repo, Store: store, Artifacts: workflow.ArtifactStore{Root: cfg.Workspace}, Cycle: 1, Contract: cfg.Contract})
		must(t, err)
		n.fault = func(stage string) error {
			if stage == cfg.Stage {
				os.Exit(73)
			}
			return nil
		}
		_, err = n.Apply("T-1", edits())
		t.Fatalf("did not crash: %v", err)
	}
	for _, stage := range []string{"published", "prepared", "staged", "write:0", "write:1", "observed", "external"} {
		t.Run(stage, func(t *testing.T) {
			o, n := fixture(t, nil)
			must(t, n.Close())
			must(t, o.Store.Close())
			crash := stage
			if stage == "external" {
				crash = "write:0"
			}
			data, err := json.Marshal(childConfig{o.Repo, o.Artifacts.Root, o.Contract, crash})
			must(t, err)
			configPath := filepath.Join(t.TempDir(), "child.json")
			must(t, os.WriteFile(configPath, data, 0o600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeCrashRecovery$")
			cmd.Env = append(os.Environ(), "YANAI_EXECUTOR_CRASH_HELPER="+configPath)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("child: %v %s", err, output)
			}
			o.Store, err = workflow.OpenStore(filepath.Join(o.Artifacts.Root, "workflow.db"), "test")
			must(t, err)
			defer o.Store.Close()
			if stage == "external" {
				put(t, o.Repo.Path, "yanai-server/a.go", "human edit\n")
			}
			recovered, err := Open(o)
			if stage == "external" {
				if err == nil {
					recovered.Close()
					t.Fatal("external edit adopted")
				}
				b, err := os.ReadFile(filepath.Join(o.Repo.Path, "yanai-server/a.go"))
				must(t, err)
				if string(b) != "human edit\n" {
					t.Fatal("external edit erased")
				}
				return
			}
			must(t, err)
			defer recovered.Close()
			current, _, err := o.Store.PatchState(1, o.Contract)
			must(t, err)
			if stage == "observed" {
				if !current.Dirty {
					t.Fatal("completed patch blindly rolled back")
				}
			} else if current.Baseline() != recovered.base.Baseline() {
				t.Fatal("partial patch not rolled back")
			}
			actual, err := recovered.target.Snapshot()
			must(t, err)
			if actual.Baseline() != current.Baseline() {
				t.Fatal("recovery disagrees with disk")
			}
			if stage == "published" {
				_, err = recovered.Apply("T-1", edits())
				must(t, err)
			}
		})
	}
}

func TestNativeCheckBoundaryAndEvidence(t *testing.T) {
	checks := []workflow.Check{{ID: "test", RequiredEnv: []string{"TEST_DATABASE_URL"}, PostgresURLVar: "TEST_DATABASE_URL", Args: []string{"go", "test", "-race", "-shuffle=on", "./..."}, Dir: "yanai-server", TimeoutSeconds: 1}}
	o, n := fixture(t, checks)
	script := filepath.Join(t.TempDir(), "go")
	must(t, os.WriteFile(script, []byte("#!/bin/sh\npwd\nprintf '%s\\n' \"$@\"\nenv | /usr/bin/grep -E '^(OPENROUTER_API_KEY|GOFLAGS|DATABASE_URL|GOPROXY|TEST_DATABASE_URL)='\n"), 0o755))
	n.o.GoBinary = script
	t.Setenv("OPENROUTER_API_KEY", "must-not-leak")
	t.Setenv("GOFLAGS", "-toolexec=evil")
	t.Setenv("DATABASE_URL", "must-not-leak-either")
	result, err := n.Check(context.Background(), "test")
	must(t, err)
	if result.ExitCode != 0 || result.Before != result.After || !strings.Contains(result.Output, "yanai-server") || strings.Contains(result.Output, "must-not-leak") || strings.Contains(result.Output, "GOFLAGS=") || !strings.Contains(result.Output, "GOPROXY=off") || strings.Contains(result.Output, o.CheckInputs["TEST_DATABASE_URL"]) {
		t.Fatalf("unsafe evidence: %+v", result)
	}
	evidence, err := o.Artifacts.Read(o.Store, 1, result.Evidence.ID)
	must(t, err)
	var recorded CheckResult
	must(t, json.Unmarshal(evidence, &recorded))
	canonicalScript, err := filepath.EvalSymlinks(script)
	must(t, err)
	if recorded.ExitCode != 0 || recorded.Binary != canonicalScript || !reflect.DeepEqual(recorded.Args, result.Args) {
		t.Fatal("command evidence incomplete")
	}
	if _, err = n.Check(context.Background(), "unapproved"); err == nil {
		t.Fatal("unknown check executed")
	}
	n.o.CheckInputs["TEST_DATABASE_URL"] = ""
	if _, err = n.Check(context.Background(), "test"); err == nil {
		t.Fatal("database skipping allowed")
	}
	n.o.CheckInputs["TEST_DATABASE_URL"] = "postgres://test:test@127.0.0.1:55432/postgres?sslmode=disable"
	must(t, os.WriteFile(script, []byte("#!/bin/sh\necho failed\nexit 7\n"), 0o755))
	result, err = n.Check(context.Background(), "test")
	if err == nil || result.ExitCode != 7 || !strings.Contains(result.Output, "failed") {
		t.Fatalf("failed check: %+v %v", result, err)
	}
	must(t, os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\necho child=$!\nwait\n"), 0o755))
	start := time.Now()
	result, err = n.Check(context.Background(), "test")
	if err == nil || !result.TimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout failed: %+v %v", result, err)
	}
	child, parseErr := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(result.Output), "child="))
	must(t, parseErr)
	// Observe the descendant, not just the shell's exit or closed output pipes.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(child, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatal("check descendant survived timeout")
	}
	must(t, os.WriteFile(script, []byte("#!/bin/sh\nyes x | head -c 1100000\n"), 0o755))
	result, err = n.Check(context.Background(), "test")
	if err == nil || !result.Truncated || len(result.Output) > maxCheckOutput {
		t.Fatal("output limit did not fail check")
	}

	// A start with no finish cannot be treated as a command that did not run.
	_, err = n.publish("check-interrupted-started", CheckResult{ID: "check-interrupted"})
	must(t, err)
	if _, err = n.Check(context.Background(), "test"); err == nil || !strings.Contains(err.Error(), "unfinished check") {
		t.Fatal("interrupted check rerun")
	}
	if _, err = n.Apply("T-1", edits()); err == nil || !strings.Contains(err.Error(), "unfinished check") {
		t.Fatal("patch raced interrupted check")
	}
	// Test-only operator reconciliation permits the remaining boundary assertion.
	_, err = n.publish("check-interrupted-finished", CheckResult{ID: "check-interrupted", ExitCode: -1, Error: "test fixture reconciled"})
	must(t, err)
	must(t, os.WriteFile(script, []byte("#!/bin/sh\necho external > b.md\n"), 0o755))
	result, err = n.Check(context.Background(), "test")
	if err == nil || !strings.Contains(result.Error, "unexpected repository") {
		t.Fatal("check mutation accepted")
	}
}

func TestNativeRejectsCommands(t *testing.T) {
	for _, args := range [][]string{{"git", "reset", "--hard"}, {"sh", "-c", "true"}, {"go", "run", "./..."}, {"go", "test", "-exec=evil", "./..."}, {"go", "test", "-toolexec=evil", "./..."}, {"go", "build", "-o=/tmp/out", "./..."}, {"go", "test", "../..."}, {"/usr/bin/go", "test", "./..."}} {
		if err := validateCheck(workflow.Check{ID: "bad", Args: args, Dir: "yanai-server", TimeoutSeconds: 1}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, raw := range []string{"postgres://u:p@remote:5432/postgres", "postgres://u:p@localhost/postgres", "postgres://u:p@localhost:5432/postgres?host=remote"} {
		if err := localDatabase(raw); err == nil {
			t.Fatal("remote/overridden DB accepted")
		}
	}
	for _, verb := range []string{"test", "vet", "build"} {
		must(t, validateCheck(workflow.Check{ID: verb, Args: []string{"go", verb, "./..."}, Dir: "yanai-server", TimeoutSeconds: 30}))
	}
}

// Opt-in integration: runs approved checks against an existing clean backend,
// without applying application patches. The fixture contract is test-only.
