// Package templates carries the embedded files that 'yanai init' writes out.
package templates

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed files
var embedded embed.FS

// Extract writes the templates into dest. It doesn't overwrite what already
// exists; it returns the list of created files and the list of files that
// already existed.
func Extract(dest string) (created, existing []string, err error) {
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
		if _, err := os.Stat(destPath); err == nil {
			existing = append(existing, rel)
			return nil
		}
		data, err := embedded.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, data, 0o644); err != nil {
			return err
		}
		created = append(created, rel)
		return nil
	})
	return created, existing, err
}
