package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunCommandAdoptsFileChangesAndStopsOnIndexChanges(t *testing.T) {
	_, n := fixture(t, nil)
	res, err := n.RunCommand(context.Background(), "C-1", []string{"sh", "-c", "printf 'made by a command\\n' > generated.txt; echo done"}, "yanai-server", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "done") || strings.Join(res.Changed, ",") != "yanai-server/generated.txt" {
		t.Fatalf("result %+v", res)
	}
	// The change is now the engine's recorded state: guarded reads keep working.
	if f, err := n.Read("yanai-server/generated.txt"); err != nil || f == nil || string(f.Data) != "made by a command\n" {
		t.Fatalf("adopted file: %v %v", f, err)
	}
	res, err = n.RunCommand(context.Background(), "C-2", []string{"sh", "-c", "exit 3"}, "yanai-server", 10*time.Second)
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("failing command: %+v %v", res, err)
	}
	if _, err = n.RunCommand(context.Background(), "C-3", []string{"true"}, "../outside", 10*time.Second); err == nil {
		t.Fatal("directory outside the repository accepted")
	}
	// Moving the index is not a file change the worker can commit.
	if _, err = n.RunCommand(context.Background(), "C-4", []string{"git", "add", "generated.txt"}, "yanai-server", 10*time.Second); err == nil || !strings.Contains(err.Error(), "unexpected repository state") {
		t.Fatalf("index change adopted: %v", err)
	}
}
