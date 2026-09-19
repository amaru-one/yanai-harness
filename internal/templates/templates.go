// Package templates carries the embedded files that 'yanai init' writes out,
// and the rules for bringing an existing workspace up to date with them.
//
// The prompts and context documents are the product: a correction to a prompt
// is only worth making if it reaches the workspaces people actually run. Plain
// skip-if-exists extraction meant it never did. So init records what it wrote,
// and a later init compares three versions of each file — the one on disk, the
// one it originally wrote, and the one embedded in the binary — to tell an
// upstream change apart from the user's own edit.
//
// The rule that governs all of it: a file the user edited is never overwritten.
package templates

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

//go:embed files
var embedded embed.FS

// TemplateVersion is bumped whenever the embedded files change in a way users
// should be told about. It is recorded in the manifest so an upgrade can be
// reported as a version step rather than an unexplained set of diffs.
const TemplateVersion = 2

// ManifestName is the record init leaves in the workspace. It is what makes
// "you edited this" distinguishable from "we changed this".
const ManifestName = ".yanai-template-manifest.json"

// Manifest records, per file, the SHA-256 of the version this tool last
// shipped into the workspace — not of what is on disk now.
//
// That distinction is the whole design. Hashing the disk would record local
// customizations as if they were ours, and the next upgrade would overwrite
// them: 'init --repo' edits yanai.config.json right after extraction, so a
// disk-based manifest would hand that edited config back as "unedited" and
// replace the configured repo path with the template default. Comparing the
// disk against what we shipped instead makes any local change — the user's or
// init's own — read as a local change, which is what it is.
type Manifest struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"`
}

// Disposition is what happened to one template file during Extract.
type Disposition int

const (
	// Created: the file was not in the workspace and was written.
	Created Disposition = iota
	// Updated: upstream changed it and the user had not edited it, so it was
	// replaced in place.
	Updated
	// Conflict: upstream changed it and so did the user. Theirs is kept and
	// the new version is written alongside as <name>.new.
	Conflict
	// Unchanged: the embedded file still matches what init last wrote.
	Unchanged
)

// Result is one file's outcome. New is set only for a Conflict.
type Result struct {
	Path string
	How  Disposition
	New  string
}

// Extract writes the templates into dest, reports what it did with each one,
// and records the manifest. A locally modified file is never overwritten: when
// upstream has also moved, the incoming version is written next to it as
// <name>.new for the user to merge.
//
// A workspace with no manifest predates this mechanism, so nothing is known
// about which files were edited. Extract assumes all of them were and takes the
// cautious branch, which is why fromVersion reports -1 for that case.
func Extract(dest string) (results []Result, fromVersion int, err error) {
	old, hasManifest := readManifest(dest)
	fromVersion = -1
	if hasManifest {
		fromVersion = old.Version
	}

	next := Manifest{Version: TemplateVersion, Files: map[string]string{}}

	err = fs.WalkDir(embedded, "files", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("files", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		destPath := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(destPath, 0o755)
		}

		want, err := embedded.ReadFile(path)
		if err != nil {
			return err
		}
		wantSum := sum(want)
		key := filepath.ToSlash(rel)

		onDisk, readErr := os.ReadFile(destPath)
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				return readErr
			}
			if err := write(destPath, want); err != nil {
				return err
			}
			results = append(results, Result{Path: key, How: Created})
			next.Files[key] = wantSum
			return nil
		}

		diskSum := sum(onDisk)
		shipped, known := old.Files[key]

		switch {
		case known && shipped == wantSum:
			// We have nothing new to offer. Whatever the workspace did with
			// this file is its own business, so say nothing about it.
			results = append(results, Result{Path: key, How: Unchanged})
			next.Files[key] = shipped

		case diskSum == wantSum:
			// Already identical to what we ship, however it got there.
			results = append(results, Result{Path: key, How: Unchanged})
			next.Files[key] = wantSum

		case known && diskSum == shipped:
			// Untouched since we wrote it, and upstream has moved on.
			if err := write(destPath, want); err != nil {
				return err
			}
			results = append(results, Result{Path: key, How: Updated})
			next.Files[key] = wantSum

		default:
			// Locally modified (or we have no record and must assume so) and
			// upstream moved too. Keep theirs; put ours alongside. The shipped
			// hash is left as it was, so the conflict is reported again until
			// the merge actually happens.
			newPath := destPath + ".new"
			if err := write(newPath, want); err != nil {
				return err
			}
			results = append(results, Result{Path: key, How: Conflict, New: key + ".new"})
			if known {
				next.Files[key] = shipped
			}
		}
		return nil
	})
	if err != nil {
		return nil, fromVersion, err
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })
	return results, fromVersion, writeManifest(dest, next)
}

func writeManifest(dest string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, ManifestName), append(data, '\n'), 0o644)
}

func readManifest(dest string) (Manifest, bool) {
	data, err := os.ReadFile(filepath.Join(dest, ManifestName))
	if err != nil {
		return Manifest{Files: map[string]string{}}, false
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil || m.Files == nil {
		// A corrupt manifest is treated as no manifest: assume every file is
		// the user's and never overwrite on a guess.
		return Manifest{Files: map[string]string{}}, false
	}
	return m, true
}

func write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
