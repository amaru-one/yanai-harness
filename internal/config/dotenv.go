package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// APIKey returns the OpenRouter key: the configured environment variable
// first, then the same name read from a .env file in the workspace or next
// to the yanai binary (see KeySources). Only that one variable is read; the
// rest of the file is ignored and nothing is exported to the environment.
func (c *Config) APIKey() string {
	if v := os.Getenv(c.OpenRouter.APIKeyEnv); v != "" {
		return v
	}
	for _, path := range c.KeySources() {
		if v := dotenvValue(path, c.OpenRouter.APIKeyEnv); v != "" {
			return v
		}
	}
	return ""
}

// KeySources lists the .env files APIKey consults, in order: the
// workspace's, then the one beside the running yanai binary.
func (c *Config) KeySources() []string {
	var out []string
	if c.path != "" {
		out = append(out, filepath.Join(filepath.Dir(c.path), ".env"))
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if p := filepath.Join(filepath.Dir(exe), ".env"); len(out) == 0 || p != out[0] {
			out = append(out, p)
		}
	}
	return out
}

// dotenvValue reads one variable from a .env file: KEY=value lines, an
// optional leading "export ", optional matching quotes; comments and blank
// lines are skipped. A missing or unreadable file yields "".
func dotenvValue(path, name string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	value := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		raw = strings.TrimSpace(raw)
		if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') && raw[len(raw)-1] == raw[0] {
			raw = raw[1 : len(raw)-1]
		} else if i := strings.Index(raw, " #"); i >= 0 {
			raw = strings.TrimSpace(raw[:i])
		}
		value = raw // the last assignment wins, as in a shell
	}
	return value
}
