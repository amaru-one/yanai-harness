package team

import (
	"encoding/json"
	"testing"

	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

// A real provider emits "depends_on": [] for a ticket that depends on nothing,
// where the mock omits the key entirely. The two spellings survive a save/load
// round trip differently — workflow.Ticket drops an empty list through
// omitempty and reads back nil, ws.Task keeps it and reads back an empty slice
// — so a bare reflect.DeepEqual reported a plan as changed the moment it was
// reloaded, and every single-ticket cycle stopped at `review` with "task
// projection changed". Only real model output reaches this; that is why the
// mock-driven tests never saw it.
func TestTaskProjectionSurvivesEmptyDependencyRoundTrip(t *testing.T) {
	ticket := workflow.Ticket{
		SchemaVersion: "1", ID: "T-001", Type: workflow.TechnicalEnabler,
		Title: "Only ticket", Description: "No dependencies.", Owner: "ingeniero",
		Status: "pending", Outputs: []string{"yanai-server/x.go"},
		Criteria: []string{"compiles"}, DependsOn: []string{}, MaxAttempts: 2, Revision: 1,
	}
	plan := workflow.Proposal{SchemaVersion: "1", Tickets: []workflow.Ticket{ticket}}

	// Persist and reload exactly as the workspace does.
	raw, err := json.Marshal(struct {
		Plan  workflow.Proposal `json:"plan"`
		Tasks []ws.Task         `json:"tasks"`
	}{plan, tasksForProposal(plan)})
	if err != nil {
		t.Fatal(err)
	}
	var reloaded struct {
		Plan  workflow.Proposal `json:"plan"`
		Tasks []ws.Task         `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}

	if reloaded.Plan.Tickets[0].DependsOn != nil {
		t.Fatal("precondition: the ticket's empty dependency list should reload as nil")
	}
	if reloaded.Tasks[0].DependsOn == nil {
		t.Fatal("precondition: the task's empty dependency list should reload as an empty slice")
	}
	if !sameTaskProjection(reloaded.Tasks, tasksForProposal(reloaded.Plan)) {
		t.Error("a reloaded plan with no dependencies must still match its own projection")
	}
}

// The comparison must stay strict about dependencies that actually differ.
func TestTaskProjectionStillRejectsChangedDependencies(t *testing.T) {
	base := []ws.Task{{ID: "T-001", Owner: "ingeniero", Status: "pending"}}
	changed := []ws.Task{{ID: "T-001", Owner: "ingeniero", Status: "pending", DependsOn: []string{"T-000"}}}
	if sameTaskProjection(base, changed) {
		t.Error("an added dependency must not compare equal")
	}
	if sameTaskProjection(changed, base) {
		t.Error("a removed dependency must not compare equal")
	}
	if sameTaskProjection(base, []ws.Task{}) {
		t.Error("a different task count must not compare equal")
	}
}
