// Package repoctx builds the repository context for the teaching app that
// gets handed to the agents, in two passes:
//
//   - Index: the file tree plus the content of each SPEC.md/CLAUDE.md.
//     It's the compressed map — it grows with the repo much slower than the
//     code, because a SPEC.md summarizes an entire folder in a few paragraphs.
//   - Files: the full content of the specific paths an agent asked for
//     after reading the Index.
//
// No dump of the whole repository ever goes into an agent's context: that's
// what a fixed byte cap can't scale to as the code grows.
package repoctx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yanai/yanai-harness/internal/config"
)

// Index returns the repository tree and the content of its SPEC.md and
// CLAUDE.md files. It's the first thing an agent sees: enough to decide
// which code files to request with Files, without loading the code itself.
func Index(r config.Repo) (string, error) {
	if strings.TrimSpace(r.Path) == "" {
		return "_(no hay repositorio configurado)_", nil
	}
	root, err := filepath.Abs(r.Path)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("repo.path is not an accessible directory: %s", root)
	}

	exts := map[string]bool{}
	for _, e := range r.Extensions {
		exts[strings.ToLower(e)] = true
	}
	exclude := map[string]bool{}
	for _, d := range r.ExcludeDirs {
		exclude[d] = true
	}
	priority := map[string]bool{}
	for _, p := range r.Priority {
		priority[filepath.ToSlash(p)] = true
	}

	type file struct {
		rel  string
		size int64
	}
	var all []file

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // ignore what can't be read
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if exclude[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
				return filepath.SkipDir
			}
			return nil
		}
		slash := filepath.ToSlash(rel)
		if !exts[strings.ToLower(filepath.Ext(d.Name()))] && !priority[slash] && !priority[d.Name()] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		all = append(all, file{rel: slash, size: info.Size()})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].rel < all[j].rel })

	var b strings.Builder
	fmt.Fprintf(&b, "## Estructura del repositorio (%s)\n\n```\n", root)
	for _, a := range all {
		fmt.Fprintf(&b, "%s (%d bytes)\n", a.rel, a.size)
	}
	if len(all) == 0 {
		b.WriteString("(sin archivos que coincidan con los filtros)\n")
	}
	b.WriteString("```\n\n")
	b.WriteString("## Mapa: contenido de los SPEC.md y CLAUDE.md\n\n")
	b.WriteString("Cada SPEC.md describe su carpeta — propósito, contenido, convenciones — y " +
		"es la fuente de verdad sobre el código, no al revés. Úsalos para decidir qué " +
		"archivos de código pedir a continuación (formato NECESITO, ver tu instrucción).\n\n")

	hadSpec := false
	for _, a := range all {
		base := filepath.Base(a.rel)
		if base != "SPEC.md" && base != "CLAUDE.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(a.rel)))
		if err != nil {
			continue
		}
		hadSpec = true
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", a.rel, string(data))
	}
	if !hadSpec {
		b.WriteString("_(el repositorio no tiene SPEC.md ni CLAUDE.md todavía; pide los archivos " +
			"de código que necesites a partir del árbol de arriba)_\n")
	}
	return b.String(), nil
}

// Files reads the full content of the specific paths an agent requested
// after seeing the Index. Paths outside the repository or missing are
// reported, not silently ignored: an agent that asked for something and
// didn't get it needs to know.
func Files(r config.Repo, paths []string) (string, error) {
	if strings.TrimSpace(r.Path) == "" {
		return "_(no hay repositorio configurado)_", nil
	}
	if len(paths) == 0 {
		return "_(el agente no pidió ningún archivo de código; se apoya en el Indice)_", nil
	}
	root, err := filepath.Abs(r.Path)
	if err != nil {
		return "", err
	}
	rootWithSep := root + string(filepath.Separator)

	maxFile := r.MaxBytesFile
	if maxFile == 0 {
		maxFile = 24000
	}
	maxTotal := r.MaxBytesTotal
	if maxTotal == 0 {
		maxTotal = 400000
	}

	var b strings.Builder
	total := 0
	skipped := 0
	for _, rel := range paths {
		path := filepath.Join(root, filepath.FromSlash(rel))
		absPath, err := filepath.Abs(path)
		if err != nil || !strings.HasPrefix(absPath, rootWithSep) {
			fmt.Fprintf(&b, "### %s\n\n_(ruta fuera del repositorio, omitida)_\n\n", rel)
			continue
		}
		if total >= maxTotal {
			skipped++
			continue
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			fmt.Fprintf(&b, "### %s\n\n_(no se pudo leer: %v)_\n\n", rel, err)
			continue
		}
		text := string(data)
		truncated := false
		if len(text) > maxFile {
			text = text[:maxFile]
			truncated = true
		}
		fmt.Fprintf(&b, "### %s\n\n```%s\n%s\n```\n", rel, language(rel), text)
		if truncated {
			b.WriteString("_(archivo recortado por tamaño)_\n")
		}
		b.WriteString("\n")
		total += len(text)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "_(%d archivo(s) pedidos no se incluyeron: se alcanzó el tope de %d bytes)_\n", skipped, maxTotal)
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
