package lockfile

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLockWaitWaitsForReleaseThenGivesUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := LockWait(path, 300*time.Millisecond); err == nil {
		t.Fatal("acquired a held lock")
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("did not wait before giving up")
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		unlock()
	}()
	second, err := LockWait(path, 3*time.Second)
	if err != nil {
		t.Fatalf("lock released by the holder was not acquired: %v", err)
	}
	second()
}
