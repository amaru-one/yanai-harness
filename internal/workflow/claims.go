package workflow

import (
	"database/sql"
	"errors"
	"time"
)

// Claim is a lease naming who is holding a ticket, and until when. It
// answers a question the workspace writer flock cannot: the flock tells a
// second process "someone is running right now"; a claim survives the first
// process's death and tells the *next* process "this ticket was left mid-
// work, by whom, until when" — which is what reconciliation needs to act on.
type Claim struct {
	Cycle       int
	TicketID    string
	Holder      string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
	ExpiresAt   time.Time
}

// ErrClaimHeld means a live, unexpired claim by a different holder exists.
var ErrClaimHeld = errors.New("ticket is already claimed by another holder")

// ClaimTicket atomically moves a ticket pending -> claimed and records the
// claim's lease, in one transaction: a crash between the two steps would
// otherwise leave a lease for a ticket whose status doesn't show it claimed,
// or a "claimed" ticket with no lease to expire.
func (s *Store) ClaimTicket(cycle int, ticketID, holder string, ttl time.Duration, evt Event) (TicketRecord, Claim, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return TicketRecord{}, Claim{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var revision int
	var from string
	var version int64
	if err := tx.QueryRow(`SELECT revision, status, state_version FROM workflow_tickets
		WHERE project = ? AND cycle = ? AND id = ? ORDER BY revision DESC LIMIT 1`, s.project, cycle, ticketID).
		Scan(&revision, &from, &version); err != nil {
		return TicketRecord{}, Claim{}, err
	}

	var existingHolder, expiresAt string
	claimErr := tx.QueryRow(`SELECT holder, expires_at FROM workflow_claims WHERE project = ? AND cycle = ? AND ticket_id = ?`,
		s.project, cycle, ticketID).Scan(&existingHolder, &expiresAt)
	nowT := time.Now().UTC()
	leaseLive := false
	switch {
	case claimErr == nil:
		if exp, perr := time.Parse(time.RFC3339Nano, expiresAt); perr == nil && nowT.Before(exp) {
			leaseLive = true
		}
	case !errors.Is(claimErr, sql.ErrNoRows):
		return TicketRecord{}, Claim{}, claimErr
	}

	// pending -> claimed is the ordinary path, gated by the transition
	// table like everything else. A ticket already showing "claimed" is
	// only claimable here if its lease has lapsed (or was never recorded) —
	// equivalent to ExpireClaims having freed it a moment earlier — and even
	// then the ticket's own status/version are left untouched: only the
	// lease changes hands, since the ticket never actually left "claimed".
	movesStatus := false
	switch {
	case from == TicketPending:
		if !actorAllowed(ticketTransitions, from, TicketClaimed, ActorEngine) {
			return TicketRecord{}, Claim{}, &ErrTransitionNotAllowed{From: from, To: TicketClaimed, Actor: ActorEngine}
		}
		movesStatus = true
	case from == TicketClaimed || from == TicketCandidateReady:
		// A candidate_ready ticket is still being worked on — Step 8 applies
		// it and runs its checks under this same lease — so it takes a lease
		// without moving status. Its status already records what happened;
		// only the right to keep working on it changes hands, and only once
		// the previous holder's lease has lapsed. The holder that already owns
		// the lease is renewing it, not competing for it.
		if leaseLive && existingHolder != holder {
			return TicketRecord{}, Claim{}, ErrClaimHeld
		}
	default:
		return TicketRecord{}, Claim{}, &ErrTransitionNotAllowed{From: from, To: TicketClaimed, Actor: ActorEngine}
	}

	claim := Claim{Cycle: cycle, TicketID: ticketID, Holder: holder, AcquiredAt: nowT, HeartbeatAt: nowT, ExpiresAt: nowT.Add(ttl)}
	acquired, expires := nowT.Format(time.RFC3339Nano), claim.ExpiresAt.Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO workflow_claims(project, cycle, ticket_id, holder, acquired_at, heartbeat_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project, cycle, ticket_id) DO UPDATE SET holder = excluded.holder, acquired_at = excluded.acquired_at, heartbeat_at = excluded.heartbeat_at, expires_at = excluded.expires_at`,
		s.project, cycle, ticketID, holder, acquired, acquired, expires); err != nil {
		return TicketRecord{}, Claim{}, err
	}
	if movesStatus {
		res, err := tx.Exec(`UPDATE workflow_tickets SET status = ?, state_version = state_version + 1, updated_at = ?
			WHERE project = ? AND cycle = ? AND id = ? AND revision = ? AND state_version = ?`,
			TicketClaimed, acquired, s.project, cycle, ticketID, revision, version)
		if err != nil {
			return TicketRecord{}, Claim{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return TicketRecord{}, Claim{}, &ErrStaleVersion{Kind: "ticket", Expected: version}
		}
	}
	evt.Cycle, evt.TicketID, evt.Actor = cycle, ticketID, ActorEngine
	if evt.Type == "" {
		evt.Type = "ticket.claimed"
	}
	if err := s.appendEventTx(tx, evt); err != nil {
		return TicketRecord{}, Claim{}, err
	}
	if err := tx.Commit(); err != nil {
		return TicketRecord{}, Claim{}, err
	}
	rec, err := s.GetTicket(cycle, ticketID)
	return rec, claim, err
}

// Heartbeat extends holder's lease on ticketID. Callers refresh it around
// each model call in a role turn, so a long-running but healthy attempt
// isn't mistaken for an abandoned one.
func (s *Store) Heartbeat(cycle int, ticketID, holder string, ttl time.Duration) error {
	nowT := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE workflow_claims SET heartbeat_at = ?, expires_at = ?
		WHERE project = ? AND cycle = ? AND ticket_id = ? AND holder = ?`,
		nowT.Format(time.RFC3339Nano), nowT.Add(ttl).Format(time.RFC3339Nano), s.project, cycle, ticketID, holder)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("no live claim for this ticket held by this holder")
	}
	return nil
}

// ReleaseClaim drops holder's lease on ticketID. It does not change ticket
// status; callers pair it with an ApplyTicketStatus call of their own.
func (s *Store) ReleaseClaim(cycle int, ticketID, holder string) error {
	_, err := s.db.Exec(`DELETE FROM workflow_claims WHERE project = ? AND cycle = ? AND ticket_id = ? AND holder = ?`,
		s.project, cycle, ticketID, holder)
	return err
}

// ExpireClaims moves every ticket whose lease has lapsed back to pending,
// recording claim.expired for each, and returns the tickets it touched. This
// is the only path from claimed back to pending other than an explicit
// release — it is what stops a dead process's claim from blocking the
// ticket forever.
func (s *Store) ExpireClaims() ([]TicketRecord, error) {
	rows, err := s.db.Query(`SELECT cycle, ticket_id FROM workflow_claims WHERE project = ? AND expires_at < ?`, s.project, now())
	if err != nil {
		return nil, err
	}
	type key struct {
		cycle int
		id    string
	}
	var expired []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.cycle, &k.id); err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		expired = append(expired, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return nil, err
	}
	rows.Close() //nolint:errcheck

	var out []TicketRecord
	for _, k := range expired {
		rec, expiredOK, err := s.expireOneClaim(k.cycle, k.id)
		if err != nil {
			return out, err
		}
		if expiredOK {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *Store) expireOneClaim(cycle int, ticketID string) (TicketRecord, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return TicketRecord{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	var revision int
	var status string
	var version int64
	if err := tx.QueryRow(`SELECT revision, status, state_version FROM workflow_tickets
		WHERE project = ? AND cycle = ? AND id = ? ORDER BY revision DESC LIMIT 1`, s.project, cycle, ticketID).
		Scan(&revision, &status, &version); err != nil {
		return TicketRecord{}, false, err
	}
	if status != TicketClaimed {
		// Released and moved on since ExpireClaims scanned; just drop the
		// stale lease row.
		if _, err := tx.Exec(`DELETE FROM workflow_claims WHERE project = ? AND cycle = ? AND ticket_id = ?`, s.project, cycle, ticketID); err != nil {
			return TicketRecord{}, false, err
		}
		return TicketRecord{}, false, tx.Commit()
	}
	res, err := tx.Exec(`UPDATE workflow_tickets SET status = ?, state_version = state_version + 1, updated_at = ?
		WHERE project = ? AND cycle = ? AND id = ? AND revision = ? AND state_version = ?`,
		TicketPending, now(), s.project, cycle, ticketID, revision, version)
	if err != nil {
		return TicketRecord{}, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return TicketRecord{}, false, &ErrStaleVersion{Kind: "ticket", Expected: version}
	}
	if _, err := tx.Exec(`DELETE FROM workflow_claims WHERE project = ? AND cycle = ? AND ticket_id = ?`, s.project, cycle, ticketID); err != nil {
		return TicketRecord{}, false, err
	}
	if err := s.appendEventTx(tx, Event{Cycle: cycle, TicketID: ticketID, Actor: ActorEngine, Type: "claim.expired"}); err != nil {
		return TicketRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return TicketRecord{}, false, err
	}
	rec, err := s.GetTicket(cycle, ticketID)
	return rec, true, err
}
