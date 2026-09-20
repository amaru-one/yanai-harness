package repository

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockWriter serializes runs across workspaces and linked Git worktrees.
// The lock file stays in Git metadata: deleting it would let a second process
// lock a different inode. Closing the descriptor (including on crash) releases
// the OS lock. Supported execution hosts are macOS and Linux.
func (t *Target) LockWriter() (func(), error) {
	path := filepath.Join(t.CommonDir, "yanai-harness.writer.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("repository already has an active writer (or locking failed): %w", err)
	}
	s, err := t.Snapshot()
	if err != nil || s.Dirty {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("repository has uncommitted or untracked changes; run requires a clean checkout; preserve your work and regenerate the plan after resolving it")
	}
	return func() { _ = f.Close() }, nil
}
