package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDotenvValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "# comment\nOTHER=x\nexport OPENROUTER_API_KEY=\"sk-or-quoted\"\nNOISE = y # note\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := dotenvValue(path, "OPENROUTER_API_KEY"); got != "sk-or-quoted" {
		t.Fatalf("got %q", got)
	}
	if got := dotenvValue(path, "NOISE"); got != "y" {
		t.Fatalf("inline comment not stripped: %q", got)
	}
	if got := dotenvValue(path, "MISSING"); got != "" {
		t.Fatalf("missing key returned %q", got)
	}
	if got := dotenvValue(filepath.Join(t.TempDir(), "none"), "X"); got != "" {
		t.Fatal("missing file returned a value")
	}
}

func TestAPIKeyPrefersEnvironmentThenWorkspaceDotenv(t *testing.T) {
	ws := t.TempDir()
	c := &Config{path: filepath.Join(ws, FileName), OpenRouter: OpenRouter{APIKeyEnv: "YANAI_TEST_KEY"}}
	t.Setenv("YANAI_TEST_KEY", "")
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("YANAI_TEST_KEY=from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := c.APIKey(); got != "from-dotenv" {
		t.Fatalf("workspace .env not used: %q", got)
	}
	t.Setenv("YANAI_TEST_KEY", "from-env")
	if got := c.APIKey(); got != "from-env" {
		t.Fatalf("environment did not win: %q", got)
	}
	if sources := c.KeySources(); len(sources) == 0 || sources[0] != filepath.Join(ws, ".env") {
		t.Fatalf("sources %v", sources)
	}
}
