// Package lockfile provides one primitive: an exclusive, non-blocking
// advisory lock on a file, held for the lifetime of a process. It underlies
// both the application-repository writer lock (internal/repository) and the
// harness workspace writer lock (internal/ws) — same mechanism, different
// scope, and each answers "is another process using this right now?", never
// anything about a process that already died (see internal/workflow's claim
// leases for that question instead).
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Lock takes an exclusive lock on path, creating it if necessary, and
// returns a function that releases it. Closing the descriptor — including
// implicitly, if the process dies — releases the OS lock, so a crashed
// process never leaves this lock held. Supported hosts are macOS and Linux.
func Lock(path string) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close() //nolint:errcheck
		return nil, fmt.Errorf("%s is already locked by another process: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// LockWait is Lock, retried every 200ms for up to wait while another process
// holds the lock. A short command started right beside a long one (a
// decision recorded while a run is still starting) waits instead of failing.
func LockWait(path string, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		unlock, err := Lock(path)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return unlock, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}
