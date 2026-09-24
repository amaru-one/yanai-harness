package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
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

func openWorkflowStore(t *testing.T, workspace string) *workflow.Store {
	t.Helper()
	cfg, err := config.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	project := strings.TrimSpace(cfg.Project)
	if project == "" {
		project = filepath.Base(workspace)
	}
	store, err := workflow.OpenStore(filepath.Join(workspace, "workflow.db"), project)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func load(t *testing.T, workspace string) *ws.State {
	t.Helper()
	w, err := ws.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	st, err := w.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func stubToolchain(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	goPath := filepath.Join(dir, "go")
	put(t, goPath, "#!/bin/sh\nprintf 'ok  example.test/project\\t0.01s\\n'\n")
	if err := os.Chmod(goPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YANAI_GO_BINARY", goPath)
	t.Setenv("YANAI_GO_CACHE", t.TempDir())
	t.Setenv("YANAI_GO_MODCACHE", t.TempDir())
	t.Setenv("YANAI_TEST_ADMIN_URL", "")
}
