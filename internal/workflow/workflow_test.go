package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func roles(extra bool) []Role {
	r := []Role{{ID: "product-owner", Model: "model"}, {ID: "arquitecto-bd", Model: "model"}, {ID: "ingeniero", Model: "model"}, {ID: "disenador", Model: "model"}}
	if extra {
		r = append(r, Role{ID: "normativo", Model: "model"})
	}
	return r
}

func ticket(id, owner string, deps ...string) Ticket {
	return Ticket{SchemaVersion: "1", ID: id, Type: "implementation", Owner: owner, Status: "pending", DependsOn: deps, Criteria: []string{"tested"}, Outputs: []string{"artifact"}}
}

func TestValidateProposalAndRoleRegistry(t *testing.T) {
	p := Proposal{SchemaVersion: "1", ID: "p-1", Outcome: OutcomeProposeChange, Summary: "x", Tickets: []Ticket{ticket("T-1", "normativo")}}
	if err := ValidateProposal(p, roles(true)); err != nil {
		t.Fatal(err)
	}
}

func TestProductOutcomes(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeNoChange, OutcomeNeedsEvidence, OutcomeOutOfScope, OutcomeBlockedBaseline} {
		if err := ValidateProposal(Proposal{SchemaVersion: "1", ID: "p-" + string(outcome), Outcome: outcome}, roles(false)); err != nil {
			t.Fatalf("outcome %s: %v", outcome, err)
		}
	}
	if err := ValidateProposal(Proposal{SchemaVersion: "1", ID: "bad", Outcome: OutcomeProposeChange}, roles(false)); err == nil {
		t.Fatal("PROPOSE_CHANGE without tickets accepted")
	}
}

func TestValidateTicketsRejectsDuplicatesCyclesAndMissingCriteria(t *testing.T) {
	cases := []struct {
		name string
		ts   []Ticket
	}{
		{"duplicate", []Ticket{ticket("T-1", "ingeniero"), ticket("T-1", "ingeniero")}},
		{"cycle", []Ticket{ticket("T-1", "ingeniero", "T-2"), ticket("T-2", "arquitecto-bd", "T-1")}},
		{"criteria", []Ticket{{ID: "T-1", Type: "implementation", Owner: "ingeniero", Outputs: []string{"x"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTickets(tc.ts, roles(false)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestReadyTicketsUsesDependencies(t *testing.T) {
	ts := []Ticket{ticket("T-2", "ingeniero", "T-1"), ticket("T-1", "arquitecto-bd")}
	if got := ReadyTickets(ts); len(got) != 1 || got[0].ID != "T-1" {
		t.Fatalf("ready=%v", got)
	}
	ts[0].Status = "verified"
	if got := ReadyTickets(ts); len(got) != 1 || got[0].ID != "T-1" {
		t.Fatalf("pending dependency should remain ready only once: %v", got)
	}
	ts[1].Status = "verified"
	if got := ReadyTickets(ts); len(got) != 0 {
		t.Fatalf("verified tickets should not be scheduled: %v", got)
	}
}

// Store behaviour (transitions, claims, attempts, project scoping) now lives
// in store_test.go, alongside the store.go/transitions.go/claims.go/
// attempts.go it exercises. ApprovalValid is pure and stays here.
func TestApprovalValid(t *testing.T) {
	if !ApprovalValid(Approval{Actor: "human", ContractHash: "c", PlanHash: "p", ScopeHash: "s", Baseline: "b"}, "p", "s", "b", "c") {
		t.Fatal("valid approval rejected")
	}
	if ApprovalValid(Approval{Actor: "human", ContractHash: "c", PlanHash: "p", ScopeHash: "s", Baseline: "b"}, "changed", "s", "b", "c") {
		t.Fatal("changed plan remained approved")
	}
}

func TestSafeRelativePath(t *testing.T) {
	if _, err := SafeRelativePath(t.TempDir(), "../../etc/passwd"); err == nil {
		t.Fatal("escape accepted")
	}
	if got, err := SafeRelativePath(t.TempDir(), "cycles/001/state.json"); err != nil || got != "cycles/001/state.json" {
		t.Fatalf("path=%q err=%v", got, err)
	}
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeRelativePath(root, "link/file"); err == nil {
		t.Fatal("symlink escape accepted")
	}
}

func TestArtifactStorePublishesImmutablyAndDetectsTampering(t *testing.T) {
	s := openTestStore(t, "yanai")
	afs := ArtifactStore{Root: t.TempDir()}
	ref, err := afs.Publish(s, 1, ArtifactRef{ID: "a-1", Path: "artifacts/one.md", Version: "1"}, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := afs.Publish(s, 1, ref, []byte("changed")); err == nil {
		t.Fatal("overwrote immutable artifact")
	}
	if got, err := afs.Read(s, 1, ref.ID); err != nil || string(got) != "hello" {
		t.Fatalf("read=%q err=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(afs.Root, "artifacts/one.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := afs.Read(s, 1, ref.ID); err == nil {
		t.Fatal("tampered artifact accepted")
	}
}
