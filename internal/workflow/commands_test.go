package workflow

import "testing"

func TestCommandRequestsWaitForAHumanDecision(t *testing.T) {
	s := budgetStore(t)
	args := []string{"docker", "compose", "config", "--quiet"}
	first, err := s.RequestCommand(1, "work-a", 3, "worker", args, ".", "validate compose")
	if err != nil || first.Status != CommandPending {
		t.Fatalf("request: %+v %v", first, err)
	}
	// A replayed step finds its own request instead of creating another.
	again, err := s.RequestCommand(1, "work-a", 3, "worker", args, ".", "validate compose")
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
	later, err := s.RequestCommand(1, "work-a", 9, "worker", args, ".", "again")
	if err != nil || later.Status != CommandApproved {
		t.Fatalf("always-approved repeat: %+v %v", later, err)
	}
	elsewhere, err := s.RequestCommand(1, "work-a", 10, "worker", args, "deploy", "other dir")
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
