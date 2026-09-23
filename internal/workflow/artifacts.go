package workflow

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ArtifactStore publishes immutable artifacts beneath a workspace root. A
// reference is usable only after its content hash has been verified.
type ArtifactStore struct{ Root string }

// Publish binds ref to store's pending/published bookkeeping before ever
// touching the filesystem: BeginArtifact's INSERT is what makes two
// concurrent publishers of the same ref or path race safely -- exactly one
// wins the row, the loser errors out before linking any bytes. A pending
// row found already (a previous attempt that committed the row but died
// before linking, or before marking published) is finished rather than
// retried from scratch. A published row is a benign replay when the hash
// matches, and refused otherwise.
func (s ArtifactStore) Publish(store *Store, cycle int, ref ArtifactRef, content []byte) (ArtifactRef, error) {
	path, err := SafeRelativePath(s.Root, ref.Path)
	if err != nil {
		return ArtifactRef{}, err
	}
	actual := contentHashBytes(content)
	if ref.SHA256 != "" && ref.SHA256 != actual {
		return ArtifactRef{}, errors.New("artifact hash does not match declared hash")
	}
	ref.Path, ref.SHA256 = path, actual

	existing, err := store.GetArtifact(cycle, ref.ID)
	switch {
	case err == nil:
		if existing.Path != path || existing.Version != ref.Version || existing.SHA256 != actual {
			return ArtifactRef{}, fmt.Errorf("artifact %s identity/content differs from the stored reservation", ref.ID)
		}
		switch existing.State {
		case "published":
			if existing.SHA256 != actual {
				return ArtifactRef{}, fmt.Errorf("artifact %s already published with a different hash", ref.ID)
			}
			return ref, nil // benign replay, nothing to do
		case "pending":
			// A previous attempt committed the row but died before linking
			// (or being marked published) -- finish it rather than retry
			// from scratch.
		default:
			return ArtifactRef{}, fmt.Errorf("artifact %s is recorded as %q, not publishable here", ref.ID, existing.State)
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := store.BeginArtifact(cycle, ref); err != nil {
			return ArtifactRef{}, err // lost the race; the winner is publishing this ref
		}
	default:
		return ArtifactRef{}, err
	}

	if err := linkIntoPlace(s.Root, path, content, actual); err != nil {
		return ArtifactRef{}, err
	}
	if _, err := store.PublishArtifact(cycle, ref.ID, actual); err != nil {
		return ArtifactRef{}, err
	}
	return ref, nil
}

// linkIntoPlace writes content to a temp file beside dest and links it into
// place atomically via os.Link, whose EEXIST is itself atomic on a name
// collision -- unlike Stat-then-Rename, no window exists where two
// processes both observe "not there yet". A collision is only ever accepted
// when the file already on disk hashes to wantHash; otherwise nothing is
// overwritten. Link failing for a reason other than EEXIST (no hardlink
// support, cross-device temp dir) falls back to O_CREATE|O_EXCL, preserving
// the same all-or-nothing guarantee.
func linkIntoPlace(root, path string, content []byte, wantHash string) error {
	dest := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".artifact-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck
	if _, err := tmp.Write(content); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	switch err := os.Link(tmpName, dest); {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrExist):
		return acceptIfMatches(dest, path, wantHash)
	default:
		f, openErr := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if openErr != nil {
			if errors.Is(openErr, os.ErrExist) {
				return acceptIfMatches(dest, path, wantHash)
			}
			return openErr
		}
		defer f.Close() //nolint:errcheck
		_, writeErr := f.Write(content)
		return writeErr
	}
}

// acceptIfMatches is the shared EEXIST resolution for both linkIntoPlace
// code paths: a name collision is harmless -- and left alone -- only when
// the bytes already there are the ones being published.
func acceptIfMatches(dest, path, wantHash string) error {
	existing, err := os.ReadFile(dest)
	if err != nil {
		return err
	}
	if contentHashBytes(existing) != wantHash {
		return fmt.Errorf("artifact already exists with different content: %s", path)
	}
	return nil // another process's earlier attempt already placed identical bytes
}

// Read verifies against the store's own record for refID, not a
// caller-supplied ArtifactRef -- closing the "trust the caller's hash" gap:
// a forged ref could otherwise make tampered bytes look verified. Only a
// published artifact is readable.
func (s ArtifactStore) Read(store *Store, cycle int, refID string) ([]byte, error) {
	rec, err := store.GetArtifact(cycle, refID)
	if err != nil {
		return nil, err
	}
	if rec.State != "published" {
		return nil, fmt.Errorf("artifact %s is not published (state=%s)", refID, rec.State)
	}
	path, err := SafeRelativePath(s.Root, rec.Path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	if contentHashBytes(b) != rec.SHA256 {
		return nil, errors.New("artifact is missing or has been tampered with")
	}
	return b, nil
}

func contentHashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
