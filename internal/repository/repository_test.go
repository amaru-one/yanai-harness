package repository_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repoctx"
	"github.com/yanai/yanai-harness/internal/repository"
)

func write(t *testing.T, root, path, text string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	} else {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func fixture(t *testing.T) config.Repo {
	t.Helper()
	root := t.TempDir()
	write(t, root, "yanai-server/go.mod", "module yanai-server\n\ngo 1.24\n")
	write(t, root, "yanai-server/SPEC.md", "BACKEND_SPEC\n")
	write(t, root, "yanai-server/main.go", "package main\n// BACKEND_CODE\n")
	write(t, root, "AGENTS.md", "GOVERNING_INSTRUCTIONS\n")
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "baseline")
	return config.Repo{Path: root, Extensions: []string{".go", ".md", ".json", ".mod"}, ExcludeDirs: []string{"vendor"}}
}

func open(t *testing.T, r config.Repo) *repository.Target {
	t.Helper()
	target, err := repository.Open(r, "")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestRejectWrongTargetsAndWorkspaceOverlap(t *testing.T) {
	r := fixture(t)
	for _, path := range []string{"", filepath.Join(r.Path, "missing"), filepath.Join(r.Path, "yanai-server"), t.TempDir()} {
		bad := r
		bad.Path = path
		if _, err := repository.Open(bad, ""); err == nil {
			t.Errorf("accepted invalid target %q", path)
		}
	}
	for _, ws := range []string{r.Path, filepath.Join(r.Path, "workspace"), filepath.Dir(r.Path)} {
		if _, err := repository.Open(r, ws); err == nil {
			t.Errorf("accepted overlapping workspace %s", ws)
		}
	}
	alias := filepath.Join(t.TempDir(), "repo-alias")
	if err := os.Symlink(r.Path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Open(r, filepath.Join(alias, "workspace")); err == nil {
		t.Fatal("accepted overlapping symlink workspace")
	}
	bad := r
	bad.AllowedPaths = []string{"yanai-ui"}
	if _, err := repository.Open(bad, ""); err == nil {
		t.Fatal("configuration widened backend boundary")
	}
	write(t, r.Path, "yanai-server/go.mod", "module github.com/yanai/yanai-harness\n")
	if _, err := repository.Open(r, ""); err == nil {
		t.Fatal("accepted wrong module")
	}
}

func TestGitEnvironmentCannotRedirectTarget(t *testing.T) {
	r := fixture(t)
	other := fixture(t)
	t.Setenv("GIT_DIR", filepath.Join(other.Path, ".git"))
	t.Setenv("GIT_WORK_TREE", other.Path)
	target := open(t, r)
	canonical, err := repository.Canonical(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if target.Root != canonical {
		t.Fatal("redirected target")
	}
}

func TestReadPolicyAppliesToIndexAndExplicitReads(t *testing.T) {
	r := fixture(t)
	write(t, r.Path, ".gitignore", "yanai-server/ignored.md\n")
	denied := []string{"yanai-ui/SPEC.md", "notes.md", "yanai-server/.env", "yanai-server/.hidden/SPEC.md", "yanai-server/secrets.json", "yanai-server/credentials.json", "yanai-server/private.key", "yanai-server/vendor/SPEC.md", "yanai-server/ignored.md"}
	for _, p := range denied {
		write(t, r.Path, p, "DO_NOT_EXPOSE_CONTENT")
	}
	outside := t.TempDir()
	write(t, outside, "secret.md", "DO_NOT_EXPOSE_CONTENT")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(r.Path, "yanai-server", "leak.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.Path, "yanai-server", "linked")); err != nil {
		t.Fatal(err)
	}
	denied = append(denied, "yanai-server/leak.md", "yanai-server/linked/secret.md", "../yanai-server/main.go", "/yanai-server/main.go", "yanai-server/../yanai-ui/SPEC.md", "yanai-server/../../outside", ".git/config")
	target := open(t, r)
	for _, p := range denied {
		if err := target.CheckPath(p); err == nil {
			t.Errorf("allowed protected path %q", p)
		}
	}
	index, err := repoctx.Index(r)
	if err != nil {
		t.Fatal(err)
	}
	files, err := repoctx.Files(r, append(denied, "yanai-server/main.go", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{index, files} {
		if strings.Contains(text, "DO_NOT_EXPOSE_CONTENT") {
			t.Fatal("exposed denied content")
		}
		if !strings.Contains(text, "DIRTY") {
			t.Fatal("dirty planning context was not labelled")
		}
		if !strings.Contains(text, "GOVERNING_INSTRUCTIONS") {
			t.Fatal("missing root instructions")
		}
	}
	if strings.Contains(index, "yanai-ui/") || strings.Contains(index, "ignored.md") {
		t.Fatal("index exposed excluded entries")
	}
	if !strings.Contains(files, "BACKEND_CODE") {
		t.Fatal("allowed source missing")
	}
	if err := target.CheckPath("yanai-server/internal/new/new.go"); err != nil {
		t.Fatalf("new backend output rejected: %v", err)
	}
}

func TestModuleSymlinkRejected(t *testing.T) {
	r := fixture(t)
	other := fixture(t)
	name := filepath.Join(r.Path, "yanai-server", "go.mod")
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other.Path, "yanai-server", "go.mod"), name); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Open(r, ""); err == nil {
		t.Fatal("accepted symlinked module")
	}
}

func TestWriterCleanlinessIdentityAndSharedLock(t *testing.T) {
	r := fixture(t)
	target := open(t, r)
	snapshot, err := target.Snapshot()
	if err != nil || snapshot.Dirty {
		t.Fatalf("baseline: %+v %v", snapshot, err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(r.Path, alias); err != nil {
		t.Fatal(err)
	}
	aliasRepo := r
	aliasRepo.Path = alias
	aliasSnapshot, err := open(t, aliasRepo).Snapshot()
	if err != nil || aliasSnapshot.Baseline() != snapshot.Baseline() {
		t.Fatal("alias changed identity")
	}
	clone := filepath.Join(t.TempDir(), "clone")
	git(t, r.Path, "clone", "-q", r.Path, clone)
	cloneRepo := r
	cloneRepo.Path = clone
	cloneSnapshot, err := open(t, cloneRepo).Snapshot()
	if err != nil || cloneSnapshot.Head != snapshot.Head || cloneSnapshot.Baseline() == snapshot.Baseline() {
		t.Fatal("same-HEAD clone did not have distinct identity")
	}
	unlock, err := target.LockWriter()
	if err != nil {
		t.Fatal(err)
	}
	if release, err := open(t, aliasRepo).LockWriter(); err == nil {
		release()
		t.Error("second writer acquired the lock")
	}
	worktree := filepath.Join(t.TempDir(), "worktree")
	git(t, r.Path, "worktree", "add", "--detach", worktree)
	worktreeRepo := r
	worktreeRepo.Path = worktree
	if release, err := open(t, worktreeRepo).LockWriter(); err == nil {
		release()
		t.Error("linked worktree bypassed lock")
	}
	unlock()
	for _, path := range []string{"yanai-server/main.go", "new-untracked.txt"} {
		write(t, r.Path, path, "user work")
		if release, err := target.LockWriter(); err == nil {
			release()
			t.Fatalf("dirty checkout accepted: %s", path)
		}
		data, err := os.ReadFile(filepath.Join(r.Path, path))
		if err != nil || string(data) != "user work" {
			t.Fatal("user work changed")
		}
		if path == "yanai-server/main.go" {
			git(t, r.Path, "add", path)
			git(t, r.Path, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "resolve")
		} else if err := os.Remove(filepath.Join(r.Path, path)); err != nil {
			t.Fatal(err)
		}
	}
	release, err := target.LockWriter()
	if err != nil {
		t.Fatalf("lock leaked on rejection: %v", err)
	}
	release()
}

func TestWriterLockReleasedOnProcessExit(t *testing.T) {
	if path := os.Getenv("YANAI_LOCK_TEST_CHILD"); path != "" {
		target := open(t, config.Repo{Path: path})
		if _, err := target.LockWriter(); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // deliberately bypass unlock/defer, as on process termination
	}
	r := fixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriterLockReleasedOnProcessExit$")
	cmd.Env = append(os.Environ(), "YANAI_LOCK_TEST_CHILD="+r.Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, out)
	}
	release, err := open(t, r).LockWriter()
	if err != nil {
		t.Fatal(err)
	}
	release()
}
