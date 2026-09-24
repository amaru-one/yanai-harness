package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/executor"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// round drives one execution round from wherever it already is to a terminal
// state, and never repeats durable work: a round that already holds a
// validated candidate calls no model, and one that already holds a confirmed
// patch writes nothing. It returns a *repairable error when a replacement
// generation may fix what went wrong, and any other error to stop.
func (e *execution) round(ctx context.Context, ticket workflow.Ticket, r workflow.ExecutionRound, version int64) error {
	if err := e.r.guard(); err != nil {
		return err
	}
	var err error

	candidate := workflow.Candidate{}
	if r.Candidate == nil {
		if r, version, candidate, err = e.generate(ctx, ticket, r, version); err != nil {
			return err
		}
	} else {
		data, readErr := e.artifacts.Read(e.store, e.st.Cycle, r.Candidate.ID)
		if readErr != nil {
			return readErr
		}
		// Re-validate the resumed candidate against the approved ticket rather
		// than unmarshalling it back into trust: it is durable evidence of what
		// was decided, not the decision itself.
		if candidate, err = workflow.DecodeCandidate(string(data), ticket); err != nil {
			return e.reject(r, version, repair("recorded candidate no longer validates: %v", err))
		}
		fmt.Fprintf(os.Stderr, "  %s round %d: reusing the recorded candidate (no model call)\n", ticket.ID, r.Round)
	}
	if len(candidate.Observations) > 0 {
		if err = e.store.RecordObservations(e.st.Cycle, ticket.Owner, r.Candidate.ID, candidate.Observations); err != nil {
			return err
		}
		if err = e.store.RequireResolved(e.st.Cycle); err != nil {
			return err
		}
		if err = e.store.HasApproval(e.st.Cycle, e.st.Approval.ContractHash); err != nil {
			return err
		}
		if err = e.readyCandidate(r); err != nil {
			return err
		}
		return e.reject(r, version, repair("human resolved observations; generate a fresh candidate using the recorded responses"))
	}
	if err = e.readyCandidate(r); err != nil {
		return err
	}

	if candidate.Result == workflow.CandidateNoChange {
		return e.finishNoChange(r, version, candidate)
	}

	if r.Patch == nil {
		if r, version, err = e.apply(ticket, r, version, candidate); err != nil {
			return err
		}
	}
	if r.State != workflow.RoundChecked {
		if r, version, err = e.check(ctx, ticket, r, version); err != nil {
			return err
		}
	}
	return e.finishImplemented(r, version, candidate)
}

// generate builds the complete execution context, publishes it, calls the
// model once, and validates the response into a durable candidate. Every
// generation is charged first, so a call that is not allowed is never made.
func (e *execution) generate(ctx context.Context, ticket workflow.Ticket, r workflow.ExecutionRound, version int64) (workflow.ExecutionRound, int64, workflow.Candidate, error) {
	var candidate workflow.Candidate
	if r.Response != nil {
		raw, err := e.artifacts.Read(e.store, e.st.Cycle, r.Response.ID)
		if err != nil {
			return r, version, candidate, err
		}
		return e.decodeResponse(ticket, r, version, string(raw))
	}
	if r.Round == 1 {
		// The first generation of a ticket is charged here; replacement rounds
		// are charged by the caller before they are opened.
		if err := e.store.ChargeRepair(e.st.Cycle, ticket.ID, ticket.MaxAttempts); err != nil {
			return r, version, candidate, err
		}
	}
	state, err := e.backend.State()
	if err != nil {
		return r, version, candidate, err
	}
	input := ExecutionInput{
		Contract:   e.st.Approval.ContractHash,
		Cycle:      e.st.Cycle,
		Round:      r.Round,
		Ticket:     ticket,
		Repository: state,
		Scope:      e.scope,
		Plan:       e.plan,
	}
	for _, dep := range ticket.DependsOn {
		rec, err := e.store.GetTicket(e.st.Cycle, dep)
		if err != nil {
			return r, version, candidate, err
		}
		approved, err := e.approved(dep)
		if err != nil {
			return r, version, candidate, err
		}
		input.Dependencies = append(input.Dependencies, Dependency{Ticket: dep, Status: rec.Status, Outputs: approved.Outputs})
	}
	if input.Preimages, err = readPreimages(e.backend, ticket); err != nil {
		return r, version, candidate, err
	}
	if input.Governing, err = readGoverning(e.backend, ticket); err != nil {
		return r, version, candidate, err
	}
	if input.Repair, err = e.repairContext(r); err != nil {
		return r, version, candidate, err
	}

	r.Expected = state.Baseline()
	e.r.ticket = ticket.ID
	selection, err := e.selectSupporting(ctx, ticket, input)
	if err != nil {
		return r, version, candidate, err
	}
	declared := map[string]bool{}
	for _, output := range ticket.Outputs {
		declared[output] = true
	}
	input.Supporting = readSupporting(e.backend, selection, declared)

	observations, err := e.store.Observations(e.st.Cycle)
	if err != nil {
		return r, version, candidate, err
	}
	input.Observations = observations
	encoded, err := input.Encode()
	if err != nil {
		return r, version, candidate, err
	}
	inputRef, err := e.publish(r, "input", "application/json", encoded)
	if err != nil {
		return r, version, candidate, err
	}
	r.Inputs = []workflow.ArtifactRef{inputRef}
	if version, err = e.store.CommitRound(r, version, workflow.Event{Type: "round.input_recorded"}); err != nil {
		return r, version, candidate, err
	}

	if err = e.r.guard(); err != nil {
		return r, version, candidate, err
	}
	reply, err := e.r.Run(ctx, ticket.Owner, executionMessage(input))
	if err != nil {
		return r, version, candidate, err
	}
	responseRef, err := e.publish(r, "response", "text/markdown; charset=utf-8", []byte(reply))
	if err != nil {
		return r, version, candidate, err
	}
	r.Response = &responseRef
	if version, err = e.store.CommitRound(r, version, workflow.Event{Type: "round.response_recorded"}); err != nil {
		return r, version, candidate, err
	}
	return e.decodeResponse(ticket, r, version, reply)
}

func (e *execution) decodeResponse(ticket workflow.Ticket, r workflow.ExecutionRound, version int64, reply string) (workflow.ExecutionRound, int64, workflow.Candidate, error) {
	var candidate workflow.Candidate
	rec, err := e.store.GetTicket(e.st.Cycle, ticket.ID)
	if err != nil {
		return r, version, candidate, err
	}
	if rec.Status == workflow.TicketClaimed {
		if _, err = e.store.ApplyTicketStatus(e.st.Cycle, ticket.ID, workflow.TicketResponseRecorded, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "response.recorded"}); err != nil {
			return r, version, candidate, err
		}
	}

	candidate, err = workflow.DecodeCandidate(strings.TrimSpace(reply), ticket)
	if err != nil {
		return r, version, candidate, e.reject(r, version, repair("task %s output: %v", ticket.ID, err))
	}
	for _, f := range candidate.Files {
		if err = e.target.CheckPath(f.Path); err != nil {
			return r, version, candidate, e.reject(r, version, repair("task %s output: %v", ticket.ID, err))
		}
	}
	normalized, err := json.Marshal(candidate)
	if err != nil {
		return r, version, candidate, err
	}
	candidateRef, err := e.publish(r, "candidate", "application/json", normalized)
	if err != nil {
		return r, version, candidate, err
	}
	r.Candidate, r.Explanation, r.State = &candidateRef, candidate.Explanation, workflow.RoundCandidate
	if version, err = e.store.CommitRound(r, version, workflow.Event{Type: "round.candidate_validated"}); err != nil {
		return r, version, candidate, err
	}
	return r, version, candidate, nil
}

// selectSupporting runs the existing two-pass file selection: the model sees
// the repository index and names the files it needs, and those are then
// supplied complete. It is bounded by the configured maximum, and a round
// repairing a previous failure skips it — the failure output and the current
// diff are the context that matters then.
func (e *execution) selectSupporting(ctx context.Context, ticket workflow.Ticket, input ExecutionInput) ([]string, error) {
	if input.Repair != nil {
		return nil, nil
	}
	var task strings.Builder
	fmt.Fprintf(&task, "Vas a implementar la tarea %s del plan aprobado, en el repositorio real.\n\nTítulo: %s\nDescripción: %s\nSalidas declaradas:\n", ticket.ID, ticket.Title, ticket.Description)
	for _, output := range ticket.Outputs {
		fmt.Fprintf(&task, "- %s\n", output)
	}
	task.WriteString("Criterios de aceptación:\n")
	for _, c := range ticket.Criteria {
		fmt.Fprintf(&task, "- %s\n", c)
	}
	task.WriteString("\nYa recibirás completas las salidas declaradas y los SPEC.md que las gobiernan; pide solo código de apoyo adicional.")
	return e.r.selectFiles(ctx, ticket.Owner, task.String())
}

// repairContext carries the previous round's actual diff and bounded failure
// output. The diff comes from the executor's own inspection of the checkout,
// not from what the model said it did.
func (e *execution) repairContext(r workflow.ExecutionRound) (*Repair, error) {
	if r.Round <= 1 {
		return nil, nil
	}
	previous, _, found, err := e.store.GetRound(e.st.Cycle, r.Contract, r.Ticket, r.TicketRevision, r.Round-1)
	if err != nil || !found {
		return nil, err
	}
	out := &Repair{Round: previous.Round}
	if previous.Failure != "" {
		out.Failures = append(out.Failures, previous.Failure)
	}
	for _, c := range previous.Checks {
		if c.Passed {
			continue
		}
		data, readErr := e.artifacts.Read(e.store, e.st.Cycle, c.Evidence.ID)
		if readErr != nil {
			return nil, readErr
		}
		var result executor.CheckResult
		if err = json.Unmarshal(data, &result); err != nil {
			return nil, err
		}
		out.Failures = append(out.Failures, fmt.Sprintf("check %s exited %d:\n%s", c.CheckID, result.ExitCode, boundedTail(result.Output, maxFailureBytes)))
	}
	diff, err := e.backend.Inspect()
	if err != nil {
		return nil, err
	}
	out.Diff = renderDiff(diff)
	return out, nil
}

// apply turns the validated candidate into edits against the exact current
// source and applies them through the executor, which re-checks every
// precondition itself and rolls back anything partial.
func (e *execution) apply(ticket workflow.Ticket, r workflow.ExecutionRound, version int64, candidate workflow.Candidate) (workflow.ExecutionRound, int64, error) {
	if err := e.r.guard(); err != nil {
		return r, version, err
	}
	before, err := e.backend.State()
	if err != nil {
		return r, version, err
	}
	if r.Expected != "" && r.Expected != before.Baseline() {
		return r, version, errors.New("repository state changed since this candidate was generated; nothing was applied")
	}
	r.Expected = before.Baseline()

	var edits []executor.Edit
	for _, f := range candidate.Files {
		current, err := e.backend.Read(f.Path)
		if err != nil {
			return r, version, err
		}
		switch f.Operation {
		case workflow.CandidateUnchanged:
			continue
		case workflow.CandidateDelete:
			if current == nil {
				return r, version, e.reject(r, version, repair("task %s output: candidate deletes %s, which does not exist", ticket.ID, f.Path))
			}
			edits = append(edits, executor.Edit{Path: f.Path, Before: current, After: nil})
		case workflow.CandidateSource:
			next := &executor.File{Data: []byte(*f.Source), Mode: f.FileMode()}
			if current != nil && string(current.Data) == string(next.Data) && current.Mode == next.Mode {
				continue // this file is already exactly what the candidate asks for
			}
			edits = append(edits, executor.Edit{Path: f.Path, Before: current, After: next})
		}
	}
	if len(edits) == 0 {
		return r, version, e.reject(r, version, repair("task %s output: the candidate changes nothing in the checkout; report %q with an explanation instead", ticket.ID, workflow.CandidateNoChange))
	}

	receipt, err := e.backend.Apply(ticket.ID, edits)
	if err != nil {
		// The executor already rolled back anything partial and reconciled the
		// recorded state; a rejected patch is a repairable round, an I/O or
		// reconciliation failure is not.
		if receipt.Patch.ID == "" {
			return r, version, e.reject(r, version, repair("task %s: patch refused: %v", ticket.ID, err))
		}
		return r, version, err
	}
	after, err := e.backend.State()
	if err != nil {
		return r, version, err
	}
	if after.Baseline() != receipt.After {
		return r, version, errors.New("applied state does not match the confirmed patch receipt")
	}
	changed := changedPaths(before, after)
	declared := map[string]bool{}
	for _, output := range ticket.Outputs {
		declared[output] = true
	}
	for _, path := range changed {
		if !declared[path] {
			return r, version, fmt.Errorf("applied diff touches %s, which %s did not declare", path, ticket.ID)
		}
	}
	diff, err := e.backend.Inspect()
	if err != nil {
		return r, version, err
	}
	rendered, err := json.Marshal(diff)
	if err != nil {
		return r, version, err
	}
	diffRef, err := e.publish(r, "diff", "application/json", rendered)
	if err != nil {
		return r, version, err
	}
	r.Patch, r.Diff, r.Confirmed, r.State = &receipt.Patch, &diffRef, after.Baseline(), workflow.RoundApplied
	if version, err = e.store.CommitRound(r, version, workflow.Event{Type: "round.patch_confirmed", Payload: after.Baseline()}); err != nil {
		return r, version, err
	}
	fmt.Fprintf(os.Stderr, "  %s applied %d file(s) to %s\n", ticket.ID, len(edits), e.target.Root)
	return r, version, nil
}

// check runs every approved check, in the approved order, against the applied
// state. A failure leaves the diff exactly where it is: the point of the
// recorded diff is that a human (or a repair round) can see what failed.
func (e *execution) check(ctx context.Context, ticket workflow.Ticket, r workflow.ExecutionRound, version int64) (workflow.ExecutionRound, int64, error) {
	r.Checks = nil
	for _, c := range e.contract.Policy.Checks {
		if err := ctx.Err(); err != nil {
			return r, version, err
		}
		if err := e.r.guard(); err != nil {
			return r, version, err
		}
		if err := e.store.Heartbeat(e.st.Cycle, ticket.ID, e.holder, e.ttl); err != nil {
			return r, version, err
		}
		fmt.Fprintf(os.Stderr, "  %s check %s…\n", ticket.ID, c.ID)
		result, runErr := e.backend.Check(ctx, c.ID)
		if result.ID == "" {
			// The check never started: a configuration or environment refusal,
			// not a verdict about the code.
			return r, version, fmt.Errorf("check %s could not run: %w", c.ID, runErr)
		}
		record := workflow.CheckRecord{
			CheckID:  c.ID,
			RunID:    result.ID,
			Evidence: result.Evidence,
			ExitCode: result.ExitCode,
			Tested:   result.Before,
			Passed:   runErr == nil && result.ExitCode == 0 && !result.TimedOut && !result.Truncated,
		}
		if runErr != nil {
			record.Failure = runErr.Error()
		}
		if record.Passed {
			if failure := verifyTestEvidence(c, result); failure != "" {
				record.Passed, record.Failure = false, failure
			}
		}
		r.Checks = append(r.Checks, record)
		if record.Evidence.ID == "" {
			// A started check with no published result is the one case that
			// must never be retried or interpreted: its process may still be
			// running. The executor refuses further work until an operator
			// reconciles it.
			return r, version, fmt.Errorf("check %s produced no durable evidence: %w", c.ID, runErr)
		}
		if !record.Passed {
			if unknownOutcome(result, runErr) {
				// Not a verdict about the code: a timeout, a cancellation,
				// truncated evidence or a check that wrote to the repository.
				// The round keeps its confirmed patch and stays where it is,
				// so a later run re-runs the checks rather than regenerating a
				// patch that is already applied — and nothing proceeds until a
				// person has reconciled whatever produced this.
				r.Failure = record.Failure
				if _, err := e.store.CommitRound(r, version, workflow.Event{Type: "round.check_unknown", Payload: record.Failure}); err != nil {
					return r, version, err
				}
				return r, version, fmt.Errorf("check %s did not produce a trustworthy outcome (%s); reconcile it before further execution", c.ID, record.Failure)
			}
			// The round keeps every check record it collected, including this
			// failure, and the applied diff stays in the checkout for repair.
			return r, version, e.rejectChecked(r, version, record)
		}
	}
	if len(r.Checks) == 0 {
		return r, version, errors.New("the approved contract has no checks; an unchecked change cannot be implemented")
	}
	// An earlier untrustworthy outcome stays in the event log, not on a round
	// that has since passed every check against this exact state.
	r.State, r.Failure = workflow.RoundChecked, ""
	var err error
	if version, err = e.store.CommitRound(r, version, workflow.Event{Type: "round.checked"}); err != nil {
		return r, version, err
	}
	return r, version, nil
}

// verifyTestEvidence applies approved generic output rules and retains the Go
// adapter's test-result checks. Output matching relies on trusted check programs.
func verifyTestEvidence(c workflow.Check, result executor.CheckResult) string {
	if failure := workflow.CheckOutputFailure(c, result.Output); failure != "" {
		return failure
	}
	if len(c.Args) < 2 || c.Args[0] != "go" || c.Args[1] != "test" {
		return ""
	}
	if c.RequiresPostgres && !result.DatabaseEnabled {
		return "test check ran without a disposable PostgreSQL URL; skipped database tests cannot prove acceptance"
	}
	packages, skips := 0, 0
	for _, line := range strings.Split(result.Output, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "ok "), strings.HasPrefix(trimmed, "--- PASS"), trimmed == "PASS":
			packages++
		case strings.HasPrefix(trimmed, "--- SKIP"), strings.HasPrefix(trimmed, "=== SKIP"):
			skips++
		case strings.Contains(trimmed, "DATABASE TESTS SKIPPED"):
			skips++
		case strings.HasPrefix(trimmed, "FAIL"), strings.HasPrefix(trimmed, "--- FAIL"):
			return "test output reports a failure despite a zero exit code"
		}
	}
	if skips > 0 {
		return fmt.Sprintf("%d skipped test(s) in the approved check; a skipped test is not a passing test", skips)
	}
	if packages == 0 {
		return "the approved test check executed no tests"
	}
	return ""
}

// unknownOutcome distinguishes "the code is wrong" from "we do not know what
// happened". A timeout, a cancellation, truncated evidence or a check that
// wrote to the repository are all the second kind, and none of them may be
// answered by generating a different patch.
func unknownOutcome(result executor.CheckResult, err error) bool {
	if result.TimedOut || result.Truncated {
		return true
	}
	if result.After != result.Before {
		return true
	}
	if result.ExitCode < 0 {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// reject records a repairable failure: the round is closed as failed and the
// ticket withdraws its candidate claim, while every artifact it produced stays
// exactly where it is.
func (e *execution) reject(r workflow.ExecutionRound, version int64, cause error) error {
	r.Failure, r.State = cause.Error(), workflow.RoundFailed
	if _, err := e.store.CommitRound(r, version, workflow.Event{Type: "round.failed", Payload: r.Failure}); err != nil {
		return err
	}
	rec, err := e.store.GetTicket(e.st.Cycle, r.Ticket)
	if err != nil {
		return err
	}
	if rec.Status == workflow.TicketResponseRecorded || rec.Status == workflow.TicketCandidateReady {
		if _, err = e.store.ApplyTicketStatus(e.st.Cycle, r.Ticket, workflow.TicketResponseRejected, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "response.rejected", Payload: r.Failure}); err != nil {
			return err
		}
	}
	return cause
}

func (e *execution) rejectChecked(r workflow.ExecutionRound, version int64, record workflow.CheckRecord) error {
	return e.reject(r, version, repair("check %s failed with exit %d; the applied diff is preserved for repair: %s", record.CheckID, record.ExitCode, record.Failure))
}

// readyCandidate walks a ticket whose durable candidate exists forward to
// candidate_ready through the permitted edges, without another model call.
func (e *execution) readyCandidate(r workflow.ExecutionRound) error {
	if r.Candidate == nil {
		return nil
	}
	rec, err := e.store.GetTicket(e.st.Cycle, r.Ticket)
	if err != nil {
		return err
	}
	if rec.Status == workflow.TicketCandidateReady {
		return nil
	}
	if rec.Status == workflow.TicketClaimed {
		if rec, err = e.store.ApplyTicketStatus(e.st.Cycle, r.Ticket, workflow.TicketResponseRecorded, workflow.ActorEngine, rec.StateVersion, workflow.Event{Type: "response.recorded"}); err != nil {
			return err
		}
	}
	if rec.Status != workflow.TicketResponseRecorded {
		return fmt.Errorf("ticket %s is %q; a recorded candidate cannot be adopted from there", r.Ticket, rec.Status)
	}
	return e.store.RecordCandidate(e.st.Cycle, r.Ticket, r.Candidate.ID, rec.StateVersion)
}

func (e *execution) finishImplemented(r workflow.ExecutionRound, version int64, candidate workflow.Candidate) error {
	manifest, err := e.manifest(r, candidate, workflow.RoundImplemented)
	if err != nil {
		return err
	}
	r.Manifest = &manifest
	rec, err := e.store.GetTicket(e.st.Cycle, r.Ticket)
	if err != nil {
		return err
	}
	if err = e.store.MarkImplemented(r, version, rec.StateVersion); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  %s implemented (tested state %s)\n", r.Ticket, short(r.Confirmed))
	return nil
}

func (e *execution) finishNoChange(r workflow.ExecutionRound, version int64, candidate workflow.Candidate) error {
	state, err := e.backend.State()
	if err != nil {
		return err
	}
	if r.Expected != "" && state.Baseline() != r.Expected {
		return errors.New("a no-change outcome cannot be recorded against a changed repository")
	}
	r.Confirmed, r.Explanation = state.Baseline(), candidate.Explanation
	manifest, err := e.manifest(r, candidate, workflow.RoundNoChange)
	if err != nil {
		return err
	}
	r.Manifest = &manifest
	rec, err := e.store.GetTicket(e.st.Cycle, r.Ticket)
	if err != nil {
		return err
	}
	if err = e.store.MarkNoChange(r, version, rec.StateVersion); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  %s reported no change; it satisfies no dependency and implements nothing\n", r.Ticket)
	return nil
}

func (e *execution) manifest(r workflow.ExecutionRound, candidate workflow.Candidate, outcome string) (workflow.ArtifactRef, error) {
	m := ImplementationManifest{
		Schema: "1", Cycle: e.st.Cycle, Contract: r.Contract, Approval: e.st.Approval,
		Ticket: r.Ticket, TicketRevision: r.TicketRevision, Round: r.Round,
		Outcome: outcome, Explanation: candidate.Explanation,
		Baseline: e.backend.Base().Baseline(), Expected: r.Expected, Confirmed: r.Confirmed,
		Inputs: r.Inputs, Response: r.Response, Candidate: r.Candidate, Patch: r.Patch, Diff: r.Diff,
		Checks: r.Checks, RecordedAt: time.Now().UTC(),
	}
	for _, f := range candidate.Files {
		if f.Operation != workflow.CandidateUnchanged {
			m.Paths = append(m.Paths, f.Path)
		}
	}
	sort.Strings(m.Paths)
	data, err := json.Marshal(m)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	return e.publish(r, "manifest", "application/json", data)
}

// publish writes one round artifact. The content digest is part of both the
// reference and the path, so republishing identical bytes after an interrupted
// round is a benign replay while different bytes can never overwrite evidence
// something already points at.
func (e *execution) publish(r workflow.ExecutionRound, name, media string, content []byte) (workflow.ArtifactRef, error) {
	digest := workflow.Digest(string(content))
	ext := "json"
	if strings.HasPrefix(media, "text/markdown") {
		ext = "md"
	}
	ref := workflow.ArtifactRef{
		ID:      fmt.Sprintf("exec-%s-%s-r%d-%s-%s", short(r.Contract), r.Ticket, r.Round, name, short(digest)),
		Path:    fmt.Sprintf("cycles/%03d/execution/%s/%s/%d/%s-%s.%s", e.st.Cycle, r.Contract, r.Ticket, r.Round, name, short(digest), ext),
		Version: digest,
		Media:   media,
	}
	return e.artifacts.Publish(e.store, e.st.Cycle, ref, content)
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// changedPaths reports the backend paths whose content differs between two
// recorded snapshots, in a stable order.
func changedPaths(before, after workflow.RepositoryState) []string {
	seen := map[string]bool{}
	var out []string
	for path, hash := range after.Content {
		if before.Content[path] != hash && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	for path := range before.Content {
		if _, ok := after.Content[path]; !ok && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func renderDiff(d executor.Diff) string {
	var b strings.Builder
	for _, edit := range d.Edits {
		switch {
		case edit.Before == nil:
			fmt.Fprintf(&b, "\n### nuevo %s\n\n%s\n", edit.Path, string(edit.After.Data))
		case edit.After == nil:
			fmt.Fprintf(&b, "\n### borrado %s\n\n%s\n", edit.Path, string(edit.Before.Data))
		default:
			fmt.Fprintf(&b, "\n### modificado %s\n\n#### antes\n\n%s\n\n#### después\n\n%s\n", edit.Path, string(edit.Before.Data), string(edit.After.Data))
		}
	}
	if b.Len() == 0 {
		return "_(sin cambios en el checkout)_"
	}
	return b.String()
}

// executionMessage renders the durable input for the model. The JSON payload
// is the authority — it is what was published as evidence — and the prose
// around it only tells the model how to read and answer it.
func executionMessage(in ExecutionInput) string {
	var b strings.Builder
	b.WriteString("TAREA_DE_EJECUCION\n\n")
	b.WriteString("# Alcance y estado del producto\n\n")
	b.WriteString(in.Scope)
	b.WriteString("\n\n# Plan aprobado\n\n")
	b.WriteString(in.Plan)
	fmt.Fprintf(&b, "\n\n"+workflow.TicketHeading+"%s\n\nTítulo: %s\nDescripción: %s\nCriterios de aceptación:\n", in.Ticket.ID, in.Ticket.Title, in.Ticket.Description)
	for _, c := range in.Ticket.Criteria {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	b.WriteString("\n# Instrucciones del repositorio\n")
	for _, d := range in.Governing {
		if d.Omitted != "" {
			fmt.Fprintf(&b, "\n## %s\n_(%s)_\n", d.Path, d.Omitted)
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", d.Path, d.Source)
	}
	b.WriteString("\n# Contenido actual COMPLETO de cada salida declarada\n")
	for _, p := range in.Preimages {
		if !p.Exists {
			fmt.Fprintf(&b, "\n## %s\n_(no existe todavía; esta tarea está aprobada para crearlo)_\n", p.Path)
			continue
		}
		fmt.Fprintf(&b, "\n## %s (modo %s)\n\n```\n%s\n```\n", p.Path, p.Mode, *p.Source)
	}
	if len(in.Supporting) > 0 {
		b.WriteString("\n# Código de apoyo\n")
		for _, d := range in.Supporting {
			if d.Omitted != "" {
				fmt.Fprintf(&b, "\n## %s\n_(%s)_\n", d.Path, d.Omitted)
				continue
			}
			fmt.Fprintf(&b, "\n## %s\n\n```\n%s\n```\n", d.Path, d.Source)
		}
	}
	if len(in.Dependencies) > 0 {
		b.WriteString("\n# Tareas de las que depende (ya aplicadas en el checkout)\n")
		for _, d := range in.Dependencies {
			fmt.Fprintf(&b, "- %s (%s): %s\n", d.Ticket, d.Status, strings.Join(d.Outputs, ", "))
		}
	}
	if in.Repair != nil {
		fmt.Fprintf(&b, "\n# Intento anterior (ronda %d) rechazado\n\nEl diff que está AHORA MISMO en el checkout:\n%s\n\nPor qué se rechazó:\n", in.Repair.Round, in.Repair.Diff)
		for _, f := range in.Repair.Failures {
			fmt.Fprintf(&b, "\n```\n%s\n```\n", f)
		}
		b.WriteString("\nCorrige la causa real. El contenido de arriba ya incluye tu intento anterior aplicado.\n")
	}
	if len(in.Observations) > 0 {
		raw, _ := json.Marshal(in.Observations)
		b.WriteString("\n# Observaciones y respuestas humanas (no amplían el contrato)\n")
		b.Write(raw)
		b.WriteByte('\n')
	}
	// The checklist is repeated as a bare list because it is what the response
	// must answer one-for-one: every path here needs its own entry, and
	// nothing else may appear.
	b.WriteString(workflow.RenderDeclaredOutputs(in.Ticket.Outputs))
	b.WriteString("\n# Cómo entregar\n\n")
	b.WriteString(candidateInstructions)
	return b.String()
}
