package executor

import (
	"strings"
	"testing"

	"github.com/yanai/yanai-harness/internal/workflow"
)

func TestProjectCheckInputsAreRequiredBeforeApproval(t *testing.T) {
	check := workflow.Check{ID: "database", Args: []string{"go", "build", "./..."}, Dir: "yanai-server", TimeoutSeconds: 30, RequiredEnv: []string{"TEST_DATABASE_URL"}, PostgresURLVar: "TEST_DATABASE_URL"}
	o, _ := fixture(t, []workflow.Check{check})
	policy := workflow.ExecutionPolicy{MaxTokens: 1, MaxCostUSD: 1, MaxActiveSeconds: 1, MaxCalls: 1, Prices: map[string]workflow.ModelPrice{}, Checks: []workflow.Check{check}}
	good := map[string]string{"TEST_DATABASE_URL": "postgres://test:test@127.0.0.1:55432/postgres?sslmode=disable"}
	if err := Preflight(o.Repo, o.Artifacts.Root, policy, good); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		inputs map[string]string
		want   string
	}{
		{"missing", nil, "TEST_DATABASE_URL"},
		{"remote database", map[string]string{"TEST_DATABASE_URL": "postgres://test:test@database.example:5432/postgres"}, "loopback"},
		{"undeclared", map[string]string{"TEST_DATABASE_URL": good["TEST_DATABASE_URL"], "EXTRA": "value"}, "not declared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Preflight(o.Repo, o.Artifacts.Root, policy, tc.inputs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("preflight: %v, want %q", err, tc.want)
			}
		})
	}
}
