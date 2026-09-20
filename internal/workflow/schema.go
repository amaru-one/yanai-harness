package workflow

import (
	"database/sql"
	"errors"
	"fmt"
)

// storeSchemaVersion is the ladder this binary knows how to run. OpenStore
// refuses a database stamped with a newer version: an old binary must never
// guess at what a newer schema's columns mean.
const storeSchemaVersion = 2

// migrations are applied in order, each in its own transaction, and never
// rewritten once released — a later version only appends. Table DDL uses
// "IF NOT EXISTS" so a migration is safe to re-run if the version stamp
// commit is interrupted between statements.
var migrations = []string{
	`
CREATE TABLE IF NOT EXISTS workflow_cycles (
	project        TEXT NOT NULL,
	cycle          INTEGER NOT NULL,
	phase          TEXT NOT NULL,
	verdict        TEXT NOT NULL DEFAULT '',
	origin         TEXT NOT NULL DEFAULT '',
	base_commit    TEXT NOT NULL DEFAULT '',
	plan_hash      TEXT NOT NULL DEFAULT '',
	scope_hash     TEXT NOT NULL DEFAULT '',
	baseline_hash  TEXT NOT NULL DEFAULT '',
	payload        TEXT NOT NULL DEFAULT '',
	state_version  INTEGER NOT NULL DEFAULT 1,
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	PRIMARY KEY (project, cycle)
);

CREATE TABLE IF NOT EXISTS workflow_tickets (
	project        TEXT NOT NULL,
	cycle          INTEGER NOT NULL,
	id             TEXT NOT NULL,
	revision       INTEGER NOT NULL,
	owner          TEXT NOT NULL,
	status         TEXT NOT NULL,
	payload        TEXT NOT NULL,
	state_version  INTEGER NOT NULL DEFAULT 1,
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	PRIMARY KEY (project, cycle, id, revision)
);

CREATE TABLE IF NOT EXISTS workflow_approvals (
	id             TEXT PRIMARY KEY,
	project        TEXT NOT NULL,
	cycle          INTEGER NOT NULL,
	actor          TEXT NOT NULL,
	plan_hash      TEXT NOT NULL,
	scope_hash     TEXT NOT NULL,
	baseline_hash  TEXT NOT NULL,
	contract_hash  TEXT NOT NULL DEFAULT '',
	approved_at    TEXT NOT NULL,
	UNIQUE (project, cycle, contract_hash)
);

CREATE TABLE IF NOT EXISTS workflow_claims (
	project        TEXT NOT NULL,
	cycle          INTEGER NOT NULL,
	ticket_id      TEXT NOT NULL,
	holder         TEXT NOT NULL,
	acquired_at    TEXT NOT NULL,
	heartbeat_at   TEXT NOT NULL,
	expires_at     TEXT NOT NULL,
	PRIMARY KEY (project, cycle, ticket_id)
);
CREATE INDEX IF NOT EXISTS workflow_claims_expiry_idx ON workflow_claims(project, expires_at);

CREATE TABLE IF NOT EXISTS workflow_attempts (
	id                TEXT PRIMARY KEY,
	project           TEXT NOT NULL,
	cycle             INTEGER NOT NULL DEFAULT 0,
	ticket_id         TEXT NOT NULL DEFAULT '',
	role              TEXT NOT NULL,
	kind              TEXT NOT NULL,
	request_hash      TEXT NOT NULL,
	response_hash     TEXT NOT NULL DEFAULT '',
	state             TEXT NOT NULL,
	cost_known        INTEGER NOT NULL DEFAULT 0,
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens      INTEGER NOT NULL DEFAULT 0,
	error             TEXT NOT NULL DEFAULT '',
	started_at        TEXT NOT NULL,
	ended_at          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS workflow_attempts_state_idx ON workflow_attempts(project, state);

CREATE TABLE IF NOT EXISTS workflow_artifacts (
	project        TEXT NOT NULL,
	cycle          INTEGER NOT NULL,
	ref_id         TEXT NOT NULL,
	path           TEXT NOT NULL,
	sha256         TEXT NOT NULL,
	version        TEXT NOT NULL DEFAULT '',
	state          TEXT NOT NULL,
	created_at     TEXT NOT NULL,
	published_at   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (project, cycle, ref_id),
	UNIQUE (project, path)
);

CREATE TABLE IF NOT EXISTS workflow_events (
	id               TEXT PRIMARY KEY,
	project          TEXT NOT NULL,
	cycle            INTEGER NOT NULL DEFAULT 0,
	ticket_id        TEXT NOT NULL DEFAULT '',
	actor            TEXT NOT NULL,
	type             TEXT NOT NULL,
	idempotency_key  TEXT NOT NULL UNIQUE,
	payload          TEXT NOT NULL DEFAULT '',
	created_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS workflow_events_cycle_idx ON workflow_events(project, cycle);

CREATE TABLE IF NOT EXISTS workflow_commands (
	key         TEXT PRIMARY KEY,
	project     TEXT NOT NULL,
	command     TEXT NOT NULL,
	result      TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL
);
`,
	// v2: a `legacy` marker on cycles and tickets. yanai import --legacy
	// writes rows directly at whatever phase/status the old state.json had —
	// which can coincide with a real transition-table "from" value, e.g. an
	// old cycle sitting at "approved". Without this column, a fresh 'yanai
	// run' could pick such a cycle up and treat an unverified, imported
	// record as newly authorized work. Every mutating store method checks
	// it and refuses outright when set, regardless of what the transition
	// table would otherwise allow.
	`
ALTER TABLE workflow_cycles ADD COLUMN legacy INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_tickets ADD COLUMN legacy INTEGER NOT NULL DEFAULT 0;
`,
}

// applyMigrations brings db up to storeSchemaVersion, or refuses if the
// database is already stamped with a version newer than this binary knows.
func applyMigrations(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS workflow_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	var raw string
	current := 0
	err := db.QueryRow(`SELECT value FROM workflow_meta WHERE key = 'schema_version'`).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		current = 0
	case err != nil:
		return err
	default:
		if _, err := fmt.Sscanf(raw, "%d", &current); err != nil {
			return fmt.Errorf("workflow_meta.schema_version is not a number: %q", raw)
		}
	}
	if current > storeSchemaVersion {
		return fmt.Errorf("workflow store is at schema version %d, newer than this binary's %d; upgrade yanai", current, storeSchemaVersion)
	}
	for v := current; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO workflow_meta(key, value) VALUES ('schema_version', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, fmt.Sprintf("%d", v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
