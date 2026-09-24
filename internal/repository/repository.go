// Package repository binds the harness to an operator-selected Git checkout.
// All paths are relative to its Git root, never to the harness workspace.
package repository

import (
	"crypto/sha256"
	"fmt"
	"github.com/yanai/yanai-harness/internal/workflow"
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
		return nil, fmt.Errorf("repo.path is empty; bind it with 'yanai init --repo /path/to/project'")
	}
	root, err := Canonical(r.Path)
	if err != nil {
		return nil, fmt.Errorf("repo.path: %w (use 'yanai init --repo /path/to/project')", err)
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
		t.repo.AllowedPaths = []string{"."}
	}
	for _, p := range t.repo.AllowedPaths {
		if p != "." {
			if err := cleanRelative(p); err != nil {
				return nil, fmt.Errorf("allowed_paths: %w", err)
			}
		}
	}
	if r.ModuleDir != "" {
		if err := t.CheckDirectory(r.ModuleDir); err != nil {
			return nil, err
		}
		mod, err := t.readRegular(filepath.ToSlash(filepath.Join(r.ModuleDir, "go.mod")), 65536)
		if err != nil || moduleName(mod) == "" || moduleName(mod) == "github.com/yanai/yanai-harness" {
			return nil, fmt.Errorf("configured Go module is missing at %s/go.mod", r.ModuleDir)
		}
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

type Snapshot = workflow.RepositoryState

// Branch names the checked-out branch, or "DETACHED@<commit>" when HEAD is
// not on one. It is recorded in the execution contract separately from the
// snapshot because a checkout can be moved onto another branch without its
// HEAD commit or working tree changing at all — the diff would then land
// somewhere the human never approved.
func (t *Target) Branch() (string, error) {
	name, err := t.git("symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			return trimmed, nil
		}
	}
	head, err := t.git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("repository needs a committed baseline: %w", err)
	}
	return "DETACHED@" + strings.TrimSpace(head), nil
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
	index, err := t.git("ls-files", "--stage", "-z")
	if err != nil {
		return Snapshot{}, err
	}
	names, err := t.git("ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Snapshot{}, err
	}
	content := map[string]string{}
	fingerprintTarget := *t
	fingerprintTarget.repo.AllowedPaths = []string{"."}
	fingerprintTarget.repo.ExcludeDirs = nil
	for _, path := range strings.Split(names, "\x00") {
		if path == "" || fingerprintTarget.CheckPath(path) != nil {
			continue
		}
		info, err := os.Lstat(filepath.Join(t.Root, filepath.FromSlash(path)))
		if os.IsNotExist(err) {
			content[path] = "deleted"
			continue
		}
		if err != nil {
			return Snapshot{}, err
		}
		data, err := fingerprintTarget.ReadFile(path, int(info.Size())+1)
		if err != nil {
			return Snapshot{}, err
		}
		if int64(len(data)) != info.Size() {
			return Snapshot{}, fmt.Errorf("file changed during fingerprint: %s", path)
		}
		content[path] = fmt.Sprintf("%o:%x", info.Mode().Perm(), sha256.Sum256(data))
	}
	return Snapshot{Root: t.Root, CommonDir: t.CommonDir, Head: strings.TrimSpace(head), Dirty: status != "", Content: content, IndexHash: fmt.Sprintf("%x", sha256.Sum256([]byte(index))), StatusHash: fmt.Sprintf("%x", sha256.Sum256([]byte(status)))}, nil
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
		if prefix == "." || path == prefix || strings.HasPrefix(path, prefix+"/") {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("path outside allowed backend paths: %q", path)
	}
	for _, part := range strings.Split(path, "/") {
		lower := strings.ToLower(part)
		if strings.HasPrefix(part, ".") || lower == "secrets" || strings.Contains(lower, "credential") || strings.HasPrefix(lower, "secret") || strings.HasPrefix(lower, "id_rsa") || strings.HasPrefix(lower, "id_ed25519") {
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

// CheckDirectory accepts the Git root only when explicitly allowed. File paths
// still cannot name "."; check working directories are a separate capability.
func (t *Target) CheckDirectory(path string) error {
	if path == "." {
		for _, p := range t.repo.AllowedPaths {
			if p == "." {
				return nil
			}
		}
		return fmt.Errorf("repository root is outside allowed paths")
	}
	return t.CheckPath(path)
}
