// Package repoctx builds a policy-filtered repository index and selected-file context.
package repoctx

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repository"
)

func selected(r config.Repo, path string) bool {
	if len(r.Extensions) == 0 {
		return true
	}
	base := filepath.Base(path)
	if base == "SPEC.md" || path == "AGENTS.md" {
		return true
	}
	for _, p := range r.Priority {
		if p == path || p == base {
			return true
		}
	}
	for _, ext := range r.Extensions {
		if strings.EqualFold(ext, filepath.Ext(path)) {
			return true
		}
	}
	return false
}

// Index lists the files Git knows about (tracked or untracked, never
// ignored) that pass the same checks as explicit reads; symlinks, secrets
// and paths outside the configured boundary stay out.
func Index(r config.Repo) (string, error) {
	target, err := repository.Open(r, "")
	if err != nil {
		return "", err
	}
	snapshot, err := target.Snapshot()
	if err != nil {
		return "", err
	}
	files, err := target.Files()
	if err != nil {
		return "", err
	}
	var paths []string
	for _, rel := range files {
		if selected(r, rel) {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString(snapshot.Label())
	b.WriteString("\n## Repository structure\n\n```\n")
	for _, path := range paths {
		fmt.Fprintln(&b, path)
	}
	b.WriteString("```\n\n## Instructions and SPEC.md\n\n")
	for _, path := range paths {
		if filepath.Base(path) != "SPEC.md" && path != "AGENTS.md" {
			continue
		}
		data, err := target.ReadFile(path, 4*1024*1024+1)
		if err != nil {
			return "", err
		}
		if len(data) > 4*1024*1024 {
			return "", fmt.Errorf("governing document too large: %s", path)
		}
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", path, data)
	}
	return b.String(), nil
}

// Files reports denied/missing paths without reading them. Explicit requests
// cannot bypass the index's allowlist, excludes, or extension filters.
func Files(r config.Repo, paths []string) (string, error) {
	target, err := repository.Open(r, "")
	if err != nil {
		return "", err
	}
	snapshot, err := target.Snapshot()
	if err != nil {
		return "", err
	}
	maxFile, maxTotal := r.MaxBytesFile, r.MaxBytesTotal
	if maxFile <= 0 {
		maxFile = 24000
	}
	if maxTotal <= 0 {
		maxTotal = 400000
	}
	var b strings.Builder
	b.WriteString(snapshot.Label())
	total := 0
	for _, path := range paths {
		if err := target.CheckPath(path); err != nil {
			fmt.Fprintf(&b, "\n### %s\n_(denied: %v)_\n", path, err)
			continue
		}
		if !selected(r, path) {
			fmt.Fprintf(&b, "\n### %s\n_(excluded by file filters)_\n", path)
			continue
		}
		limit := min(maxFile, maxTotal-total)
		if limit <= 0 {
			b.WriteString("\n_(context byte limit reached)_\n")
			break
		}
		data, err := target.ReadFile(path, limit+1)
		if err != nil {
			fmt.Fprintf(&b, "\n### %s\n_(not read: %v)_\n", path, err)
			continue
		}
		truncated := len(data) > limit
		if truncated {
			data = data[:limit]
		}
		fmt.Fprintf(&b, "\n### %s\n\n```%s\n%s\n```\n", path, language(path), data)
		if truncated {
			b.WriteString("_(file truncated to fit the size limit)_\n")
		}
		total += len(data)
	}
	return b.String(), nil
}

func language(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".svelte":
		return "svelte"
	case ".ts":
		return "ts"
	case ".js":
		return "js"
	case ".sql":
		return "sql"
	case ".json":
		return "json"
	case ".html":
		return "html"
	case ".css":
		return "css"
	case ".md":
		return "markdown"
	case ".yaml", ".yml":
		return "yaml"
	default:
		return ""
	}
}
