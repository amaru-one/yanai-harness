package workflow

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestConcurrentPublishProducesExactlyOneWinner drives real goroutine
// concurrency at Publish, not just sequential calls: many publishers race to
// publish the same ref, and BeginArtifact's INSERT against
// workflow_artifacts' primary key is what decides the race -- the same
// pattern TestConcurrentClaimsProduceExactlyOneWinner already exercises for
// claims.
func TestConcurrentPublishProducesExactlyOneWinner(t *testing.T) {
	s := openTestStore(t, "yanai")
	afs := ArtifactStore{Root: t.TempDir()}

	const n = 8
	type result struct {
		ref ArtifactRef
		err error
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			content := []byte(fmt.Sprintf("content-%d", i))
			ref, err := afs.Publish(s, 1, ArtifactRef{ID: "a-1", Path: "artifacts/race.md", Version: "1"}, content)
			results <- result{ref, err}
		}()
	}
	wins, losses := 0, 0
	var winner ArtifactRef
	for i := 0; i < n; i++ {
		r := <-results
		if r.err == nil {
			wins++
			winner = r.ref
		} else {
			losses++
		}
	}
	if wins != 1 || losses != n-1 {
		t.Fatalf("wins=%d losses=%d, want exactly one winner", wins, losses)
	}

	rec, err := s.GetArtifact(1, "a-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != "published" || rec.SHA256 != winner.SHA256 {
		t.Fatalf("record after the race = %+v, want published matching the winner's hash", rec)
	}
	got, err := afs.Read(s, 1, "a-1")
	if err != nil {
		t.Fatal(err)
	}
	if contentHashBytes(got) != winner.SHA256 {
		t.Fatal("published file content does not match the winner's hash")
	}
}

// TestPublishBenignReplayIsIdempotent covers a successful publish replayed
// with identical content: it must return the same ref without error and
// without touching the file (or the store row) again.
func TestPublishBenignReplayIsIdempotent(t *testing.T) {
	s := openTestStore(t, "yanai")
	afs := ArtifactStore{Root: t.TempDir()}

	ref, err := afs.Publish(s, 1, ArtifactRef{ID: "a-1", Path: "artifacts/one.md", Version: "1"}, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetArtifact(1, "a-1")
	if err != nil {
		t.Fatal(err)
	}

	replay, err := afs.Publish(s, 1, ArtifactRef{ID: "a-1", Path: "artifacts/one.md", Version: "1"}, []byte("hello"))
	if err != nil {
		t.Fatalf("benign replay returned an error: %v", err)
	}
	if replay != ref {
		t.Fatalf("replay ref = %+v, want the original %+v", replay, ref)
	}
	after, err := s.GetArtifact(1, "a-1")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("benign replay touched the stored record: before=%+v after=%+v", before, after)
	}
}

// TestPublishRefusesConflictingReplay covers a replay with different
// content against the same ref, both when the existing row is published and
// when it is pending with a file already on disk (a crash between linking
// and marking published). Neither case may overwrite what is already there.
func TestPublishRefusesConflictingReplay(t *testing.T) {
	s := openTestStore(t, "yanai")
	afs := ArtifactStore{Root: t.TempDir()}

	ref, err := afs.Publish(s, 1, ArtifactRef{ID: "a-1", Path: "artifacts/one.md", Version: "1"}, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := afs.Publish(s, 1, ref, []byte("goodbye")); err == nil {
		t.Fatal("conflicting replay against a published artifact was accepted")
	}

	pendingRef := ArtifactRef{ID: "a-2", Path: "artifacts/two.md", Version: "1"}
	if _, err := s.BeginArtifact(1, ArtifactRef{ID: pendingRef.ID, Path: pendingRef.Path, SHA256: contentHashBytes([]byte("first"))}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(afs.Root, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(afs.Root, "artifacts/two.md"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := afs.Publish(s, 1, pendingRef, []byte("second")); err == nil {
		t.Fatal("conflicting replay against a pending artifact with a file on disk was accepted")
	}
	rec, err := s.GetArtifact(1, "a-2")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != "pending" {
		t.Fatalf("pending row was mutated by the refused conflicting replay: %+v", rec)
	}
}

// TestArtifactReconciliationBranches exercises the three outcomes startup
// reconciliation must produce from a pending row, using the same store
// primitives (PendingArtifacts, PublishArtifact, DropPendingArtifact) that
// cmd/yanai's reconcileArtifacts composes: a file on disk with no matching
// row is never adopted, matching the rule for untrusted staged output.
func TestArtifactReconciliationBranches(t *testing.T) {
	s := openTestStore(t, "yanai")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}

	matchContent := []byte("matches")
	if _, err := s.BeginArtifact(1, ArtifactRef{ID: "matching", Path: "artifacts/matching.md", SHA256: contentHashBytes(matchContent)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts/matching.md"), matchContent, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.BeginArtifact(1, ArtifactRef{ID: "missing", Path: "artifacts/missing.md", SHA256: contentHashBytes([]byte("gone"))}); err != nil {
		t.Fatal(err)
	}
	// Deliberately no file written for "missing".

	if _, err := s.BeginArtifact(1, ArtifactRef{ID: "mismatched", Path: "artifacts/mismatched.md", SHA256: contentHashBytes([]byte("recorded"))}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts/mismatched.md"), []byte("actually-different"), 0o644); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending=%d, want 3", len(pending))
	}
	for _, a := range pending {
		data, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(a.Path)))
		switch {
		case os.IsNotExist(readErr):
			if err := s.DropPendingArtifact(a.Cycle, a.RefID); err != nil {
				t.Fatal(err)
			}
		case readErr != nil:
			t.Fatal(readErr)
		case contentHashBytes(data) == a.SHA256:
			if _, err := s.PublishArtifact(a.Cycle, a.RefID, a.SHA256); err != nil {
				t.Fatal(err)
			}
		default:
			// left pending, reported by the caller; nothing to do here.
		}
	}

	if rec, err := s.GetArtifact(1, "matching"); err != nil || rec.State != "published" {
		t.Fatalf("matching artifact = %+v, err=%v, want published", rec, err)
	}
	if _, err := s.GetArtifact(1, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing artifact's pending row was not dropped, err=%v", err)
	}
	if rec, err := s.GetArtifact(1, "mismatched"); err != nil || rec.State != "pending" {
		t.Fatalf("mismatched artifact = %+v, err=%v, want left pending", rec, err)
	}
}
