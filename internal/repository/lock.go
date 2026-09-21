package repository

import (
	"fmt"
	"path/filepath"

	"github.com/yanai/yanai-harness/internal/lockfile"
)

// LockWriter serializes runs across workspaces and linked Git worktrees.
// The lock file stays in Git metadata: deleting it would let a second
// process lock a different inode.
func (t *Target) LockWriter() (func(), error) {
	return t.LockWriterExpected("")
}

// LockWriterExpected also permits precisely the engine-recorded patch state.
func (t *Target) LockWriterExpected(expected string) (func(), error) {
	path := filepath.Join(t.CommonDir, "yanai-harness.writer.lock")
	unlock, err := lockfile.Lock(path)
	if err != nil {
		return nil, fmt.Errorf("repository already has an active writer (or locking failed): %w", err)
	}
	s, err := t.Snapshot()
	if err != nil || (s.Dirty && (expected == "" || s.Baseline() != expected)) {
		unlock()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("repository has uncommitted or untracked changes; run requires a clean checkout; preserve your work and regenerate the plan after resolving it")
	}
	return unlock, nil
}
