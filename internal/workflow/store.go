// Package workflow's Store is the sole durable authority for cycle, ticket,
// claim, attempt and event state. cmd/yanai and internal/team read and
// mutate through it exclusively (Step 5); cycles/NNN/state.json becomes a
// generated, labelled projection of what is read here, never the other way
// around.
package workflow

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps one project's workflow database. Every row it reads or writes
// is scoped to Project — see bindProject — so a workspace copied or renamed
// onto another project's database is refused rather than silently mixing
// state.
type Store struct {
	db      *sql.DB
	path    string
	project string
}

// OpenStore opens (creating and migrating if needed) the database at path
// for the given project identifier.
func OpenStore(path, project string) (*Store, error) {
	if strings.TrimSpace(project) == "" {
		return nil, errors.New("workflow store requires a non-empty project identifier")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite serializes writers regardless, and this makes
	// that serialization happen predictably inside this process rather than
	// racing the driver's pool against itself.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=10000;`); err != nil {
		db.Close()
		return nil, err
	}
	if err := applyMigrations(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path, project: project}
	if err := s.bindProject(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Project returns the identifier this store is bound to.
func (s *Store) Project() string { return s.project }

func (s *Store) bindProject() error {
	var existing string
	err := s.db.QueryRow(`SELECT value FROM workflow_meta WHERE key = 'project'`).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err := s.db.Exec(`INSERT INTO workflow_meta(key, value) VALUES ('project', ?)`, s.project)
		return err
	case err != nil:
		return err
	case existing != s.project:
		return fmt.Errorf("workflow store %s belongs to project %q, not %q: a workspace must not be copied or renamed onto another project's store", s.path, existing, s.project)
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func newID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b[:]))
}

// appendEventTx inserts e within the caller's transaction, filling ID,
// CreatedAt and IdempotencyKey when the caller left them blank. It is the
// only way any table's row is committed alongside its event, which is what
// keeps the two from drifting apart.
func (s *Store) appendEventTx(tx *sql.Tx, e Event) error {
	if e.Type == "" {
		return errors.New("event requires a type")
	}
	if e.ID == "" {
		e.ID = newID("evt")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	key := e.IdempotencyKey
	if key == "" {
		key = fmt.Sprintf("%s/%d/%s/%s/%s", s.project, e.Cycle, e.Type, e.TicketID, e.ID)
	}
	_, err := tx.Exec(`INSERT INTO workflow_events(id, project, cycle, ticket_id, actor, type, idempotency_key, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, s.project, e.Cycle, e.TicketID, e.Actor, e.Type, key, e.Payload, e.CreatedAt.Format(time.RFC3339Nano))
	return err
}

// AppendEvent commits e on its own, outside any other transition. Duplicate
// IdempotencyKey values are rejected by the column's UNIQUE constraint.
func (s *Store) AppendEvent(e Event) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := s.appendEventTx(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- Cycles ----

// CycleRecord is a cycle's durable row. Intake/Scope/Proposal/Plan live in
// Payload as caller-defined JSON — Store does not know their shape, which is
// what keeps this package free of an import cycle with internal/ws.
type CycleRecord struct {
	Project      string
	Cycle        int
	Phase        string
	Verdict      string
	Origin       string
	BaseCommit   string
	PlanHash     string
	ScopeHash    string
	BaselineHash string
	Payload      string
	StateVersion int64
	// Legacy marks a row written by ImportLegacyCycle rather than
	// CreateCycle. Every mutating method refuses a legacy row outright,
	// regardless of what the transition table would otherwise allow for its
	// phase — see the v2 migration comment in schema.go for why that matters.
	Legacy bool
}

// CycleFields carries the columns a phase transition may also set. A blank
// field leaves the stored column unchanged — ApplyCyclePhase overlays these
// onto what's already there rather than blanking columns a given transition
// has nothing new to say about.
type CycleFields struct {
	Verdict, BaseCommit, PlanHash, ScopeHash, BaselineHash, Payload string
}

// CreateCycle inserts a new cycle at PhaseNoCycle, state_version 1: not yet
// analyzed, but reserved, so callers can commit intake and scope before the
// model is even asked (SetCyclePayload) and only later move it to its first
// real phase (ApplyCyclePhase). Splitting creation from that first phase
// change this way is what lets a crash between the two be recoverable rather
// than losing the cycle number or its early bookkeeping.
func (s *Store) CreateCycle(cycle int, origin string) (CycleRecord, error) {
	ts := now()
	if _, err := s.db.Exec(`INSERT INTO workflow_cycles(project, cycle, phase, origin, state_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)`, s.project, cycle, PhaseNoCycle, origin, ts, ts); err != nil {
		return CycleRecord{}, err
	}
	return s.GetCycle(cycle)
}

func (s *Store) GetCycle(cycle int) (CycleRecord, error) {
	r := CycleRecord{Project: s.project, Cycle: cycle}
	var legacy int
	err := s.db.QueryRow(`SELECT phase, verdict, origin, base_commit, plan_hash, scope_hash, baseline_hash, payload, state_version, legacy
		FROM workflow_cycles WHERE project = ? AND cycle = ?`, s.project, cycle).
		Scan(&r.Phase, &r.Verdict, &r.Origin, &r.BaseCommit, &r.PlanHash, &r.ScopeHash, &r.BaselineHash, &r.Payload, &r.StateVersion, &legacy)
	r.Legacy = legacy != 0
	return r, err
}

// ImportLegacyCycle inserts a cycle row directly at whatever phase/verdict a
// pre-store state.json recorded, marked Legacy so no mutating method will
// ever act on it — it is history being recorded, not a transition being
// requested, and phase strings here need not even be ones the transition
// table recognizes (an old cycle could be sitting at "sufficient" or
// "discussed", values that predate this step entirely).
func (s *Store) ImportLegacyCycle(cycle int, phase, verdict, origin, payload string) (CycleRecord, error) {
	ts := now()
	if _, err := s.db.Exec(`INSERT INTO workflow_cycles(project, cycle, phase, verdict, origin, payload, state_version, legacy, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, 1, ?, ?)`, s.project, cycle, phase, verdict, origin, payload, ts, ts); err != nil {
		return CycleRecord{}, err
	}
	return s.GetCycle(cycle)
}

// MaxCycle returns the highest cycle number recorded for this project, or 0
// if none exists yet.
func (s *Store) MaxCycle() (int, error) {
	var max sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(cycle) FROM workflow_cycles WHERE project = ?`, s.project).Scan(&max); err != nil {
		return 0, err
	}
	if !max.Valid {
		return 0, nil
	}
	return int(max.Int64), nil
}

// ListCycles returns every cycle recorded for this project, oldest first.
func (s *Store) ListCycles() ([]CycleRecord, error) {
	rows, err := s.db.Query(`SELECT cycle FROM workflow_cycles WHERE project = ? ORDER BY cycle`, s.project)
	if err != nil {
		return nil, err
	}
	var nums []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		nums = append(nums, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return nil, err
	}
	rows.Close() //nolint:errcheck

	out := make([]CycleRecord, 0, len(nums))
	for _, n := range nums {
		r, err := s.GetCycle(n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ApplyCyclePhase moves a cycle from its current phase to `to`, iff the
// stored state_version still equals expectedVersion and actor is permitted
// to make that move. The phase change, its field updates and its event are
// committed in one transaction.
func (s *Store) ApplyCyclePhase(cycle int, to, actor string, expectedVersion int64, fields CycleFields, evt Event) (CycleRecord, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CycleRecord{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var from, verdict, baseCommit, planHash, scopeHash, baselineHash, payload string
	var version int64
	var legacy int
	if err := tx.QueryRow(`SELECT phase, verdict, base_commit, plan_hash, scope_hash, baseline_hash, payload, state_version, legacy
		FROM workflow_cycles WHERE project = ? AND cycle = ?`, s.project, cycle).
		Scan(&from, &verdict, &baseCommit, &planHash, &scopeHash, &baselineHash, &payload, &version, &legacy); err != nil {
		return CycleRecord{}, err
	}
	if legacy != 0 {
		return CycleRecord{}, &ErrLegacyRecord{Kind: "cycle", ID: fmt.Sprintf("%d", cycle)}
	}
	if version != expectedVersion {
		return CycleRecord{}, &ErrStaleVersion{Kind: "cycle", Expected: expectedVersion}
	}
	if !actorAllowed(cycleTransitions, from, to, actor) {
		return CycleRecord{}, &ErrTransitionNotAllowed{From: from, To: to, Actor: actor}
	}
	verdict, baseCommit, planHash, scopeHash, baselineHash, payload = overlayCycleFields(fields, verdict, baseCommit, planHash, scopeHash, baselineHash, payload)
	res, err := tx.Exec(`UPDATE workflow_cycles SET phase = ?, verdict = ?, base_commit = ?, plan_hash = ?, scope_hash = ?, baseline_hash = ?, payload = ?, state_version = state_version + 1, updated_at = ?
		WHERE project = ? AND cycle = ? AND state_version = ?`,
		to, verdict, baseCommit, planHash, scopeHash, baselineHash, payload, now(), s.project, cycle, expectedVersion)
	if err != nil {
		return CycleRecord{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return CycleRecord{}, &ErrStaleVersion{Kind: "cycle", Expected: expectedVersion}
	}
	evt.Cycle, evt.Actor = cycle, actor
	if evt.Type == "" {
		evt.Type = "cycle.phase_changed:" + to
	}
	if err := s.appendEventTx(tx, evt); err != nil {
		return CycleRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return CycleRecord{}, err
	}
	return s.GetCycle(cycle)
}

func overlayCycleFields(fields CycleFields, verdict, baseCommit, planHash, scopeHash, baselineHash, payload string) (string, string, string, string, string, string) {
	if fields.Verdict != "" {
		verdict = fields.Verdict
	}
	if fields.BaseCommit != "" {
		baseCommit = fields.BaseCommit
	}
	if fields.PlanHash != "" {
		planHash = fields.PlanHash
	}
	if fields.ScopeHash != "" {
		scopeHash = fields.ScopeHash
	}
	if fields.BaselineHash != "" {
		baselineHash = fields.BaselineHash
	}
	if fields.Payload != "" {
		payload = fields.Payload
	}
	return verdict, baseCommit, planHash, scopeHash, baselineHash, payload
}

// SetCyclePayload updates a cycle's fields (verdict, hashes, payload)
// without changing its phase, under the same conditional-version rule as
// ApplyCyclePhase. It exists because not every commit is a phase change —
// recording intake before the model is even asked, or a task's staged
// output while the cycle stays "approved" — and the transition table has no
// entry for "stay put", nor should it need one just to let this case in.
func (s *Store) SetCyclePayload(cycle int, expectedVersion int64, actor string, fields CycleFields, evt Event) (CycleRecord, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CycleRecord{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var verdict, baseCommit, planHash, scopeHash, baselineHash, payload string
	var version int64
	var legacy int
	if err := tx.QueryRow(`SELECT verdict, base_commit, plan_hash, scope_hash, baseline_hash, payload, state_version, legacy
		FROM workflow_cycles WHERE project = ? AND cycle = ?`, s.project, cycle).
		Scan(&verdict, &baseCommit, &planHash, &scopeHash, &baselineHash, &payload, &version, &legacy); err != nil {
		return CycleRecord{}, err
	}
	if legacy != 0 {
		return CycleRecord{}, &ErrLegacyRecord{Kind: "cycle", ID: fmt.Sprintf("%d", cycle)}
	}
	if version != expectedVersion {
		return CycleRecord{}, &ErrStaleVersion{Kind: "cycle", Expected: expectedVersion}
	}
	verdict, baseCommit, planHash, scopeHash, baselineHash, payload = overlayCycleFields(fields, verdict, baseCommit, planHash, scopeHash, baselineHash, payload)
	res, err := tx.Exec(`UPDATE workflow_cycles SET verdict = ?, base_commit = ?, plan_hash = ?, scope_hash = ?, baseline_hash = ?, payload = ?, state_version = state_version + 1, updated_at = ?
		WHERE project = ? AND cycle = ? AND state_version = ?`,
		verdict, baseCommit, planHash, scopeHash, baselineHash, payload, now(), s.project, cycle, expectedVersion)
	if err != nil {
		return CycleRecord{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return CycleRecord{}, &ErrStaleVersion{Kind: "cycle", Expected: expectedVersion}
	}
	evt.Cycle, evt.Actor = cycle, actor
	if evt.Type == "" {
		evt.Type = "cycle.updated"
	}
	if err := s.appendEventTx(tx, evt); err != nil {
		return CycleRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return CycleRecord{}, err
	}
	return s.GetCycle(cycle)
}

// ---- Tickets ----

// TicketRecord is a ticket's durable row. Payload holds the caller's
// marshalled Ticket for the same reason CycleRecord.Payload does.
type TicketRecord struct {
	Project      string
	Cycle        int
	ID           string
	Revision     int
	Owner        string
	Status       string
	Payload      string
	StateVersion int64
	// Legacy marks a row written by ImportLegacyTicket. Its status is
	// always TicketLegacyUnverified, which already has no outgoing edge in
	// ticketTransitions — this field is recorded alongside for the same
	// defense-in-depth reason CycleRecord.Legacy exists, and so a caller can
	// tell a genuinely-unverified imported ticket from one that just
	// happens to be freshly pending.
	Legacy bool
}

// SaveTicket inserts a new ticket at revision 1 (or t.Revision, if the
// caller set one), status pending, state_version 1.
func (s *Store) SaveTicket(cycle int, t Ticket) (TicketRecord, error) {
	if strings.TrimSpace(t.ID) == "" {
		return TicketRecord{}, errors.New("ticket requires a non-empty id")
	}
	revision := t.Revision
	if revision == 0 {
		revision = 1
	}
	status := t.Status
	if status == "" {
		status = TicketPending
	}
	payload, err := json.Marshal(t)
	if err != nil {
		return TicketRecord{}, err
	}
	// Idempotent replay: a plan re-consolidated from the exact same input
	// (analyze/discuss's own idempotent-command handling, or simply retrying
	// after a crash between saving tickets and committing the cycle phase)
	// re-submits the same ticket unchanged. That returns the existing row
	// rather than colliding on the primary key. A *different* payload under
	// the same (cycle, id, revision) is a genuine conflict — the caller
	// should have deleted the old tickets first (DeleteTickets) when
	// consolidating a revised plan.
	if existing, err := s.GetTicket(cycle, t.ID); err == nil {
		if existing.Revision == revision && existing.Payload == string(payload) {
			return existing, nil
		}
		if existing.Revision == revision {
			return TicketRecord{}, fmt.Errorf("ticket %s revision %d already exists with different content", t.ID, revision)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return TicketRecord{}, err
	}
	ts := now()
	if _, err := s.db.Exec(`INSERT INTO workflow_tickets(project, cycle, id, revision, owner, status, payload, state_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		s.project, cycle, t.ID, revision, t.Owner, status, string(payload), ts, ts); err != nil {
		return TicketRecord{}, err
	}
	return s.GetTicket(cycle, t.ID)
}

// GetTicket returns a ticket's latest revision.
func (s *Store) GetTicket(cycle int, id string) (TicketRecord, error) {
	r := TicketRecord{Project: s.project, Cycle: cycle, ID: id}
	var legacy int
	err := s.db.QueryRow(`SELECT revision, owner, status, payload, state_version, legacy FROM workflow_tickets
		WHERE project = ? AND cycle = ? AND id = ? ORDER BY revision DESC LIMIT 1`, s.project, cycle, id).
		Scan(&r.Revision, &r.Owner, &r.Status, &r.Payload, &r.StateVersion, &legacy)
	r.Legacy = legacy != 0
	return r, err
}

// ImportLegacyTicket inserts a ticket row directly at TicketLegacyUnverified
// — never at whatever status the old state.json recorded, since that status
// vocabulary (including "done") no longer exists — marked Legacy so nothing
// can promote it. The original status is preserved inside originalPayload
// for inspection.
func (s *Store) ImportLegacyTicket(cycle int, id, owner, originalPayload string) (TicketRecord, error) {
	if strings.TrimSpace(id) == "" {
		return TicketRecord{}, errors.New("ticket requires a non-empty id")
	}
	ts := now()
	if _, err := s.db.Exec(`INSERT INTO workflow_tickets(project, cycle, id, revision, owner, status, payload, state_version, legacy, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?, ?, 1, 1, ?, ?)`,
		s.project, cycle, id, owner, TicketLegacyUnverified, originalPayload, ts, ts); err != nil {
		return TicketRecord{}, err
	}
	return s.GetTicket(cycle, id)
}

// ListTickets returns every ticket's latest revision for a cycle, ordered by
// id. A single correlated query, not a collect-then-fetch loop: with the
// store's single pooled connection, a query nested inside an open Rows would
// deadlock (see UnresolvedAttempts's fix for the same pattern).
func (s *Store) ListTickets(cycle int) ([]TicketRecord, error) {
	rows, err := s.db.Query(`SELECT id, revision, owner, status, payload, state_version, legacy FROM workflow_tickets t1
		WHERE project = ? AND cycle = ? AND revision = (
			SELECT MAX(revision) FROM workflow_tickets t2
			WHERE t2.project = t1.project AND t2.cycle = t1.cycle AND t2.id = t1.id)
		ORDER BY id`, s.project, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []TicketRecord
	for rows.Next() {
		r := TicketRecord{Project: s.project, Cycle: cycle}
		var legacy int
		if err := rows.Scan(&r.ID, &r.Revision, &r.Owner, &r.Status, &r.Payload, &r.StateVersion, &legacy); err != nil {
			return nil, err
		}
		r.Legacy = legacy != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteTickets removes every ticket recorded for a cycle. It is for
// consolidating a *revised* plan after a human rejection: Discuss only ever
// runs from a phase before any ticket could have been claimed or executed,
// so the old set can never be superseding real work — only replacing an
// unapproved proposal with another one.
func (s *Store) DeleteTickets(cycle int) error {
	_, err := s.db.Exec(`DELETE FROM workflow_tickets WHERE project = ? AND cycle = ?`, s.project, cycle)
	return err
}

// ImportLegacyArtifact records a file-era deliverable's hash for provenance.
// It skips the two-phase pending/published flow ArtifactStore.Publish uses
// (that's Step 5's artifact-publication work, not import's) since there is
// nothing to reconcile: the file already exists on disk and always has,
// from before any store existed. Idempotent under either of the table's
// unique constraints, so re-running import is safe.
func (s *Store) ImportLegacyArtifact(cycle int, refID, path, sha256 string) error {
	ts := now()
	_, err := s.db.Exec(`INSERT INTO workflow_artifacts(project, cycle, ref_id, path, sha256, state, created_at, published_at)
		VALUES (?, ?, ?, ?, ?, 'legacy', ?, ?)
		ON CONFLICT DO NOTHING`, s.project, cycle, refID, path, sha256, ts, ts)
	return err
}

// ApplyTicketStatus moves a ticket's latest revision from its current status
// to `to`, under the same conditional-version and actor rules as
// ApplyCyclePhase.
func (s *Store) ApplyTicketStatus(cycle int, id, to, actor string, expectedVersion int64, evt Event) (TicketRecord, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return TicketRecord{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var revision int
	var from string
	var version int64
	if err := tx.QueryRow(`SELECT revision, status, state_version FROM workflow_tickets
		WHERE project = ? AND cycle = ? AND id = ? ORDER BY revision DESC LIMIT 1`, s.project, cycle, id).
		Scan(&revision, &from, &version); err != nil {
		return TicketRecord{}, err
	}
	if version != expectedVersion {
		return TicketRecord{}, &ErrStaleVersion{Kind: "ticket", Expected: expectedVersion}
	}
	if !actorAllowed(ticketTransitions, from, to, actor) {
		return TicketRecord{}, &ErrTransitionNotAllowed{From: from, To: to, Actor: actor}
	}
	res, err := tx.Exec(`UPDATE workflow_tickets SET status = ?, state_version = state_version + 1, updated_at = ?
		WHERE project = ? AND cycle = ? AND id = ? AND revision = ? AND state_version = ?`,
		to, now(), s.project, cycle, id, revision, expectedVersion)
	if err != nil {
		return TicketRecord{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return TicketRecord{}, &ErrStaleVersion{Kind: "ticket", Expected: expectedVersion}
	}
	evt.Cycle, evt.TicketID, evt.Actor = cycle, id, actor
	if evt.Type == "" {
		evt.Type = "ticket.status_changed:" + to
	}
	if err := s.appendEventTx(tx, evt); err != nil {
		return TicketRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return TicketRecord{}, err
	}
	return s.GetTicket(cycle, id)
}

// ---- Approvals ----

// RecordApproval commits the human's approval. It does not itself change any
// cycle phase — the caller pairs it with ApplyCyclePhase(..., PhaseApproved,
// ActorHuman, ...) — but it is the durable record of who approved what.
func (s *Store) RecordApproval(a Approval) error {
	if a.ID == "" || a.Actor == "" || a.PlanHash == "" || a.ScopeHash == "" || a.Baseline == "" {
		return errors.New("approval requires id, actor, plan, scope, and baseline hashes")
	}
	if a.ApprovedAt.IsZero() {
		a.ApprovedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO workflow_approvals(id, project, cycle, actor, plan_hash, scope_hash, baseline_hash, contract_hash, approved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, s.project, a.Cycle, a.Actor, a.PlanHash, a.ScopeHash, a.Baseline, a.ContractHash, a.ApprovedAt.Format(time.RFC3339Nano))
	return err
}

// ---- Commands (idempotent CLI invocations; wired in Step 5.7) ----

// CheckCommand reports whether key was already recorded, and its result.
func (s *Store) CheckCommand(key string) (result string, found bool, err error) {
	err = s.db.QueryRow(`SELECT result FROM workflow_commands WHERE key = ? AND project = ?`, key, s.project).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return result, true, nil
}

// RecordCommand commits key's result. A second RecordCommand for the same
// key fails on the primary key, which is the point: a replayed command reads
// via CheckCommand, it does not re-record.
func (s *Store) RecordCommand(key, command, result string) error {
	_, err := s.db.Exec(`INSERT INTO workflow_commands(key, project, command, result, created_at) VALUES (?, ?, ?, ?, ?)`,
		key, s.project, command, result, now())
	return err
}
