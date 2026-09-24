package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// Preflight checks static prerequisites without executing project code. It is
// run before approval and again before execution; a passing preflight is not
// evidence that the project's test suite will pass or even reach the database.
func Preflight(repo config.Repo, workspace string, policy workflow.ExecutionPolicy, inputs map[string]string) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	target, err := repository.Open(repo, workspace)
	if err != nil {
		return err
	}
	n := &Native{o: Options{Artifacts: workflow.ArtifactStore{Root: workspace}, CheckInputs: inputs}, target: target, contract: workflow.ExecutionContract{Policy: policy}}
	toolNames := make([]string, 0, len(policy.Tools))
	for name := range policy.Tools {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)
	for _, name := range toolNames {
		if _, err := n.resolveCheckTool(name, policy.Tools[name]); err != nil {
			return fmt.Errorf("check tool %s: %w", name, err)
		}
	}
	used := map[string]bool{}
	for _, c := range policy.Checks {
		if err := validateCheck(c, policy.Tools); err != nil {
			return fmt.Errorf("check %s: %w", c.ID, err)
		}
		if err := target.CheckDirectory(c.Dir); err != nil {
			return fmt.Errorf("check %s directory: %w", c.ID, err)
		}
		for _, name := range c.RequiredEnv {
			used[name] = true
		}
		if _, err := n.checkInputs(c); err != nil {
			return err
		}
		if c.Args[0] == "go" {
			binary := os.Getenv(GoBinaryEnv)
			if binary == "" {
				binary, err = exec.LookPath("go")
				if err != nil {
					return fmt.Errorf("check %s needs an installed Go toolchain: %w", c.ID, err)
				}
			}
			if _, err := filepath.EvalSymlinks(binary); err != nil {
				return fmt.Errorf("check %s Go toolchain: %w", c.ID, err)
			}
		}
	}
	inputNames := make([]string, 0, len(inputs))
	for name := range inputs {
		inputNames = append(inputNames, name)
	}
	sort.Strings(inputNames)
	for _, name := range inputNames {
		if !used[name] {
			return fmt.Errorf("check input %s is not declared by any approved check", name)
		}
	}
	return nil
}
