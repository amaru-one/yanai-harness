package team

import (
	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/workflow"
	"testing"
)

func TestLanguageNeutralCheckEvidence(t *testing.T) {
	c := workflow.Check{Args: []string{"cargo", "test"}, Evidence: "output", SuccessPattern: "1 passed", FailurePattern: "ignored"}
	for _, tc := range []struct {
		output string
		fail   bool
	}{{"1 passed", false}, {"0 passed", true}, {"1 passed; 1 ignored", true}} {
		got := verifyTestEvidence(c, executor.CheckResult{Output: tc.output})
		if (got != "") != tc.fail {
			t.Fatalf("%q: %q", tc.output, got)
		}
	}
	c.Evidence = "exit_code"
	c.SuccessPattern = ""
	c.FailurePattern = ""
	if got := verifyTestEvidence(c, executor.CheckResult{}); got != "" {
		t.Fatal(got)
	}
}
