package workflow

import "testing"

func TestCommandRequestsWaitForAHumanDecision(t *testing.T) {
	s := budgetStore(t)
	args := []string{"docker", "compose", "config", "--quiet"}
	first, err := s.RequestCommand(1, "work-a", 3, "worker", args, ".", "validate compose", 120, nil)
	if err != nil || first.Status != CommandPending {
		t.Fatalf("request: %+v %v", first, err)
	}
	// A replayed step finds its own request instead of creating another.
	again, err := s.RequestCommand(1, "work-a", 3, "worker", args, ".", "validate compose", 120, nil)
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if pending, _ := s.PendingCommands(1); len(pending) != 1 {
		t.Fatalf("pending = %v", pending)
	}
	if err := s.DecideCommand(1, first.ID, false, false, ""); err == nil {
		t.Fatal("denial without a note accepted")
	}
	if err := s.DecideCommand(1, first.ID, false, true, "no"); err == nil {
		t.Fatal("--always accepted on a denial")
	}
	if err := s.DecideCommand(1, first.ID, true, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideCommand(1, first.ID, true, false, ""); err == nil {
		t.Fatal("a decided request was decided again")
	}
	// "Always" approves the same command in the same directory later in the cycle.
	later, err := s.RequestCommand(1, "work-a", 9, "worker", args, ".", "again", 120, nil)
	if err != nil || later.Status != CommandApproved {
		t.Fatalf("always-approved repeat: %+v %v", later, err)
	}
	elsewhere, err := s.RequestCommand(1, "work-a", 10, "worker", args, "deploy", "other dir", 120, nil)
	if err != nil || elsewhere.Status != CommandPending {
		t.Fatalf("same command in another directory must ask again: %+v %v", elsewhere, err)
	}
	if err := s.DecideCommand(1, elsewhere.ID, false, false, "use the root compose file"); err != nil {
		t.Fatal(err)
	}
	denied, _ := s.Command(1, elsewhere.ID)
	if denied.Status != CommandDenied || denied.Note != "use the root compose file" {
		t.Fatalf("denied: %+v", denied)
	}
	if pending, _ := s.PendingCommands(1); len(pending) != 0 {
		t.Fatalf("pending after decisions = %v", pending)
	}
}

func TestAutoApproveRules(t *testing.T) {
	logs := AutoApproveRule{Args: []string{"docker", "compose", "-f", "*", "logs", "..."}, Description: "compose logs"}
	ps := AutoApproveRule{Args: []string{"docker", "ps"}, Description: "list containers"}
	cases := []struct {
		rule AutoApproveRule
		args []string
		want bool
	}{
		{logs, []string{"docker", "compose", "-f", "docker-compose.local.yml", "logs", "--tail", "20", "api"}, true},
		{logs, []string{"docker", "compose", "-f", "docker-compose.local.yml", "logs"}, true},
		{logs, []string{"docker", "compose", "-f", "docker-compose.local.yml", "down", "-v"}, false},
		{ps, []string{"docker", "ps"}, true},
		{ps, []string{"docker", "ps", "-a"}, false}, // no "...": exact length
		{ps, []string{"bash", "-c", "docker ps"}, false},
		{AutoApproveRule{Args: []string{"bash", "-n", "*"}, Description: "syntax"}, []string{"bash", "-n", "deploy/smoke.sh"}, true},
		{AutoApproveRule{Args: []string{"bash", "-n", "*"}, Description: "syntax"}, []string{"bash", "-n", "x.sh", "-c"}, false},
	}
	for _, c := range cases {
		if got := c.rule.Matches(c.args); got != c.want {
			t.Errorf("%v matches %v = %v, want %v", c.rule.Args, c.args, got, c.want)
		}
	}
	for _, bad := range []AutoApproveRule{
		{Args: []string{"bash", "..."}, Description: "shell"},
		{Args: []string{"*"}, Description: "anything"},
		{Args: []string{"docker", "...", "ps"}, Description: "dots in the middle"},
		{Args: []string{"docker", "ps"}},
	} {
		if bad.Validate() == nil {
			t.Errorf("rule %v accepted", bad.Args)
		}
	}

	s := budgetStore(t)
	auto, err := s.RequestCommand(1, "work-b", 1, "worker", []string{"docker", "ps"}, ".", "look", 30, []AutoApproveRule{ps})
	if err != nil || auto.Status != CommandApproved || auto.Timeout != 30 || auto.Note != "auto-approved by rule: list containers" {
		t.Fatalf("rule approval: %+v %v", auto, err)
	}
	if pending, _ := s.PendingCommands(1); len(pending) != 0 {
		t.Fatalf("auto-approved request left pending: %v", pending)
	}
}
