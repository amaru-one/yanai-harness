// Package repoctx builds a backend-only repository index and selected-file context.
package repoctx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repository"
)

func selected(r config.Repo, path string) bool {
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

// Index uses the same boundary checks as explicit reads; symlink targets,
// ignored files, secrets and the UI never enter the index or SPEC payloads.
func Index(r config.Repo) (string, error) {
	target, err := repository.Open(r, "")
	if err != nil {
		return "", err
	}
	snapshot, err := target.Snapshot()
	if err != nil {
		return "", err
	}
	allowed := r.AllowedPaths
	if len(allowed) == 0 {
		allowed = []string{"AGENTS.md", repository.ModuleDir}
	}
	var paths []string
	err = filepath.WalkDir(target.Root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(target.Root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			included := false
			for _, prefix := range allowed {
				if rel == prefix || strings.HasPrefix(rel, prefix+"/") || strings.HasPrefix(prefix, rel+"/") {
					included = true
				}
			}
			if !included || strings.HasPrefix(d.Name(), ".") || d.Name() == "yanai-ui" {
				return filepath.SkipDir
			}
			for _, excluded := range r.ExcludeDirs {
				if d.Name() == excluded {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !selected(r, rel) {
			return nil
		}
		if err := target.CheckPath(rel); err != nil {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString(snapshot.Label())
	b.WriteString("\n## Estructura del repositorio\n\n```\n")
	for _, path := range paths {
		fmt.Fprintln(&b, path)
	}
	b.WriteString("```\n\n## Instrucciones y SPEC.md\n\n")
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
			b.WriteString("_(archivo recortado por tamaño)_\n")
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
