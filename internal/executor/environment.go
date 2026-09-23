package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yanai/yanai-harness/internal/workflow"
)

// Environment variables an operator uses to point the executor at the Go
// toolchain and the disposable database its checks require. They are operator
// inputs, read once by the CLI; no model output ever reaches them.
const (
	GoBinaryEnv    = "YANAI_GO_BINARY"
	GoCacheEnv     = "YANAI_GO_CACHE"
	ModuleCacheEnv = "YANAI_GO_MODCACHE"
	// AdminURLEnv is retained for configured Go projects' database tests,
	// so the harness supplies checks with exactly the variable those tests
	// gate on. Without it the database tests skip, and a skipped database test
	// can never count as acceptance — Check refuses to run at all.
	AdminURLEnv = "YANAI_TEST_ADMIN_URL"
)

// Defaults resolves optional Go compatibility settings only when approved checks need Go,
// from the operator's environment. It never inherits the caller's whole
// environment into the check: checkEnvironment builds a fresh one from exactly
// these values. A missing cache is resolved from `go env` rather than guessed,
// and a relative or in-checkout cache is refused there.
func Defaults(o Options) (Options, error) {
	if o.Store == nil {
		return o, errors.New("toolchain defaults require an approved contract")
	}
	contract, err := o.Store.Contract(o.Cycle, o.Contract)
	if err != nil {
		return o, err
	}
	needsGo := false
	for _, c := range contract.Policy.Checks {
		if len(c.Args) > 0 && c.Args[0] == "go" {
			needsGo = true
		}
	}
	o.AdminURL = strings.TrimSpace(os.Getenv(AdminURLEnv))
	if o.AdminURL != "" {
		if err := localDatabase(o.AdminURL); err != nil {
			return o, err
		}
	}
	if !needsGo {
		return o, nil
	}
	o.GoBinary = strings.TrimSpace(os.Getenv(GoBinaryEnv))
	if o.GoBinary == "" {
		path, err := exec.LookPath("go")
		if err != nil {
			return o, fmt.Errorf("approved checks need the Go toolchain on PATH, or %s: %w", GoBinaryEnv, err)
		}
		o.GoBinary = path
	}
	if o.GoCache, err = goEnv(o.GoBinary, GoCacheEnv, "GOCACHE"); err != nil {
		return o, err
	}
	if o.ModuleCache, err = goEnv(o.GoBinary, ModuleCacheEnv, "GOMODCACHE"); err != nil {
		return o, err
	}
	return o, nil
}

func goEnv(binary, override, name string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(override)); value != "" {
		return value, nil
	}
	out, err := exec.Command(binary, "env", name).Output()
	if err != nil {
		return "", fmt.Errorf("could not resolve %s from the Go toolchain: %w", name, err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", errors.New("the Go toolchain reported an empty " + name + "; set " + override)
	}
	return value, nil
}

func (n *Native) resolveCheckTool(name string, tool workflow.CheckTool) (string, error) {
	if err := workflow.ValidateCheckTool(name, tool); err != nil {
		return "", err
	}
	binary, err := filepath.EvalSymlinks(tool.Binary)
	if err != nil {
		return "", err
	}
	if workflow.ForbiddenCheckProgram(binary) {
		return "", errors.New("resolved check executable is forbidden")
	}
	info, err := os.Stat(binary)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("check tool must be an executable regular file")
	}
	// Resolve directory aliases too (e.g. macOS /var -> /private/var),
	// without following the executable symlink out of a writable workspace.
	invocationDir, err := filepath.EvalSymlinks(filepath.Dir(tool.Binary))
	if err != nil {
		return "", err
	}
	invocation := filepath.Join(invocationDir, filepath.Base(tool.Binary))
	for _, root := range []string{n.target.Root, n.o.Artifacts.Root} {
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", err
		}
		for _, path := range []string{binary, invocation} {
			rel, err := filepath.Rel(canonical, path)
			if err != nil {
				return "", err
			}
			if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", errors.New("check tool must be installed outside the target and workspace")
			}
		}
	}
	// Preserve the invocation path: Python virtual environments and other
	// installations derive runtime configuration from their executable location.
	return tool.Binary, nil
}
func (n *Native) checkEnvironment(c workflow.Check, scratch string) ([]string, string, error) {
	if c.Args[0] == "go" {
		return n.goCheckEnvironment(scratch)
	}
	binary, err := n.resolveCheckTool(c.Args[0], n.contract.Policy.Tools[c.Args[0]])
	if err != nil {
		return nil, "", err
	}
	dirs := map[string]bool{filepath.Dir(binary): true}
	for name, tool := range n.contract.Policy.Tools {
		path, err := n.resolveCheckTool(name, tool)
		if err != nil {
			return nil, "", err
		}
		dirs[filepath.Dir(path)] = true
	}
	paths := make([]string, 0, len(dirs))
	for dir := range dirs {
		paths = append(paths, dir)
	}
	sort.Strings(paths)
	paths = append(paths, "/usr/bin", "/bin")
	env := []string{"PATH=" + strings.Join(paths, string(os.PathListSeparator)), "HOME=" + scratch, "TMPDIR=" + scratch, "LANG=C", "TZ=UTC"}
	if c.RequiresPostgres {
		if err := localDatabase(n.o.AdminURL); err != nil {
			return nil, "", err
		}
		env = append(env, "YANAI_TEST_ADMIN_URL="+n.o.AdminURL)
	}
	return env, binary, nil
}
