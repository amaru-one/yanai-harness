package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ArtifactStore publishes immutable artifacts beneath a workspace root. A
// reference is usable only after its content hash has been verified.
type ArtifactStore struct{ Root string }

func (s ArtifactStore) Publish(ref ArtifactRef, content []byte) (ArtifactRef, error) {
	path, err := SafeRelativePath(s.Root, ref.Path)
	if err != nil {
		return ArtifactRef{}, err
	}
	actual := contentHashBytes(content)
	if ref.SHA256 != "" && ref.SHA256 != actual {
		return ArtifactRef{}, errors.New("artifact hash does not match declared hash")
	}
	ref.Path, ref.SHA256 = path, actual
	dest := filepath.Join(s.Root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return ArtifactRef{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".artifact-*")
	if err != nil {
		return ArtifactRef{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return ArtifactRef{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return ArtifactRef{}, err
	}
	if err := tmp.Close(); err != nil {
		return ArtifactRef{}, err
	}
	if _, err := os.Stat(dest); err == nil {
		return ArtifactRef{}, fmt.Errorf("artifact already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return ArtifactRef{}, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return ArtifactRef{}, err
	}
	return ref, nil
}

func (s ArtifactStore) Read(ref ArtifactRef) ([]byte, error) {
	path, err := SafeRelativePath(s.Root, ref.Path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	if ref.SHA256 == "" || contentHashBytes(b) != ref.SHA256 {
		return nil, errors.New("artifact is missing or has been tampered with")
	}
	return b, nil
}

func contentHashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
