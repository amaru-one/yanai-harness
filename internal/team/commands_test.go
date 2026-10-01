package team

import (
	"strings"
	"testing"

	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/workflow"
)

func TestCommandOutcomeTellsTheAgentHowItWasApproved(t *testing.T) {
	res := executor.CommandResult{ExitCode: 0, Output: "ok"}
	cases := map[string]workflow.CommandRequest{
		"always":       {Always: true},
		"always-reuse": {Note: "approved always with C-1"},
		"rule":         {Note: "auto-approved by rule: list containers"},
		"once":         {},
	}
	want := map[string]string{"always": "always:", "always-reuse": "always:", "rule": "rule: list containers", "once": "once:"}
	for name, request := range cases {
		got, _ := commandOutcome(res, request).Result["approval"].(string)
		if !strings.HasPrefix(got, want[name]) {
			t.Errorf("%s: approval = %q, want prefix %q", name, got, want[name])
		}
	}
}

func TestCommandGuidanceListsRules(t *testing.T) {
	text := commandGuidance([]workflow.AutoApproveRule{{Args: []string{"docker", "ps", "..."}, Description: "list containers"}})
	if !strings.Contains(text, "docker ps ...: list containers") || !strings.Contains(text, "timeout_seconds") {
		t.Fatalf("guidance:\n%s", text)
	}
}
