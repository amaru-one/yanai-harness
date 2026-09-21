package workflow

import (
	"errors"
	"strings"
	"time"
)

// ArtifactRecord is workflow_artifacts' durable row: the bridge between what
// SQLite believes was published and what the filesystem actually holds.
type ArtifactRecord struct {
	Project     string
	Cycle       int
	RefID       string
	Path        string
	SHA256      string
	Version     string
	State       string // "pending" | "published" | "legacy"
	CreatedAt   time.Time
	PublishedAt time.Time
}

// BeginArtifact commits a pending row before any bytes are linked into
// place: a plain INSERT against workflow_artifacts' PRIMARY KEY
// (project, cycle, ref_id) and UNIQUE (project, path). Two concurrent
// publishers of the same ref or path have exactly one INSERT succeed; the
// loser gets the driver's constraint error back as-is -- the same pattern
// ClaimTicket already uses -- and is expected to re-check GetArtifact rather
// than inspect the error.
func (s *Store) BeginArtifact(cycle int, ref ArtifactRef) (ArtifactRecord, error) {
	if _, err := s.db.Exec(`INSERT INTO workflow_artifacts(project, cycle, ref_id, path, sha256, version, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
		s.project, cycle, ref.ID, ref.Path, ref.SHA256, ref.Version, now()); err != nil {
		return ArtifactRecord{}, err
	}
	return s.GetArtifact(cycle, ref.ID)
}

// GetArtifact returns the current row for ref_id in cycle. Callers use
// errors.Is(err, sql.ErrNoRows) to tell "never begun" apart from a real
// failure.
func (s *Store) GetArtifact(cycle int, refID string) (ArtifactRecord, error) {
	r := ArtifactRecord{Project: s.project, Cycle: cycle, RefID: refID}
	var created, published string
	if err := s.db.QueryRow(`SELECT path, sha256, version, state, created_at, published_at
		FROM workflow_artifacts WHERE project = ? AND cycle = ? AND ref_id = ?`,
		s.project, cycle, refID).
		Scan(&r.Path, &r.SHA256, &r.Version, &r.State, &created, &published); err != nil {
		return ArtifactRecord{}, err
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if published != "" {
		r.PublishedAt, _ = time.Parse(time.RFC3339Nano, published)
	}
	return r, nil
}

// PublishArtifact flips a pending row to published, stamping the hash that
// was actually linked into place. RowsAffected != 1 means the row was not
// pending -- either already published (a benign replay the caller should
// have caught via GetArtifact first) or missing entirely.
func (s *Store) PublishArtifact(cycle int, refID, sha256 string) (ArtifactRecord, error) {
	res, err := s.db.Exec(`UPDATE workflow_artifacts SET state = 'published', sha256 = ?, published_at = ?
		WHERE project = ? AND cycle = ? AND ref_id = ? AND state = 'pending'`,
		sha256, now(), s.project, cycle, refID)
	if err != nil {
		return ArtifactRecord{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ArtifactRecord{}, errors.New("artifact is not pending; it may already be published or does not exist")
	}
	return s.GetArtifact(cycle, refID)
}

// PendingArtifacts lists every state='pending' row for this project, across
// cycles -- startup reconciliation's input. Rows are collected before any
// nested query runs, the same collect-then-fetch shape ReconcileAttempts
// uses, since OpenStore pools a single connection and an open Rows would
// otherwise starve a nested query of a connection and deadlock.
func (s *Store) PendingArtifacts() ([]ArtifactRecord, error) {
	rows, err := s.db.Query(`SELECT cycle, ref_id FROM workflow_artifacts WHERE project = ? AND state = 'pending'`, s.project)
	if err != nil {
		return nil, err
	}
	type key struct {
		cycle int
		refID string
	}
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.cycle, &k.refID); err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return nil, err
	}
	rows.Close() //nolint:errcheck

	var out []ArtifactRecord
	for _, k := range keys {
		rec, err := s.GetArtifact(k.cycle, k.refID)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// DropPendingArtifact removes a pending row that reconciliation found no
// matching file for. Guarded to state='pending' so it can never delete a
// published row by accident.
func (s *Store) DropPendingArtifact(cycle int, refID string) error {
	_, err := s.db.Exec(`DELETE FROM workflow_artifacts WHERE project = ? AND cycle = ? AND ref_id = ? AND state = 'pending'`,
		s.project, cycle, refID)
	return err
}

// RecordCandidate commits the artifact reference with candidate_ready, so a
// crash before the JSON projection cannot lose the only reference to the output.
func (s *Store) RecordCandidate(cycle int, ticket, ref string, expected int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var published string
	if err = tx.QueryRow(`SELECT state FROM workflow_artifacts WHERE project=? AND cycle=? AND ref_id=?`, s.project, cycle, ref).Scan(&published); err != nil {
		return err
	}
	if published != "published" {
		return errors.New("candidate artifact is not published")
	}
	res, err := tx.Exec(`UPDATE workflow_tickets SET status='candidate_ready',state_version=state_version+1 WHERE project=? AND cycle=? AND id=? AND state_version=? AND status='response_recorded' AND legacy=0`, s.project, cycle, ticket, expected)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return &ErrStaleVersion{Kind: "ticket", Expected: expected}
	}
	_, err = tx.Exec(`INSERT INTO workflow_candidates(project,cycle,ticket,ref) VALUES(?,?,?,?) ON CONFLICT(project,cycle,ticket) DO UPDATE SET ref=excluded.ref`, s.project, cycle, ticket, ref)
	if err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, TicketID: ticket, Actor: ActorEngine, Type: "candidate.ready", Payload: ref}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CandidateRef(cycle int, ticket string) (string, error) {
	var ref string
	err := s.db.QueryRow(`SELECT ref FROM workflow_candidates WHERE project=? AND cycle=? AND ticket=?`, s.project, cycle, ticket).Scan(&ref)
	return ref, err
}

// ArtifactIDs returns immutable published inputs under a candidate directory.
func (s *Store) ArtifactIDs(cycle int, prefix string) ([]string, error) {
	rows, err := s.db.Query(`SELECT ref_id,path FROM workflow_artifacts WHERE project=? AND cycle=? AND state='published' ORDER BY ref_id`, s.project, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, path string
		if err = rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		if strings.HasPrefix(path, prefix) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}
