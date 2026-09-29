package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanai/yanai-harness/internal/workflow"
)

func TestConfiguredToolChecks(t *testing.T) {
	binary, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("requires installed python3")
	}
	// macOS /usr/bin/python3 may be an xcrun launcher; use the actual runtime.
	runtime, err := exec.Command(binary, "-c", "import sys; print(sys.executable)").Output()
	must(t, err)
	binary, err = filepath.Abs(strings.TrimSpace(string(runtime)))
	must(t, err)
	toolDir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	invocation := filepath.Join(toolDir, "python3")
	must(t, os.Symlink(binary, invocation))
	tools := map[string]workflow.CheckTool{"python": {Binary: invocation}}
	checks := []workflow.Check{
		{ID: "pass", Args: []string{"python", "-B", "-c", `import os, sys; assert "OPENROUTER_API_KEY" not in os.environ; assert sys.executable == sys.argv[1], sys.executable; print("1 assertion passed")`, invocation}, Dir: "yanai-server", TimeoutSeconds: 10, Evidence: "output", SuccessPattern: "1 assertion passed"},
		{ID: "empty", Args: []string{"python", "-B", "-c", "pass"}, Dir: "yanai-server", TimeoutSeconds: 10, Evidence: "output", SuccessPattern: "1 assertion passed"},
		{ID: "skip", Args: []string{"python", "-B", "-c", `print("1 assertion passed; skipped")`}, Dir: "yanai-server", TimeoutSeconds: 10, Evidence: "output", SuccessPattern: "1 assertion passed", FailurePattern: "skipped"},
		{ID: "exit", Args: []string{"python", "-B", "-c", "raise SystemExit(7)"}, Dir: "yanai-server", TimeoutSeconds: 10, Evidence: "exit_code"},
	}
	t.Setenv("OPENROUTER_API_KEY", "must-not-inherit")
	t.Setenv(GoBinaryEnv, "/nonexistent-go")
	o, n := fixture(t, checks, tools)
	_, err = Defaults(o)
	must(t, err)
	for _, c := range checks {
		result, err := n.Check(context.Background(), c.ID)
		if c.ID == "pass" {
			if err != nil {
				t.Fatalf("%v\n%s", err, result.Output)
			}
		} else if err == nil {
			t.Fatalf("%s falsely accepted: %+v", c.ID, result)
		}
		if result.Evidence.SHA256 == "" || result.Before != result.After {
			t.Fatalf("missing or dirty evidence: %+v", result)
		}
		if c.ID == "empty" && !strings.Contains(result.Error, "success pattern missing") {
			t.Fatal(result.Error)
		}
		if c.ID == "skip" && !strings.Contains(result.Error, "failure pattern matched") {
			t.Fatal(result.Error)
		}
		if c.ID == "exit" && result.ExitCode != 7 {
			t.Fatal(result.ExitCode)
		}
	}
	// Any installed program may be a check tool: the human approves each check
	// command in the review. A tool still needs a simple name and an absolute,
	// executable binary.
	for _, name := range []string{"git", "bash", "docker", "rm"} {
		if err := workflow.ValidateCheckTool(name, workflow.CheckTool{Binary: "/usr/bin/" + name}); err != nil {
			t.Fatal(name, err)
		}
	}
	if workflow.ValidateCheckTool("tool", workflow.CheckTool{Binary: "bin/tool"}) == nil {
		t.Fatal("accepted a relative tool binary")
	}
	if _, err := n.resolveCheckTool("tool", workflow.CheckTool{Binary: filepath.Join(o.Artifacts.Root, "missing")}); err == nil {
		t.Fatal("accepted a missing binary")
	}
	for _, args := range [][]string{{"unknown", "test"}, {"/abs/script.sh"}, {"../outside.sh"}} {
		c := checks[0]
		c.Args = args
		if validateCheck(c, tools) == nil && workflow.ValidateCheckArguments(args) == nil {
			t.Fatal("accepted", args)
		}
	}
	// A program the repository provides runs from its path inside the checkout.
	must(t, os.WriteFile(filepath.Join(o.Repo.Path, "yanai-server", "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	script := workflow.Check{ID: "script", Args: []string{"./check.sh"}, Dir: "yanai-server", TimeoutSeconds: 10, Evidence: "exit_code"}
	if err := validateCheck(script, tools); err != nil {
		t.Fatal(err)
	}
	if _, err := n.resolveRepositoryProgram("yanai-server", "./check.sh"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.resolveRepositoryProgram("yanai-server", "../../outside.sh"); err == nil {
		t.Fatal("resolved a program outside the repository")
	}
	c := checks[0]
	c.Evidence = ""
	if validateCheck(c, tools) == nil {
		t.Fatal("missing evidence accepted")
	}
	c = checks[0]
	c.SuccessPattern = "["
	if validateCheck(c, tools) == nil {
		t.Fatal("invalid regex accepted")
	}
}
