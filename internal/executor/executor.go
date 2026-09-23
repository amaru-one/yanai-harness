// Package executor provides the native repository tools. It deliberately does
// not drive the team loop or advance candidate_ready to implemented/verified.
package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
)

const MaxSourceBytes = 2 << 20

// File is a complete source snapshot. A nil *File means absence; empty Data
// means a present empty file. Modes are permissions, not Git's type bits.
type File struct {
	Data []byte
	Mode uint32
}
type Edit struct {
	Path          string
	Before, After *File
}
type Match struct {
	Path string
	Line int
	Text string
}
type Diff struct {
	Before, After string
	Edits         []Edit
}
type Receipt struct {
	Patch         workflow.ArtifactRef
	Before, After string
}

// Backend is the seam for a future backend. Commands are selected by approved
// check ID, never supplied by a model. Close releases the repository writer.
type Backend interface {
	Read(string) (*File, error)
	Search([]string, string) ([]Match, error)
	Apply(string, []Edit) (Receipt, error)
	Inspect() (Diff, error)
	Check(context.Context, string) (CheckResult, error)
	Close() error
}

// Options are trusted operator inputs, never model output. The evidence root
// must be the workspace's durable artifact directory, outside the repository.
// AdminURL is only for a disposable local PostgreSQL cluster. GoCache and
// ModuleCache are prepopulated operator-owned caches; downloads are disabled.
type Options struct {
	Repo                                     config.Repo
	Store                                    *workflow.Store
	Artifacts                                workflow.ArtifactStore
	Cycle                                    int
	Contract                                 string
	GoBinary, GoCache, ModuleCache, AdminURL string
}

type Native struct {
	mu       sync.Mutex
	o        Options
	target   *repository.Target
	root     *os.Root
	contract workflow.ExecutionContract
	base     workflow.RepositoryState
	unlock   func()
	closed   bool
	// Fault injection is package-private and used to exercise disk/DB boundaries.
	fault func(string) error
}

var _ Backend = (*Native)(nil)

func Open(o Options) (*Native, error) {
	if o.Store == nil || o.Artifacts.Root == "" {
		return nil, errors.New("durable store and artifact root required")
	}
	t, err := repository.Open(o.Repo, o.Artifacts.Root)
	if err != nil {
		return nil, err
	}
	unlock, err := t.LockForRecovery()
	if err != nil {
		return nil, fmt.Errorf("repository already has an active writer (or locking failed): %w", err)
	}
	n := &Native{o: o, target: t, unlock: unlock}
	ok := false
	defer func() {
		if !ok {
			n.Close()
		}
	}()
	n.root, err = os.OpenRoot(t.Root)
	if err != nil {
		return nil, err
	}
	n.contract, err = o.Store.Contract(o.Cycle, o.Contract)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(n.contract.Repository, &n.base); err != nil {
		return nil, err
	}
	branch, err := t.Branch()
	if err != nil {
		return nil, err
	}
	live, err := t.Snapshot()
	if err != nil {
		return nil, err
	}
	// The contract revision, backend, candidate format and checkout identity
	// are validated before recovery reads any journal and before any write.
	if err = n.contract.ValidateExecutable(live, branch); err != nil {
		return nil, err
	}
	if n.base.Root != t.Root || n.base.CommonDir != t.CommonDir || n.base.Head != live.Head || n.base.Dirty {
		return nil, errors.New("executor requires the approved clean checkout")
	}
	for _, check := range n.contract.Policy.Checks {
		if err = validateCheck(check, n.contract.Policy.Tools); err != nil {
			return nil, err
		}
	}
	for name, tool := range n.contract.Policy.Tools {
		if _, err = n.resolveCheckTool(name, tool); err != nil {
			return nil, err
		}
	}
	if err = n.recover(); err != nil {
		return nil, err
	}
	if _, err = n.guard(); err != nil {
		return nil, err
	}
	ok = true
	return n, nil
}

func (n *Native) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	var err error
	if n.root != nil {
		err = n.root.Close()
	}
	if n.unlock != nil {
		n.unlock()
	}
	return err
}
func (n *Native) guard() (workflow.RepositoryState, error) {
	if n.closed {
		return workflow.RepositoryState{}, errors.New("executor closed")
	}
	if err := n.o.Store.HasApproval(n.o.Cycle, n.o.Contract); err != nil {
		return workflow.RepositoryState{}, err
	}
	expected, _, err := n.o.Store.PatchState(n.o.Cycle, n.o.Contract)
	if err != nil {
		return expected, err
	}
	actual, err := n.target.Snapshot()
	if err != nil {
		return actual, err
	}
	if actual.Baseline() != expected.Baseline() {
		// One message for one condition: the checkout is not what the engine
		// recorded. Whether that is a human's uncommitted work, a build
		// artifact or a check's stray write, the answer is the same — nothing
		// is reverted or deleted here, and execution stops until a person has
		// looked at it.
		return actual, errors.New("unexpected repository state: the checkout is neither the approved clean checkout nor the exact state the engine recorded; preserve external edits and reconcile")
	}
	return actual, nil
}

// Contract exposes the approved terms the executor validated. Callers read it
// to build execution context; nothing here is a route to changing it.
func (n *Native) Contract() workflow.ExecutionContract { return n.contract }

// Base is the approved clean baseline every diff is measured against.
func (n *Native) Base() workflow.RepositoryState { return n.base }

// State returns the current repository snapshot, after re-checking approval
// and that the checkout still matches exactly what the engine recorded.
func (n *Native) State() (workflow.RepositoryState, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.guard()
}
func fingerprint(f *File) string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%o:%x", f.Mode, sha256.Sum256(f.Data))
}
func same(a, b *File) bool { return fingerprint(a) == fingerprint(b) }
func (n *Native) read(path string) (*File, error) {
	if err := n.target.CheckPath(path); err != nil {
		return nil, err
	}
	info, err := n.root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxSourceBytes {
		return nil, fmt.Errorf("source must be a complete regular file of at most %d bytes: %s", MaxSourceBytes, path)
	}
	data, err := n.target.ReadFile(path, MaxSourceBytes+1)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != info.Size() {
		return nil, errors.New("source changed during read")
	}
	return &File{Data: data, Mode: uint32(info.Mode().Perm())}, nil
}
func (n *Native) Read(path string) (*File, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.guard(); err != nil {
		return nil, err
	}
	return n.read(path)
}
func (n *Native) Search(paths []string, literal string) ([]Match, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.guard(); err != nil {
		return nil, err
	}
	if len(paths) > 256 || literal == "" {
		return nil, errors.New("search requires a literal and at most 256 explicit paths")
	}
	var out []Match
	for _, path := range paths {
		f, err := n.read(path)
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue
		}
		for line, text := range strings.Split(string(f.Data), "\n") {
			if strings.Contains(text, literal) {
				out = append(out, Match{path, line + 1, text})
				if len(out) > 1000 {
					return nil, errors.New("search result limit exceeded; narrow paths or literal")
				}
			}
		}
	}
	return out, nil
}
func (n *Native) hit(stage string) error {
	if n.fault != nil {
		return n.fault(stage)
	}
	return nil
}
func nonce() string { return fmt.Sprintf("%x", rand.Text()) }
func (n *Native) publish(id string, v any) (workflow.ArtifactRef, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	ref, err := n.o.Artifacts.Publish(n.o.Store, n.o.Cycle, workflow.ArtifactRef{ID: id, Path: fmt.Sprintf("executor/%d/%s.json", n.o.Cycle, id), Version: "1", Media: "application/json"}, b)
	if err != nil {
		return ref, err
	}
	// The generic publisher fsyncs contents. Before repository mutation also
	// make its directory entries durable and verify an existing publication.
	if _, err = n.o.Artifacts.Read(n.o.Store, n.o.Cycle, id); err != nil {
		return ref, err
	}
	root, err := os.OpenRoot(n.o.Artifacts.Root)
	if err != nil {
		return ref, err
	}
	defer root.Close()
	for dir := filepath.Dir(ref.Path); ; dir = filepath.Dir(dir) {
		parent, err := root.OpenRoot(dir)
		if err != nil {
			return ref, err
		}
		err = syncDir(parent)
		parent.Close()
		if err != nil {
			return ref, err
		}
		if dir == "." {
			break
		}
	}
	return ref, nil
}

// predictedStatus matches Git's unstaged regular-file porcelain v1 records:
// tracked changes first, untracked second, byte sorted within each group.
// Snapshot's broad backend fingerprint, NOT the configured read subset, is
// the source of both maps. The index and HEAD must stay at the clean baseline.
func predictedStatus(base, next map[string]string) []byte {
	var tracked, untracked []string
	for path, before := range base {
		if next[path] != before {
			tracked = append(tracked, path)
		}
	}
	for path := range next {
		if _, ok := base[path]; !ok {
			untracked = append(untracked, path)
		}
	}
	sort.Strings(tracked)
	sort.Strings(untracked)
	var b strings.Builder
	for _, path := range tracked {
		if next[path] == "deleted" {
			b.WriteString(" D ")
		} else {
			b.WriteString(" M ")
		}
		b.WriteString(path)
		b.WriteByte(0)
	}
	for _, path := range untracked {
		b.WriteString("?? ")
		b.WriteString(path)
		b.WriteByte(0)
	}
	return []byte(b.String())
}
func (n *Native) predict(before workflow.RepositoryState, edits []Edit) workflow.RepositoryState {
	next := before
	next.Content = make(map[string]string, len(before.Content))
	for k, v := range before.Content {
		next.Content[k] = v
	}
	for _, e := range edits {
		if e.After != nil {
			next.Content[e.Path] = fingerprint(e.After)
		} else if _, tracked := n.base.Content[e.Path]; tracked {
			next.Content[e.Path] = "deleted"
		} else {
			delete(next.Content, e.Path)
		}
	}
	status := predictedStatus(n.base.Content, next.Content)
	next.Dirty = len(status) != 0
	next.StatusHash = fmt.Sprintf("%x", sha256.Sum256(status))
	return next
}
