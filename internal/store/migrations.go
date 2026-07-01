package store

import (
	"database/sql"
	"fmt"
)

// migrations are applied in order on every Open. Each statement uses
// CREATE TABLE IF NOT EXISTS (or an equivalent guard) so re-running the full
// set against an already-migrated database is a no-op.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS issues (
		id            TEXT PRIMARY KEY,
		identifier    TEXT NOT NULL,
		lane_label    TEXT NOT NULL DEFAULT '',
		board_status  TEXT NOT NULL DEFAULT '',
		deps          TEXT NOT NULL DEFAULT '[]',
		priority      INTEGER NOT NULL DEFAULT 0,
		branch_name   TEXT NOT NULL DEFAULT '',
		claim_lock    TEXT,
		claim_expires INTEGER,
		updated_at    INTEGER NOT NULL DEFAULT 0,
		last_seen     INTEGER NOT NULL DEFAULT 0,
		created_at    INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS runs (
		run_id       TEXT PRIMARY KEY,
		issue_id     TEXT NOT NULL,
		lane         TEXT NOT NULL,
		worker_pid   INTEGER,
		status       TEXT NOT NULL,
		started_at   INTEGER NOT NULL DEFAULT 0,
		heartbeat_at INTEGER NOT NULL DEFAULT 0,
		attempt      INTEGER NOT NULL DEFAULT 1,
		turn_count   INTEGER NOT NULL DEFAULT 0,
		thread_id    TEXT NOT NULL DEFAULT '',
		result_json  TEXT,
		error        TEXT,
		tokens_in    INTEGER NOT NULL DEFAULT 0,
		tokens_out   INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_runs_issue_id ON runs (issue_id)`,
	`CREATE TABLE IF NOT EXISTS events (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		ts      INTEGER NOT NULL,
		issue_id TEXT,
		run_id   TEXT,
		kind     TEXT NOT NULL,
		detail   TEXT NOT NULL DEFAULT ''
	)`,
}

// migrate applies every migration statement in order. Statements are
// idempotent, so calling migrate on an already-migrated database is a no-op.
func migrate(db *sql.DB) error {
	for i, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("applying migration %d: %w", i, err)
		}
	}
	return nil
}
