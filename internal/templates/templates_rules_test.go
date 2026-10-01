package templates

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yanai/yanai-harness/internal/config"
)

func TestDefaultConfigAutoApproveRulesAreValid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(defaultConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(defaultConfig), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Commands.AutoApprove) == 0 {
		t.Fatal("the starter configuration has no auto-approval rules")
	}
	for _, rule := range cfg.Commands.AutoApprove {
		if err := rule.Validate(); err != nil {
			t.Error(err)
		}
	}
}
