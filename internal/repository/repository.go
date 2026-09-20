// Package repository binds the harness to the existing Yanai checkout.
// All paths are relative to its Git root, never to the harness workspace.
package repository

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
)

const ModuleDir = "yanai-server"

type Target struct {
	Root      string
	CommonDir string
	repo      config.Repo
}

// Open rejects subdirectories, non-repositories, and the harness itself.
// workspace may be empty for read-only operations without a workspace.
func Open(r config.Repo, workspace string) (*Target, error) {
	if strings.TrimSpace(r.Path) == "" {
		return nil, fmt.Errorf("repo.path is empty; bind it with 'yanai init --repo /path/to/yanai'")
	}
	root, err := Canonical(r.Path)
	if err != nil {
		return nil, fmt.Errorf("repo.path: %w (use 'yanai init --repo /path/to/yanai')", err)
	}
	t := &Target{Root: root, repo: r}
	top, err := t.git("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("repo.path must be a Git checkout: %w", err)
	}
	top, err = Canonical(strings.TrimSpace(top))
	if err != nil || top != root {
		return nil, fmt.Errorf("repo.path must be the Git root, not a subdirectory: %s", root)
	}
	common, err := t.git("rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	common = strings.TrimSpace(common)
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	t.CommonDir, err = Canonical(common)
	if err != nil {
		return nil, err
	}
	if data, err := t.readRegular("go.mod", 65536); err == nil && moduleName(data) == "github.com/yanai/yanai-harness" {
		return nil, fmt.Errorf("application target cannot be the harness repository: %s", root)
	}
	if len(r.AllowedPaths) == 0 {
		t.repo.AllowedPaths = []string{"AGENTS.md", ModuleDir}
	}
	for _, p := range t.repo.AllowedPaths {
		if err := cleanRelative(p); err != nil {
			return nil, fmt.Errorf("allowed_paths: %w", err)
		}
		if p != "AGENTS.md" && p != ModuleDir && !strings.HasPrefix(p, ModuleDir+"/") {
			return nil, fmt.Errorf("allowed_paths cannot include %q: this stage allows only yanai-server and AGENTS.md", p)
		}
	}
	// Validate the module independently of the user-narrowed read allowlist.
	mod, err := t.readRegular(ModuleDir+"/go.mod", 65536)
	if err != nil || moduleName(mod) != "yanai-server" {
		return nil, fmt.Errorf("target must contain the yanai-server Go module at yanai-server/go.mod: %s", root)
	}
	if workspace != "" {
		w, err := Canonical(workspace)
		if err != nil {
			return nil, err
		}
		if contains(root, w) || contains(w, root) {
			return nil, fmt.Errorf("workspace and application repository must not overlap: %s / %s", w, root)
		}
	}
	return t, nil
}

// Canonical also resolves existing ancestors of a not-yet-created workspace.
func Canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", err
	}
	parent, err = Canonical(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func contains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func moduleName(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], "\"")
		}
	}
	return ""
}

// DiscoverSibling recognizes the harness checkout from the invocation directory
// (including its parent) or the binary location. Otherwise --repo is required.
func DiscoverSibling(starts ...string) (string, error) {
	for _, start := range starts {
		abs, err := filepath.Abs(start)
		if err != nil {
			continue
		}
		for dir := abs; ; dir = filepath.Dir(dir) {
			for _, harness := range []string{dir, filepath.Join(dir, "yanai-harness")} {
				data, err := os.ReadFile(filepath.Join(harness, "go.mod"))
				if err == nil && moduleName(data) == "github.com/yanai/yanai-harness" {
					harness, err = Canonical(harness)
					if err != nil {
						return "", err
					}
					return filepath.Join(filepath.Dir(harness), "yanai"), nil
				}
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return "", fmt.Errorf("cannot locate the harness's sibling Yanai checkout; pass --repo /path/to/yanai")
}

type Snapshot struct {
	Root      string
	CommonDir string
	Head      string
	Dirty     bool
}

func (t *Target) Snapshot() (Snapshot, error) {
	head, err := t.git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Snapshot{}, fmt.Errorf("repository needs a committed baseline: %w", err)
	}
	status, err := t.git("status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Root: t.Root, CommonDir: t.CommonDir, Head: strings.TrimSpace(head), Dirty: status != ""}, nil
}

// Baseline deliberately differs between checkouts with the same HEAD. Dirty
// snapshots are labelled for planning only; they can never authorize execution.
func (s Snapshot) Baseline() string {
	data, _ := json.Marshal(s)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s Snapshot) Label() string {
	state := "clean"
	if s.Dirty {
		state = "DIRTY — planning only; run requires a clean checkout and a new plan"
	}
	return fmt.Sprintf("Repository: %s\nModule: %s\nHEAD: %s\nWorking tree: %s\n", s.Root, ModuleDir, s.Head, state)
}

func cleanRelative(path string) error {
	if path == "" || path == "." || filepath.IsAbs(path) || strings.ContainsAny(path, "\\\x00\r\n") || filepath.ToSlash(filepath.Clean(path)) != path {
		return fmt.Errorf("invalid repository-relative path %q", path)
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return fmt.Errorf("path traversal is forbidden: %q", path)
		}
	}
	return nil
}

// CheckPath is shared by the index, explicit reads and generated output paths.
// Configuration can narrow the backend boundary, never widen it into the UI.
func (t *Target) CheckPath(path string) error {
	if err := cleanRelative(path); err != nil {
		return err
	}
	allowed := false
	for _, prefix := range t.repo.AllowedPaths {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("path outside allowed backend paths: %q", path)
	}
	for _, part := range strings.Split(path, "/") {
		lower := strings.ToLower(part)
		if strings.HasPrefix(part, ".") || lower == "yanai-ui" || lower == "secrets" || strings.Contains(lower, "credential") || strings.HasPrefix(lower, "secret") || strings.HasPrefix(lower, "id_rsa") || strings.HasPrefix(lower, "id_ed25519") {
			return fmt.Errorf("protected path: %q", path)
		}
		for _, excluded := range t.repo.ExcludeDirs {
			if part == excluded {
				return fmt.Errorf("excluded path: %q", path)
			}
		}
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".env", ".pem", ".key", ".p12", ".pfx", ".sqlite", ".db":
		return fmt.Errorf("protected file type: %q", path)
	}
	if _, err := t.git("check-ignore", "--quiet", "--no-index", "--", path); err != nil {
		if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 1 {
			return fmt.Errorf("check ignored path: %w", err)
		}
	} else {
		return fmt.Errorf("ignored path: %q", path)
	}
	return t.noSymlinks(path)
}

func (t *Target) noSymlinks(path string) error {
	current := t.Root
	for _, part := range strings.Split(path, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		} // permitted for a future output
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not allowed in repository paths: %q", path)
		}
	}
	return nil
}

func (t *Target) ReadFile(path string, limit int) ([]byte, error) {
	if err := t.CheckPath(path); err != nil {
		return nil, err
	}
	return t.readRegular(path, limit)
}

func (t *Target) readRegular(path string, limit int) ([]byte, error) {
	if err := t.noSymlinks(path); err != nil {
		return nil, err
	}
	before, err := os.Lstat(filepath.Join(t.Root, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %q", path)
	}
	root, err := os.OpenRoot(t.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("file changed during read: %q", path)
	}
	return io.ReadAll(io.LimitReader(f, int64(limit)))
}

func (t *Target) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", t.Root}, args...)...)
	// Inherited GIT_DIR/WORK_TREE/INDEX_FILE must not redirect repository checks.
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	return string(out), err
}
