package executor

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/yanai/yanai-harness/internal/workflow"
)

type journal struct {
	Ticket, Temp  string
	Before, After workflow.RepositoryState
	Edits         []Edit
	Dirs          []string // directories absent before this batch; only removed if empty
}

func patchID(after workflow.RepositoryState, revision int64) string {
	return fmt.Sprintf("patch-%s-%d", after.Baseline(), revision)
}

func (n *Native) Apply(ticket string, edits []Edit) (Receipt, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.unfinishedChecks(); err != nil {
		return Receipt{}, err
	}
	before, err := n.guard()
	if err != nil {
		return Receipt{}, err
	}
	_, revision, err := n.o.Store.PatchState(n.o.Cycle, n.o.Contract)
	if err != nil {
		return Receipt{}, err
	}
	// Own copies: the caller cannot change the recorded bytes during application.
	raw, err := json.Marshal(edits)
	if err != nil {
		return Receipt{}, err
	}
	var owned []Edit
	if err = json.Unmarshal(raw, &owned); err != nil {
		return Receipt{}, err
	}
	edits = owned
	if err = n.validateEdits(ticket, edits); err != nil {
		return Receipt{}, err
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].Path < edits[j].Path })
	j := journal{Ticket: ticket, Temp: ".yanai-executor-" + nonce(), Before: before, After: n.predict(before, edits), Edits: edits}
	dirs := map[string]bool{}
	for _, e := range edits {
		for dir := filepath.Dir(e.Path); dir != "."; dir = filepath.Dir(dir) {
			_, err := n.root.Lstat(dir)
			if errors.Is(err, os.ErrNotExist) {
				dirs[dir] = true
			} else if err != nil {
				return Receipt{}, err
			}
		}
	}
	for dir := range dirs {
		j.Dirs = append(j.Dirs, dir)
	}
	sort.Strings(j.Dirs)
	// A process may have published the journal and died before PreparePatch.
	// Reuse its exact identity instead of colliding with our fresh temp nonce.
	id := patchID(j.After, revision)
	if record, lookupErr := n.o.Store.GetArtifact(n.o.Cycle, id); lookupErr == nil {
		if record.State == "pending" {
			// The generic artifact reconciler must finish/drop an interrupted
			// publication before the executor can trust it.
			return Receipt{}, errors.New("pending source artifact; reconcile artifact publications first")
		}
		data, readErr := n.o.Artifacts.Read(n.o.Store, n.o.Cycle, id)
		if readErr != nil {
			return Receipt{}, readErr
		}
		var prior journal
		if err = json.Unmarshal(data, &prior); err != nil {
			return Receipt{}, err
		}
		if prior.Ticket != j.Ticket || prior.Before.Baseline() != j.Before.Baseline() || prior.After.Baseline() != j.After.Baseline() || !reflect.DeepEqual(prior.Edits, j.Edits) || !reflect.DeepEqual(prior.Dirs, j.Dirs) {
			return Receipt{}, errors.New("source journal identity conflict")
		}
		j = prior
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return Receipt{}, lookupErr
	}
	ref, err := n.publish(id, j)
	if err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{Patch: ref, Before: before.Baseline(), After: j.After.Baseline()}
	if err = n.hit("published"); err != nil {
		return receipt, err
	}
	if _, err = n.guard(); err != nil {
		return receipt, err
	}
	if err = n.o.Store.PreparePatch(n.o.Cycle, n.o.Contract, ticket, j.After, revision); err != nil {
		return receipt, err
	}
	if err = n.hit("prepared"); err != nil {
		return receipt, n.abort(j, err)
	}
	for _, dir := range j.Dirs {
		parent, err := n.parent(dir)
		if err != nil {
			return receipt, n.abort(j, err)
		}
		err = parent.Mkdir(filepath.Base(dir), 0o755)
		if err == nil {
			err = syncDir(parent)
		}
		parent.Close()
		if err != nil {
			return receipt, n.abort(j, err)
		}
	}
	for i, e := range edits {
		if err = n.replace(e.Path, e.Before, e.After, tempName(j, i)); err != nil {
			return receipt, n.abort(j, err)
		}
		if err = n.hit(fmt.Sprintf("write:%d", i)); err != nil {
			return receipt, n.abort(j, err)
		}
	}
	actual, err := n.target.Snapshot()
	if err != nil {
		return receipt, n.abort(j, err)
	}
	if actual.Baseline() != j.After.Baseline() {
		return receipt, n.abort(j, errors.New("post-write fingerprint mismatch"))
	}
	if err = n.hit("observed"); err != nil {
		return receipt, n.abort(j, err)
	}
	// A storage failure here leaves durable pending intent; reopening observes
	// the exact post-state and confirms it, without replaying any writes.
	return receipt, n.o.Store.ReconcilePatch(n.o.Cycle, n.o.Contract, actual)
}

func (n *Native) validateEdits(ticket string, edits []Edit) error {
	if len(edits) == 0 || len(edits) > 128 {
		return errors.New("patch requires 1..128 complete edits; no-change needs a separate outcome")
	}
	var approved *workflow.Ticket
	for i := range n.contract.Plan.Tickets {
		if n.contract.Plan.Tickets[i].ID == ticket {
			approved = &n.contract.Plan.Tickets[i]
		}
	}
	if approved == nil {
		return errors.New("unknown approved ticket")
	}
	seen := map[string]bool{}
	size := 0
	for _, e := range edits {
		if seen[e.Path] {
			return errors.New("duplicate edit path")
		}
		seen[e.Path] = true
		if err := n.target.CheckPath(e.Path); err != nil {
			return err
		}
		output, within := false, false
		for _, p := range approved.Outputs {
			if p == e.Path {
				output = true
			}
		}
		for _, p := range approved.AllowedPaths {
			if p == e.Path || strings.HasPrefix(e.Path, p+"/") {
				within = true
			}
		}
		if !output || !within {
			return fmt.Errorf("undeclared output: %s", e.Path)
		}
		for _, f := range []*File{e.Before, e.After} {
			if f != nil {
				size += len(f.Data)
				if len(f.Data) > MaxSourceBytes || (f.Mode != 0o644 && f.Mode != 0o755) {
					return errors.New("unsupported file size or mode")
				}
			}
		}
		actual, err := n.read(e.Path)
		if err != nil {
			return err
		}
		if !same(actual, e.Before) {
			return fmt.Errorf("full source precondition failed: %s", e.Path)
		}
		if same(e.Before, e.After) {
			return fmt.Errorf("empty edit: %s", e.Path)
		}
	}
	if size > 16<<20 {
		return errors.New("patch exceeds source byte limit")
	}
	// File/directory replacement batches are deliberately unsupported.
	for a := range seen {
		for b := range seen {
			if strings.HasPrefix(a, b+"/") {
				return errors.New("overlapping output paths")
			}
		}
	}
	return nil
}

// parent pins every directory after rejecting symlinks (including internal
// redirects). All temp creation and rename operations use that pinned parent.
func (n *Native) parent(path string) (*os.Root, error) {
	root, err := n.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if dir == "." {
		return root, nil
	}
	for _, part := range strings.Split(dir, "/") {
		info, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, errors.New("non-directory or symlink parent")
		}
		next, err := root.OpenRoot(part)
		root.Close()
		if err != nil {
			return nil, err
		}
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			next.Close()
			return nil, errors.New("directory changed while opening")
		}
		root = next
	}
	return root, nil
}
func syncDir(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func tempName(j journal, i int) string { return fmt.Sprintf("%s-%d", j.Temp, i) }

func (n *Native) replace(path string, expected, next *File, temp string) error {
	current, err := n.read(path)
	if err != nil {
		return err
	}
	if !same(current, expected) {
		return fmt.Errorf("external edit at %s; refusing overwrite", path)
	}
	p, err := n.parent(path)
	if err != nil {
		return err
	}
	defer p.Close()
	name := filepath.Base(path)
	if next == nil {
		if err = p.Remove(name); err != nil {
			return err
		}
		return syncDir(p)
	}
	f, err := p.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// Temp names are recorded before they are created. Even a killed writer's
	// partial temp is recoverable; destination bytes change only after fsync.
	if _, err = f.Write(next.Data); err == nil {
		err = f.Chmod(os.FileMode(next.Mode))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = n.hit("staged"); err != nil {
		return err
	}
	current, err = n.read(path)
	if err != nil {
		return err
	}
	if !same(current, expected) {
		return fmt.Errorf("external edit before rename: %s", path)
	}
	if err = p.Rename(temp, name); err != nil {
		return err
	}
	return syncDir(p)
}
func (n *Native) cleanupTemps(j journal) error {
	for i, e := range j.Edits {
		p, err := n.parent(e.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		name := tempName(j, i)
		info, err := p.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			p.Close()
			continue
		}
		if err == nil && !info.Mode().IsRegular() {
			err = errors.New("unexpected nonregular patch temp")
		}
		if err == nil {
			err = p.Remove(name)
		}
		if err == nil {
			err = syncDir(p)
		}
		p.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func (n *Native) rollback(j journal) error {
	if err := n.cleanupTemps(j); err != nil {
		return err
	}
	// Inspect ALL destinations before undoing any: no automatic overwrite of
	// a third state, even when an earlier file in the batch is still ours.
	for _, e := range j.Edits {
		f, err := n.read(e.Path)
		if err != nil {
			return err
		}
		if !same(f, e.Before) && !same(f, e.After) {
			return fmt.Errorf("external edit at %s; rollback blocked", e.Path)
		}
	}
	for i := len(j.Edits) - 1; i >= 0; i-- {
		e := j.Edits[i]
		f, err := n.read(e.Path)
		if err != nil {
			return err
		}
		if !same(f, e.Before) {
			if err = n.replace(e.Path, e.After, e.Before, tempName(j, i)); err != nil {
				return err
			}
		}
	}
	for i := len(j.Dirs) - 1; i >= 0; i-- {
		// Never RemoveAll: a concurrent unrelated file must survive.
		p, err := n.parent(j.Dirs[i])
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		err = p.Remove(filepath.Base(j.Dirs[i]))
		if err == nil {
			err = syncDir(p)
		}
		p.Close()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	actual, err := n.target.Snapshot()
	if err != nil {
		return err
	}
	if actual.Baseline() != j.Before.Baseline() {
		return errors.New("patch rolled back but external repository changes remain; reconciliation blocked")
	}
	return n.o.Store.ReconcilePatch(n.o.Cycle, n.o.Contract, actual)
}
func (n *Native) abort(j journal, cause error) error {
	// Disable synthetic faults during rollback, not real I/O failures.
	fault := n.fault
	n.fault = nil
	defer func() { n.fault = fault }()
	return errors.Join(cause, n.rollback(j))
}
func (n *Native) recover() error {
	before, after, revision, pending, err := n.o.Store.PendingPatch(n.o.Cycle, n.o.Contract)
	if err != nil || !pending {
		return err
	}
	data, err := n.o.Artifacts.Read(n.o.Store, n.o.Cycle, patchID(after, revision-1))
	if err != nil {
		return fmt.Errorf("pending patch source evidence: %w", err)
	}
	var j journal
	if err = json.Unmarshal(data, &j); err != nil {
		return err
	}
	if j.Before.Baseline() != before.Baseline() || j.After.Baseline() != after.Baseline() {
		return errors.New("journal does not match pending intent")
	}
	if err = n.cleanupTemps(j); err != nil {
		return err
	}
	actual, err := n.target.Snapshot()
	if err != nil {
		return err
	}
	if actual.Baseline() == after.Baseline() {
		return n.o.Store.ReconcilePatch(n.o.Cycle, n.o.Contract, actual)
	}
	return n.rollback(j)
}
