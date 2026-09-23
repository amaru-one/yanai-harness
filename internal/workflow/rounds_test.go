package workflow

import (
	"testing"
)

// checkedRound is a round holding everything an implementation legitimately
// requires, so each test below can remove exactly one thing and prove that
// this one thing is what the gate refuses without.
func checkedRound(hash string) ExecutionRound {
	ref := func(id string) *ArtifactRef {
		return &ArtifactRef{ID: id, Path: "cycles/001/execution/" + id + ".json", SHA256: "sha-" + id, Version: "1"}
	}
	return ExecutionRound{
		Cycle: 1, Contract: hash, Ticket: "T-1", TicketRevision: 1, Round: 1,
		State:     RoundChecked,
		Inputs:    []ArtifactRef{*ref("input")},
		Candidate: ref("candidate"), Patch: ref("patch"), Diff: ref("diff"), Manifest: ref("manifest"),
		Expected: "before", Confirmed: "after",
		Checks: []CheckRecord{{CheckID: "backend-test", RunID: "run-1", Evidence: *ref("check"), ExitCode: 0, Passed: true, Tested: "after"}},
	}
}

// readyTicket walks a ticket to candidate_ready, which is where both finishing
// gates require it to be.
func readyTicket(t *testing.T, s *Store, id string) TicketRecord {
	t.Helper()
	rec, err := s.SaveTicket(1, Ticket{SchemaVersion: "1", ID: id, Owner: "ingeniero", Status: TicketPending})
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{TicketClaimed, TicketResponseRecorded, TicketCandidateReady} {
		if rec, err = s.ApplyTicketStatus(1, id, to, ActorEngine, rec.StateVersion, Event{Type: "test:" + to}); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

func TestRoundsAdvanceOnlyForwardAndOnlyOnce(t *testing.T) {
	s, hash, _ := approvedContract(t)
	r := ExecutionRound{Cycle: 1, Contract: hash, Ticket: "T-1", TicketRevision: 1, Round: 1}
	opened, version, err := s.OpenRound(r)
	if err != nil {
		t.Fatal(err)
	}
	if opened.State != RoundOpen || version != 1 {
		t.Fatalf("opened = %+v at %d", opened, version)
	}
	// Reopening is idempotent: a crash between deciding to work and recording
	// anything must not produce a second round.
	again, sameVersion, err := s.OpenRound(r)
	if err != nil || again.Round != 1 || sameVersion != version {
		t.Fatalf("reopen = %+v %d %v", again, sameVersion, err)
	}

	opened.Candidate = &ArtifactRef{ID: "c"}
	opened.State = RoundCandidate
	version, err = s.CommitRound(opened, version, Event{Type: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitRound(opened, version-1, Event{Type: "test"}); err == nil {
		t.Fatal("a stale round version committed")
	}
	back := opened
	back.State = RoundOpen
	if _, err = s.CommitRound(back, version, Event{Type: "test"}); err == nil {
		t.Fatal("a round walked backwards")
	}
	skip := opened
	skip.State = RoundChecked
	if _, err = s.CommitRound(skip, version, Event{Type: "test"}); err != nil {
		t.Fatalf("forward progress refused: %v", err)
	}
}

func TestImplementedIsReachableOnlyThroughTheEvidenceGate(t *testing.T) {
	s, hash, _ := approvedContract(t)
	rec := readyTicket(t, s, "T-1")

	// No generic status update reaches implemented or no_change_reported:
	// the transition table has no edge for either.
	for _, to := range []string{TicketImplemented, TicketNoChangeReported} {
		if _, err := s.ApplyTicketStatus(1, "T-1", to, ActorEngine, rec.StateVersion, Event{Type: "forged"}); err == nil {
			t.Fatalf("a status update reached %q", to)
		}
		if _, err := s.ApplyTicketStatus(1, "T-1", to, ActorHuman, rec.StateVersion, Event{Type: "forged"}); err == nil {
			t.Fatalf("a human status update reached %q", to)
		}
	}

	full := checkedRound(hash)
	if _, _, err := s.OpenRound(ExecutionRound{Cycle: 1, Contract: hash, Ticket: "T-1", TicketRevision: 1, Round: 1}); err != nil {
		t.Fatal(err)
	}
	// The stored round reaches "checked" the way the real flow does, so what
	// the gate below refuses is the evidence, not the ordering.
	version, commitErr := s.CommitRound(full, 1, Event{Type: "test"})
	if commitErr != nil {
		t.Fatal(commitErr)
	}

	// Each omission is refused on its own.
	missing := map[string]func(ExecutionRound) ExecutionRound{
		"no manifest":     func(r ExecutionRound) ExecutionRound { r.Manifest = nil; return r },
		"no patch":        func(r ExecutionRound) ExecutionRound { r.Patch = nil; return r },
		"no diff":         func(r ExecutionRound) ExecutionRound { r.Diff = nil; return r },
		"no candidate":    func(r ExecutionRound) ExecutionRound { r.Candidate = nil; return r },
		"unchanged state": func(r ExecutionRound) ExecutionRound { r.Confirmed = r.Expected; return r },
		"no checks":       func(r ExecutionRound) ExecutionRound { r.Checks = nil; return r },
		"failed check": func(r ExecutionRound) ExecutionRound {
			r.Checks[0].Passed = false
			return r
		},
		"nonzero exit": func(r ExecutionRound) ExecutionRound {
			r.Checks[0].ExitCode = 1
			return r
		},
		"check of another state": func(r ExecutionRound) ExecutionRound {
			r.Checks[0].Tested = "some other state"
			return r
		},
		"unpublished check evidence": func(r ExecutionRound) ExecutionRound {
			r.Checks[0].Evidence = ArtifactRef{}
			return r
		},
		"not yet checked": func(r ExecutionRound) ExecutionRound { r.State = RoundApplied; return r },
	}
	for name, mutate := range missing {
		t.Run(name, func(t *testing.T) {
			broken := checkedRound(hash)
			broken.Checks = append([]CheckRecord(nil), broken.Checks...)
			if err := s.MarkImplemented(mutate(broken), version, rec.StateVersion); err == nil {
				t.Fatal("implemented without complete evidence")
			}
			if current, err := s.GetTicket(1, "T-1"); err != nil || current.Status != TicketCandidateReady {
				t.Fatalf("status = %+v %v", current, err)
			}
		})
	}

	if err := s.MarkImplemented(full, version, rec.StateVersion); err != nil {
		t.Fatal(err)
	}
	final, err := s.GetTicket(1, "T-1")
	if err != nil || final.Status != TicketImplemented {
		t.Fatalf("status = %+v %v", final, err)
	}
	if !SatisfiesDependency(final.Status) {
		t.Fatal("an implemented ticket does not satisfy its dependents")
	}
	// The round is finished: nothing may be committed onto it afterwards.
	if _, err := s.CommitRound(full, version+1, Event{Type: "test"}); err == nil {
		t.Fatal("a finished round accepted another commit")
	}
}

func TestNoChangeRecordsEvidenceButSatisfiesNoDependency(t *testing.T) {
	s, hash, _ := approvedContract(t)
	rec := readyTicket(t, s, "T-1")
	if _, _, err := s.OpenRound(ExecutionRound{Cycle: 1, Contract: hash, Ticket: "T-1", TicketRevision: 1, Round: 1}); err != nil {
		t.Fatal(err)
	}
	base := ExecutionRound{
		Cycle: 1, Contract: hash, Ticket: "T-1", TicketRevision: 1, Round: 1, State: RoundCandidate,
		Candidate: &ArtifactRef{ID: "candidate"}, Manifest: &ArtifactRef{ID: "manifest"},
		Expected: "state", Confirmed: "state", Explanation: "no hace falta cambiar nada",
	}
	unexplained := base
	unexplained.Explanation = " "
	if err := s.MarkNoChange(unexplained, 1, rec.StateVersion); err == nil {
		t.Fatal("an unexplained no-change was recorded")
	}
	withPatch := base
	withPatch.Patch = &ArtifactRef{ID: "patch"}
	if err := s.MarkNoChange(withPatch, 1, rec.StateVersion); err == nil {
		t.Fatal("a no-change carrying a patch was recorded")
	}
	if err := s.MarkNoChange(base, 1, rec.StateVersion); err != nil {
		t.Fatal(err)
	}
	final, err := s.GetTicket(1, "T-1")
	if err != nil || final.Status != TicketNoChangeReported {
		t.Fatalf("status = %+v %v", final, err)
	}
	if SatisfiesDependency(final.Status) {
		t.Fatal("a reported no-change unblocked a dependent")
	}
}

func TestRoundsAreScopedToTheirContractAndTicketRevision(t *testing.T) {
	s, hash, _ := approvedContract(t)
	if _, _, err := s.OpenRound(ExecutionRound{Cycle: 1, Contract: hash, Ticket: "T-1", TicketRevision: 1, Round: 1}); err != nil {
		t.Fatal(err)
	}
	// A different ticket revision is different work under different terms and
	// must not resume the first round's evidence.
	if _, _, found, err := s.LatestRound(1, hash, "T-1", 2); err != nil || found {
		t.Fatalf("a round leaked across ticket revisions: %v %v", found, err)
	}
	if _, _, found, err := s.LatestRound(1, "another-contract-hash", "T-1", 1); err != nil || found {
		t.Fatalf("a round leaked across contracts: %v %v", found, err)
	}
	if _, _, found, err := s.GetRound(1, hash, "T-1", 1, 1); err != nil || !found {
		t.Fatalf("the round is not readable by its own key: %v %v", found, err)
	}
}
