package workflow

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS workflow_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workflow_tickets (id TEXT PRIMARY KEY, payload TEXT NOT NULL, status TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 0, state_version INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS workflow_approvals (id TEXT PRIMARY KEY, payload TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workflow_events (id TEXT PRIMARY KEY, ticket_id TEXT, idempotency_key TEXT NOT NULL UNIQUE, payload TEXT NOT NULL, created_at TEXT NOT NULL);
INSERT INTO workflow_meta(key, value) VALUES ('schema_version', '1') ON CONFLICT(key) DO NOTHING;`)
	return err
}

func (s *Store) SaveTicket(t Ticket) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO workflow_tickets(id,payload,status,revision,state_version) VALUES(?,?,?,?,0)
ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,status=excluded.status,revision=excluded.revision,state_version=workflow_tickets.state_version+1`, t.ID, string(b), t.Status, t.Revision)
	return err
}

func (s *Store) GetTicket(id string) (Ticket, error) {
	var payload string
	if err := s.db.QueryRow(`SELECT payload FROM workflow_tickets WHERE id=?`, id).Scan(&payload); err != nil {
		return Ticket{}, err
	}
	var t Ticket
	if err := json.Unmarshal([]byte(payload), &t); err != nil {
		return Ticket{}, err
	}
	return t, nil
}

func (s *Store) Approve(a Approval) error {
	if a.ID == "" || a.Actor == "" || a.PlanHash == "" || a.ScopeHash == "" || a.Baseline == "" {
		return errors.New("approval requires id, actor, plan, scope, and baseline hashes")
	}
	if a.ApprovedAt.IsZero() {
		a.ApprovedAt = time.Now().UTC()
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO workflow_approvals(id,payload,created_at) VALUES(?,?,?)`, a.ID, string(b), a.ApprovedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) AppendEvent(e Event) error {
	if e.ID == "" || e.IdempotencyKey == "" || e.Type == "" {
		return errors.New("event id, type, and idempotency key are required")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO workflow_events(id,ticket_id,idempotency_key,payload,created_at) VALUES(?,?,?,?,?)`, e.ID, e.TicketID, e.IdempotencyKey, string(b), e.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}
