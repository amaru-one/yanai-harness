package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckInputsStayOutOfAgentTicket(t *testing.T) {
	const secret = "postgres://test:private@127.0.0.1:5432/postgres?sslmode=disable"
	raw := "# Change\n\n## Task\nRun the database check.\n\n## Acceptance criteria\n- The database check runs.\n\n## Check inputs\n- MAWTA_TEST_ADMIN_URL=" + secret + "\n"
	ticket, err := ParseMarkdownTicket(raw)
	if err != nil || len(ticket.CheckInputNames) != 1 || ticket.CheckInputNames[0] != "MAWTA_TEST_ADMIN_URL" {
		t.Fatalf("ticket names: %+v %v", ticket, err)
	}
	encoded, err := json.Marshal(ticket)
	if err != nil || strings.Contains(string(encoded), secret) {
		t.Fatalf("ticket leaked check input: %s %v", encoded, err)
	}
	inputs, err := ParseCheckInputs(raw)
	if err != nil || inputs["MAWTA_TEST_ADMIN_URL"] != secret {
		t.Fatalf("check inputs: %v %v", inputs, err)
	}
	redacted, err := RedactCheckInputs(raw)
	if err != nil || strings.Contains(redacted, secret) || !strings.Contains(redacted, "MAWTA_TEST_ADMIN_URL=[provided to approved checks]") {
		t.Fatalf("redaction: %s %v", redacted, err)
	}
	for _, line := range []string{"- PATH=/tmp", "- MAWTA_TEST_ADMIN_URL=", "- MAWTA_TEST_ADMIN_URL=second\n- MAWTA_TEST_ADMIN_URL=third"} {
		bad := "# Change\n\n## Task\nRun.\n\n## Acceptance criteria\n- Run.\n\n## Check inputs\n" + line + "\n"
		if _, err := ParseMarkdownTicket(bad); err == nil {
			t.Fatalf("accepted unsafe check input %q", line)
		}
	}
}

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
}
