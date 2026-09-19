package templates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const aPrompt = "prompts/product-owner.md"

// extract runs Extract and indexes the outcome by path, which is how every
// test below asks "what happened to this file?".
func extract(t *testing.T, dest string) (map[string]Result, int) {
	t.Helper()
	results, from, err := Extract(dest)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	byPath := map[string]Result{}
	for _, r := range results {
		byPath[r.Path] = r
	}
	return byPath, from
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// shipOlderVersion rewrites the manifest so it records `content` as the version
// this tool last shipped for key. That is how a test says "upstream has moved
// on since this workspace was created" without needing two builds.
func shipOlderVersion(t *testing.T, dest, key, content string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dest, ManifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	m.Files[key] = sum([]byte(content))
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ManifestName), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractCreatesEveryTemplateAndAManifest(t *testing.T) {
	dest := t.TempDir()
	results, from := extract(t, dest)
	if from != -1 {
		t.Errorf("fromVersion on an empty workspace = %d, expected -1", from)
	}
	if len(results) == 0 {
		t.Fatal("Extract wrote nothing")
	}
	for path, r := range results {
		if r.How != Created {
			t.Errorf("%s: disposition = %v, expected Created", path, r.How)
		}
	}
	for _, want := range []string{aPrompt, "context/alcance.md", "context/producto.md", "yanai.config.json"} {
		if _, ok := results[want]; !ok {
			t.Errorf("%s was not extracted", want)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, ManifestName)); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
}

func TestReExtractReportsUnchangedAndWritesNothing(t *testing.T) {
	dest := t.TempDir()
	extract(t, dest)

	results, from := extract(t, dest)
	if from != TemplateVersion {
		t.Errorf("fromVersion = %d, expected %d", from, TemplateVersion)
	}
	for path, r := range results {
		if r.How != Unchanged {
			t.Errorf("%s: disposition = %v, expected Unchanged", path, r.How)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, aPrompt+".new")); !os.IsNotExist(err) {
		t.Error("a .new file was written when nothing had changed")
	}
}

// The case the whole mechanism exists for: upstream corrects a prompt, the user
// never touched it, so they get the correction.
func TestUneditedFileIsUpdatedInPlace(t *testing.T) {
	dest := t.TempDir()
	extract(t, dest)

	const older = "an older shipped version\n"
	target := filepath.Join(dest, aPrompt)
	current := read(t, target)
	if err := os.WriteFile(target, []byte(older), 0o644); err != nil {
		t.Fatal(err)
	}
	shipOlderVersion(t, dest, aPrompt, older)

	results, _ := extract(t, dest)
	if got := results[aPrompt].How; got != Updated {
		t.Fatalf("%s: disposition = %v, expected Updated", aPrompt, got)
	}
	if read(t, target) != current {
		t.Error("an unedited file was not brought up to date")
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Error("a .new file was written for an unedited file")
	}
}

// The rule that outranks the rest: a locally modified file is never
// overwritten, even when upstream has a correction for it.
func TestEditedFileIsKeptAndNewVersionWrittenAlongside(t *testing.T) {
	dest := t.TempDir()
	extract(t, dest)

	target := filepath.Join(dest, aPrompt)
	current := read(t, target)
	const mine = "# My own prompt\n\nI rewrote this entirely.\n"
	if err := os.WriteFile(target, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	shipOlderVersion(t, dest, aPrompt, "an older shipped version\n")

	results, _ := extract(t, dest)
	r := results[aPrompt]
	if r.How != Conflict {
		t.Fatalf("%s: disposition = %v, expected Conflict", aPrompt, r.How)
	}
	if read(t, target) != mine {
		t.Fatal("a local edit was overwritten")
	}
	if got := read(t, target+".new"); got != current {
		t.Error(".new does not carry the incoming version")
	}
	if r.New != aPrompt+".new" {
		t.Errorf("Result.New = %q", r.New)
	}
}

// Until the user merges the .new file, the conflict must keep being reported —
// a one-shot warning scrolls away and the correction is lost.
func TestConflictIsReportedAgainUntilMerged(t *testing.T) {
	dest := t.TempDir()
	extract(t, dest)
	target := filepath.Join(dest, aPrompt)
	if err := os.WriteFile(target, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shipOlderVersion(t, dest, aPrompt, "an older shipped version\n")

	for i := range 2 {
		results, _ := extract(t, dest)
		if got := results[aPrompt].How; got != Conflict {
			t.Fatalf("run %d: disposition = %v, expected Conflict", i+1, got)
		}
	}
}

// Nothing to offer means nothing to say: when upstream has not moved, a local
// edit is the workspace's own business and must not be reported as a conflict.
func TestLocalEditWithoutUpstreamChangeIsNotAConflict(t *testing.T) {
	dest := t.TempDir()
	extract(t, dest)

	target := filepath.Join(dest, aPrompt)
	const mine = "# My own prompt\n"
	if err := os.WriteFile(target, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	results, _ := extract(t, dest)
	if got := results[aPrompt].How; got != Unchanged {
		t.Errorf("disposition = %v, expected Unchanged", got)
	}
	if read(t, target) != mine {
		t.Error("a local edit was overwritten")
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Error("a .new file was written although upstream had not changed")
	}
}

// Workspaces created before this mechanism have no record of what was shipped,
// so Extract must assume every file is local and overwrite nothing.
func TestWorkspaceWithoutManifestNeverOverwrites(t *testing.T) {
	dest := t.TempDir()
	extract(t, dest)
	if err := os.Remove(filepath.Join(dest, ManifestName)); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dest, aPrompt)
	const mine = "# Edited before manifests existed\n"
	if err := os.WriteFile(target, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	results, from := extract(t, dest)
	if from != -1 {
		t.Errorf("fromVersion = %d, expected -1 for a manifest-less workspace", from)
	}
	if got := results[aPrompt].How; got != Conflict {
		t.Errorf("%s: disposition = %v, expected Conflict", aPrompt, got)
	}
	if read(t, target) != mine {
		t.Error("an edit was overwritten in a workspace with no manifest")
	}
}

func TestCorruptManifestIsTreatedAsAbsent(t *testing.T) {
	dest := t.TempDir()
	extract(t, dest)
	if err := os.WriteFile(filepath.Join(dest, ManifestName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dest, aPrompt)
	const mine = "# mine\n"
	if err := os.WriteFile(target, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	results, from := extract(t, dest)
	if from != -1 {
		t.Errorf("fromVersion = %d, expected -1", from)
	}
	if got := results[aPrompt].How; got != Conflict {
		t.Errorf("disposition = %v, expected Conflict", got)
	}
	if read(t, target) != mine {
		t.Error("a corrupt manifest led to an overwrite")
	}
}

// 'init --repo' rewrites yanai.config.json immediately after extraction. That
// edit must survive every later init: losing it would silently point the whole
// team at the template's default repository.
func TestConfiguredRepoPathSurvivesReInit(t *testing.T) {
	dest := t.TempDir()
	extract(t, dest)

	cfg := filepath.Join(dest, "yanai.config.json")
	rewritten := `{"repo": {"path": "/somewhere/real"}}`
	if err := os.WriteFile(cfg, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}

	// Even across a template upgrade that touches the config.
	shipOlderVersion(t, dest, "yanai.config.json", `{"repo": {"path": "../old-default"}}`)

	results, _ := extract(t, dest)
	if got := results["yanai.config.json"].How; got != Conflict {
		t.Errorf("disposition = %v, expected Conflict", got)
	}
	if read(t, cfg) != rewritten {
		t.Fatal("the configured repo path was overwritten")
	}
}
