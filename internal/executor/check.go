package executor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yanai/yanai-harness/internal/workflow"
)

const maxCheckOutput = 1 << 20

var packageArg = regexp.MustCompile(`^\./[A-Za-z0-9_./-]+$`)

func validateCheck(c workflow.Check, allowlists ...map[string]workflow.CheckTool) error {
	if err := workflow.ValidateCheckEvidence(c); err != nil {
		return err
	}
	if len(c.Args) > 0 && c.Args[0] != "go" {
		if c.TimeoutSeconds <= 0 || c.TimeoutSeconds > 3600 || c.Dir == "" || filepath.IsAbs(c.Dir) || filepath.Clean(c.Dir) != c.Dir || c.Dir == ".." || strings.HasPrefix(c.Dir, "../") || strings.ContainsAny(c.Dir, "\\\x00\r\n") {
			return errors.New("invalid check bounds")
		}
		if err := workflow.ValidateCheckArguments(c.Args); err != nil {
			return err
		}
		if len(allowlists) == 0 {
			return errors.New("check executable is not allowlisted")
		}
		tool, ok := allowlists[0][c.Args[0]]
		if !ok {
			return errors.New("check executable is not allowlisted")
		}
		return workflow.ValidateCheckTool(c.Args[0], tool)
	}

	if len(c.Args) < 3 || c.Args[0] != "go" || c.TimeoutSeconds <= 0 || c.TimeoutSeconds > 3600 || (c.Dir == "" || filepath.IsAbs(c.Dir) || filepath.ToSlash(filepath.Clean(c.Dir)) != c.Dir || c.Dir == ".." || strings.HasPrefix(c.Dir, "../") || strings.ContainsAny(c.Dir, "\\\x00\r\n")) {
		return fmt.Errorf("unsupported check %q", c.ID)
	}
	if c.Args[1] != "test" && c.Args[1] != "vet" && c.Args[1] != "build" {
		return errors.New("only go test, go vet and go build are allowed")
	}
	packages := 0
	for _, a := range c.Args[2:] {
		switch a {
		case "-race":
		case "-v", "-count=1", "-shuffle=on":
			if c.Args[1] != "test" {
				return errors.New("test flag in non-test check")
			}
		default:
			if a != "." && a != "./..." && (!packageArg.MatchString(a) || strings.Contains(a, "..") || strings.HasSuffix(a, ".go")) {
				return fmt.Errorf("unsupported check argument %q", a)
			}
			packages++
		}
	}
	if packages == 0 {
		return errors.New("check needs explicit local packages")
	}
	return nil
}

type CheckResult struct {
	Contract                             string
	ID, CheckID, Binary, Dir             string
	Args                                 []string
	Before, After                        string
	StartedAt, FinishedAt                time.Time
	ExitCode                             int
	Output, Error                        string
	TimedOut, Truncated, DatabaseEnabled bool
	Evidence                             workflow.ArtifactRef
}
type boundedOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	size := len(p)
	room := maxCheckOutput - len(b.data)
	if len(p) > room {
		p = p[:room]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return size, nil
}
func localDatabase(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid disposable PostgreSQL URL")
	}
	if (u.Scheme != "postgres" && u.Scheme != "postgresql") || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || u.Port() == "" || u.Path == "" {
		return errors.New("test checks require a disposable PostgreSQL URL with an explicit loopback host and port")
	}
	// Connection-option overrides could redirect libpq/pgx to another host.
	for key := range u.Query() {
		if key != "sslmode" {
			return errors.New("unsupported PostgreSQL connection option")
		}
	}
	return nil
}
func (n *Native) goCheckEnvironment(c workflow.Check, scratch string) ([]string, string, error) {
	binary := n.o.GoBinary
	if binary == "" {
		var err error
		binary, err = exec.LookPath("go")
		if err != nil {
			return nil, "", err
		}
	}
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return nil, "", err
	}
	if !filepath.IsAbs(binary) {
		return nil, "", errors.New("Go binary must resolve absolutely")
	}
	for _, path := range []string{n.o.GoCache, n.o.ModuleCache} {
		if !filepath.IsAbs(path) {
			return nil, "", errors.New("absolute prepopulated Go caches required")
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, "", err
		}
		rel, err := filepath.Rel(n.target.Root, canonical)
		if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return nil, "", errors.New("Go cache cannot be in application checkout")
		}
	}
	env := []string{
		"PATH=" + filepath.Dir(binary) + ":/usr/bin:/bin",
		"HOME=" + scratch, "TMPDIR=" + scratch, "GOTMPDIR=" + scratch,
		"GOCACHE=" + n.o.GoCache, "GOMODCACHE=" + n.o.ModuleCache, "GOPATH=" + scratch,
		"GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off",
		"GOVCS=*:off", "GOTELEMETRY=off", "CGO_ENABLED=1", "LANG=C", "TZ=UTC",
	}
	inputs, err := n.checkInputs(c)
	if err != nil {
		return nil, "", err
	}
	env = append(env, inputs...)
	return env, binary, nil
}

func (n *Native) Check(ctx context.Context, id string) (CheckResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.unfinishedChecks(); err != nil {
		return CheckResult{}, err
	}
	before, err := n.guard()
	if err != nil {
		return CheckResult{}, err
	}
	var c *workflow.Check
	for i := range n.contract.Policy.Checks {
		if n.contract.Policy.Checks[i].ID == id {
			c = &n.contract.Policy.Checks[i]
		}
	}
	if c == nil {
		return CheckResult{}, errors.New("check ID is not approved")
	}
	if err = validateCheck(*c, n.contract.Policy.Tools); err != nil {
		return CheckResult{}, err
	}
	if err = n.target.CheckDirectory(c.Dir); err != nil {
		return CheckResult{}, err
	}
	if _, err = n.checkInputs(*c); err != nil {
		return CheckResult{}, err
	}
	scratch, err := os.MkdirTemp(n.o.Artifacts.Root, ".check-")
	if err != nil {
		return CheckResult{}, err
	}
	defer os.RemoveAll(scratch)
	env, binary, err := n.checkEnvironment(*c, scratch)
	if err != nil {
		return CheckResult{}, err
	}
	args := append([]string(nil), c.Args[1:]...)
	if c.Args[0] == "go" {
		args = []string{c.Args[1], "-mod=readonly", "-buildvcs=false"}
		if c.Args[1] == "build" {
			args = append(args, "-o", os.DevNull)
		}
		args = append(args, c.Args[2:]...)
	}
	result := CheckResult{ID: "check-" + nonce(), CheckID: id, Contract: n.o.Contract, Binary: binary, Dir: filepath.Join(n.target.Root, c.Dir), Args: args, Before: before.Baseline(), ExitCode: -1, StartedAt: time.Now().UTC(), DatabaseEnabled: c.PostgresURLVar != ""}
	// A start without a result is explicitly interrupted/unknown, never success.
	if _, err = n.publish(result.ID+"-started", result); err != nil {
		return result, err
	}
	duration := time.Duration(c.TimeoutSeconds) * time.Second
	if active := time.Duration(n.contract.Policy.MaxActiveSeconds) * time.Second; duration > active {
		duration = active
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = result.Dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	result.FinishedAt = time.Now().UTC()
	result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	result.Truncated = output.truncated
	result.Output = string(output.data)
	for _, value := range n.o.CheckInputs {
		if value != "" {
			result.Output = strings.ReplaceAll(result.Output, value, "[check input]")
		}
	}
	// A check is code execution. Detect unexpected repository writes; preserve
	// them for inspection rather than deleting arbitrary subprocess output.
	after, stateErr := n.guard()
	result.After = after.Baseline()
	if result.Truncated {
		err = errors.Join(err, errors.New("check output exceeded evidence limit"))
	}
	if stateErr != nil {
		err = errors.Join(err, stateErr)
	}
	if failure := workflow.CheckOutputFailure(*c, result.Output); failure != "" {
		err = errors.Join(err, errors.New(failure))
	}
	if err != nil {
		result.Error = err.Error()
	}
	ref, publishErr := n.publish(result.ID+"-finished", result)
	result.Evidence = ref
	return result, errors.Join(err, publishErr)
}

// A killed check may have left a live subprocess. Never automatically rerun it
// or start another mutation just because no result reached the artifact store.
// Operator reconciliation belongs to the future orchestration/CLI increment.
func (n *Native) unfinishedChecks() error {
	ids, err := n.o.Store.ArtifactIDs(n.o.Cycle, fmt.Sprintf("executor/%d/", n.o.Cycle))
	if err != nil {
		return err
	}
	available := map[string]bool{}
	for _, id := range ids {
		available[id] = true
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "check-") || !strings.HasSuffix(id, "-started") {
			continue
		}
		end := strings.TrimSuffix(id, "-started") + "-finished"
		if !available[end] {
			return fmt.Errorf("unfinished check %s: inspect surviving processes and reconcile evidence before further execution", id)
		}
		if _, err = n.o.Artifacts.Read(n.o.Store, n.o.Cycle, end); err != nil {
			return err
		}
	}
	return nil
}
