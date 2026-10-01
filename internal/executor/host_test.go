package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostSummaryReportsToolsAndMissingTimeout(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "jq"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	summary := HostSummary(context.Background())
	for _, want := range []string{"- OS:", "On PATH: jq", "Not installed:", "no `timeout` program"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary lacks %q:\n%s", want, summary)
		}
	}
}
