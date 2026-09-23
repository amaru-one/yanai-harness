package workflow

import (
	"fmt"
	"strings"
	"testing"
)

func TestMarkdownInputAndObservationApproval(t *testing.T) {
	good := "# Change\n\n## Task\nFix the parser.\n\n## Acceptance criteria\n- Reject invalid input.\n\n## Constraints\nKeep the API.\n"
	parsed, err := ParseMarkdownTicket(good)
	if err != nil || parsed.Criteria[0].ID != "AC-001" || parsed.Revision != Digest(good) {
		t.Fatalf("ticket: %+v %v", parsed, err)
	}
	for _, bad := range []string{"", strings.Replace(good, "## Task", "## Interview", 1), strings.Replace(good, "- Reject invalid input.", "", 1), good + "\n## Task\nsecond task", strings.Repeat("a", (1<<20)+1)} {
		if _, err = ParseMarkdownTicket(bad); err == nil {
			t.Fatalf("accepted invalid ticket %q", bad[:min(len(bad), 80)])
		}
	}
	var reply map[string]any
	for _, bad := range []string{`{"a":1,"a":2}`, `{} {}`, `{"a":{"b":1,"b":2}}`} {
		if DecodeStrict(bad, &reply) == nil {
			t.Fatal("accepted ambiguous response")
		}
	}
	s, contract, base := approvedContract(t)
	detail := ObservationDetail{Description: "Keep compatibility?", Requirement: "AC-001", Question: "Confirm the old API remains."}
	if err = s.RecordObservations(1, "ingeniero", "response-1", []ObservationDetail{detail}); err != nil {
		t.Fatal(err)
	}
	if s.HasApproval(1, contract) == nil {
		t.Fatal("observation failed to pause execution")
	}
	obs, err := s.Observations(1)
	if err != nil || len(obs) != 1 {
		t.Fatalf("observations: %v %v", obs, err)
	}
	oldToken, _ := s.ObservationReviewHash(1, contract)
	if err = s.ResolveObservation(1, obs[0].ID, "Keep the existing API."); err != nil {
		t.Fatal(err)
	}
	if s.HasApproval(1, contract) == nil {
		t.Fatal("resolution silently approved execution")
	}
	rec, _ := s.GetCycle(1)
	if s.ApproveObservations(1, rec.StateVersion, contract, oldToken) == nil {
		t.Fatal("stale observation review accepted")
	}
	token, _ := s.ObservationReviewHash(1, contract)
	if err = s.ApproveObservations(1, rec.StateVersion, contract, token); err != nil {
		t.Fatal(err)
	}
	if err = s.HasApproval(1, contract); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordObservations(1, "ingeniero", "response-1", []ObservationDetail{detail}); err != nil {
		t.Fatal(err)
	}
	if err = s.HasApproval(1, contract); err != nil {
		t.Fatal("replay revoked a resolved observation", err)
	}
	state, _, err := s.PatchState(1, contract)
	if err != nil || state.Baseline() != base.Baseline() {
		t.Fatal("supplemental approval changed patch state")
	}
	// Simulate a released v4 store with an intent that was never reconciled.
	next := copyRepo(base)
	next.Content["yanai-server/a.go"] = "changed"
	next.Dirty = true
	next.StatusHash = "changed"
	if err = s.PreparePatch(1, contract, "T-1", next, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE workflow_observations`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE workflow_meta SET value='4' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err = applyMigrations(s.db); err == nil || !strings.Contains(err.Error(), "reconcile pending repository mutations") {
		t.Fatalf("unsafe upgrade: %v", err)
	}
	var version string
	if err = s.db.QueryRow(`SELECT value FROM workflow_meta WHERE key='schema_version'`).Scan(&version); err != nil || version != "4" {
		t.Fatal(fmt.Sprint("failed upgrade changed schema: ", version, err))
	}

}
