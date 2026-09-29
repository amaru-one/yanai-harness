package repository_test

import (
	"strings"
	"testing"

	"github.com/yanai/yanai-harness/internal/repository"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"*.yml", "deploy/observability/loki.yml", true},
		{"deploy/**/*.yml", "deploy/observability/loki.yml", true},
		{"deploy/**/*.yml", "deploy/loki.yml", true},
		{"deploy/*.yml", "deploy/observability/loki.yml", false},
		{"**/SPEC.md", "yanai-server/SPEC.md", true},
		{"main.?o", "yanai-server/main.go", true},
		{"", "anything", true},
	}
	for _, c := range cases {
		got, err := repository.GlobMatch(c.glob, c.path)
		if err != nil || got != c.want {
			t.Errorf("GlobMatch(%q, %q) = %v, %v; want %v", c.glob, c.path, got, err, c.want)
		}
	}
}

func TestSearchSeesWholeRepositoryButNeverSecrets(t *testing.T) {
	r := fixture(t)
	r.AllowedPaths = nil // the default: the whole repository
	write(t, r.Path, ".gitignore", "ignored/\n")
	write(t, r.Path, ".github/workflows/ci.yml", "name: NEEDLE ci\n")
	write(t, r.Path, "deploy/observability/loki.yml", "auth_enabled: false # needle\n")
	write(t, r.Path, "yanai-server/.env", "KEY=NEEDLE\n")
	write(t, r.Path, "ignored/notes.txt", "NEEDLE\n")
	write(t, r.Path, "yanai-server/blob.bin", "NEEDLE\x00binary")
	target := open(t, r)

	files, truncated, err := target.ListFiles("", "*.yml")
	if err != nil || truncated {
		t.Fatal(err, truncated)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != ".github/workflows/ci.yml,deploy/observability/loki.yml" {
		t.Fatalf("listed %v", paths)
	}

	hits, _, err := target.Grep("needle", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, h := range hits {
		found = append(found, h.Path)
	}
	if strings.Join(found, ",") != ".github/workflows/ci.yml,deploy/observability/loki.yml" {
		t.Fatalf("grep found %v (secrets, ignored and binary files must be skipped)", found)
	}
	if hits[0].Line != 1 || !strings.Contains(hits[0].Text, "NEEDLE ci") {
		t.Fatalf("hit %+v", hits[0])
	}
	if hits, _, _ := target.Grep("NEEDLE", "deploy", "", false); len(hits) != 0 {
		t.Fatalf("case-sensitive grep under deploy matched %v", hits)
	}
	if _, _, err := target.Grep("(", "", "", false); err == nil {
		t.Fatal("invalid pattern accepted")
	}
	if _, _, err := target.ListFiles("../outside", ""); err == nil {
		t.Fatal("directory outside the repository accepted")
	}
}

func TestLines(t *testing.T) {
	data := []byte("one\ntwo\nthree\n")
	if repository.LineCount(data) != 3 || repository.LineCount([]byte("a\nb")) != 2 {
		t.Fatal("line count")
	}
	got, err := repository.Lines(data, 2, 3)
	if err != nil || got != "two\nthree\n" {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := repository.Lines(data, 2, 0); got != "two\nthree\n" {
		t.Fatalf("open end: %q", got)
	}
	if got, _ := repository.Lines(data, 3, 99); got != "three\n" {
		t.Fatalf("clamped end: %q", got)
	}
	if _, err := repository.Lines(data, 5, 6); err == nil {
		t.Fatal("range past the end accepted")
	}
}
