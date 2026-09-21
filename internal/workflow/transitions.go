package workflow

import "fmt"

// Actors a transition may be attributed to. A role ID (product-owner,
// ingeniero, ...) is never one of these: the model's text is an input a
// transition is requested with, never the actor performing it. Only a human
// or the engine itself moves state.
const (
	ActorHuman  = "human"
	ActorEngine = "engine"
)

// Cycle phases. These are workflow-owned so the transition table has a
// single home; the CLI's human-readable projection (internal/ws) mirrors
// them as its own constants, matched by a cross-package test.
const (
	PhaseNoCycle           = "no_cycle"
	PhaseAnalyzed          = "analyzed"
	PhaseNoChangeNeeded    = "no_change_needed"
	PhaseNeedsEvidence     = "needs_evidence"
	PhaseOutOfScope        = "out_of_scope"
	PhaseBlockedByBaseline = "blocked_by_baseline"
	PhaseAwaitingApproval  = "awaiting_approval"
	PhaseApproved          = "approved"
	PhaseRejected          = "rejected"
	PhaseAwaitingExecution = "awaiting_execution"
)

// TerminalPhases close a cycle without a plan. Distinguishing them from a
// change verdict is the whole point of Step 4; the transition table treats
// them uniformly here because getting to any of them is an engine act
// either way.
var TerminalPhases = []string{PhaseNoChangeNeeded, PhaseNeedsEvidence, PhaseOutOfScope, PhaseBlockedByBaseline}

// Ticket statuses. "done" is deliberately absent: a model response is
// recorded, then validated into a candidate — neither claim is "finished".
// Step 8 adds `implemented`; Step 9 adds `verified`. Nothing here reaches
// either name before its meaning is enforced.
const (
	TicketPending          = "pending"
	TicketClaimed          = "claimed"
	TicketResponseRecorded = "response_recorded"
	TicketCandidateReady   = "candidate_ready"
	TicketResponseRejected = "response_rejected"
	TicketLegacyUnverified = "legacy_unverified"
)

type edge struct{ from, to string }

// cycleTransitions and ticketTransitions are the only ways a phase or status
// may change, each mapped to the actors permitted to request it. Apply
// refuses any pair absent from the table, and refuses a present pair
// requested by an actor not listed for it — this is how "an LLM response
// cannot directly assign a successful terminal state" and "approval/
// rejection is a human act" are enforced structurally, once, here, rather
// than by scattered checks at each call site.
var cycleTransitions = map[edge][]string{}

func init() {
	allow := func(from string, to []string, actors ...string) {
		for _, t := range to {
			cycleTransitions[edge{from, t}] = actors
		}
	}
	// Analyze's outcome: a fresh cycle either opens for discussion or closes
	// on one of the four non-change findings.
	allow(PhaseNoCycle, append([]string{PhaseAnalyzed}, TerminalPhases...), ActorEngine)
	// Discuss's outcome, from a first pass or after a human's rejection: the
	// consolidated plan either awaits the human, or the team's second look
	// still finds no proposable change.
	allow(PhaseAnalyzed, append([]string{PhaseAwaitingApproval}, TerminalPhases...), ActorEngine)
	allow(PhaseRejected, append([]string{PhaseAwaitingApproval}, TerminalPhases...), ActorEngine)
	// The human gate. Nothing else may reach either side of it.
	allow(PhaseAwaitingApproval, []string{PhaseApproved, PhaseRejected}, ActorHuman)
	allow(PhaseApproved, []string{PhaseRejected}, ActorHuman)
	allow(PhaseAwaitingExecution, []string{PhaseRejected}, ActorHuman)
	// Execute's outcome, once every ticket has left "pending".
	allow(PhaseApproved, []string{PhaseAwaitingExecution}, ActorEngine)
}

var ticketTransitions = map[edge][]string{
	{TicketPending, TicketClaimed}:                   {ActorEngine},
	{TicketClaimed, TicketResponseRecorded}:          {ActorEngine},
	{TicketResponseRecorded, TicketCandidateReady}:   {ActorEngine},
	{TicketResponseRecorded, TicketResponseRejected}: {ActorEngine},
	{TicketClaimed, TicketPending}:                   {ActorEngine}, // claim expiry only
	{TicketResponseRejected, TicketPending}:          {ActorEngine}, // a new run retries
	// TicketLegacyUnverified has no outgoing edges at all: an imported
	// file-era ticket can never be promoted by any transition.
}

func allowedActors(table map[edge][]string, from, to string) []string {
	return table[edge{from, to}]
}

func actorAllowed(table map[edge][]string, from, to, actor string) bool {
	for _, a := range allowedActors(table, from, to) {
		if a == actor {
			return true
		}
	}
	return false
}

// ErrTransitionNotAllowed is returned when no rule permits from->to for actor,
// whether because the pair doesn't exist at all or because actor isn't among
// the ones allowed to request it.
type ErrTransitionNotAllowed struct{ From, To, Actor string }

func (e *ErrTransitionNotAllowed) Error() string {
	return fmt.Sprintf("transition %s -> %s is not permitted for actor %q", e.From, e.To, e.Actor)
}

// ErrStaleVersion means the row's state_version no longer matches what the
// caller read: something else committed a transition first. It is never a
// silent overwrite.
type ErrStaleVersion struct {
	Kind     string // "cycle" | "ticket"
	Expected int64
}

func (e *ErrStaleVersion) Error() string {
	return fmt.Sprintf("%s state changed since it was read (expected version %d); reload and retry", e.Kind, e.Expected)
}

// ErrLegacyRecord means the row was written by ImportLegacyCycle or
// ImportLegacyTicket, not by a live transition, and no mutating method will
// ever act on it — regardless of what its phase or status happens to say.
type ErrLegacyRecord struct {
	Kind string // "cycle" | "ticket"
	ID   string
}

func (e *ErrLegacyRecord) Error() string {
	return fmt.Sprintf("%s %s is an imported legacy record and cannot be transitioned", e.Kind, e.ID)
}
