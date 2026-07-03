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
		title         TEXT NOT NULL DEFAULT '',
		description   TEXT NOT NULL DEFAULT '',
		lane_label    TEXT NOT NULL DEFAULT '',
		board_status  TEXT NOT NULL DEFAULT '',
		rework_count  INTEGER NOT NULL DEFAULT 0,
		recover_attempts INTEGER NOT NULL DEFAULT 0,
		blocked_until    INTEGER NOT NULL DEFAULT 0,
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
		run_id          TEXT PRIMARY KEY,
		issue_id        TEXT NOT NULL,
		lane            TEXT NOT NULL,
		worker_pid      INTEGER,
		proc_started_at INTEGER,
		status          TEXT NOT NULL,
		started_at      INTEGER NOT NULL DEFAULT 0,
		heartbeat_at    INTEGER NOT NULL DEFAULT 0,
		attempt         INTEGER NOT NULL DEFAULT 1,
		turn_count      INTEGER NOT NULL DEFAULT 0,
		thread_id       TEXT NOT NULL DEFAULT '',
		result_json     TEXT,
		error           TEXT,
		tokens_in       INTEGER NOT NULL DEFAULT 0,
		tokens_out      INTEGER NOT NULL DEFAULT 0
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
	// linear_writes is the A2 outbox: transitions enqueue a pending mirror
	// write in the same transaction as the SQLite state change, and the
	// dispatcher drains it with retry so a Linear API failure can never
	// leave the board diverged from the store.
	`CREATE TABLE IF NOT EXISTS linear_writes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		issue_id   TEXT NOT NULL,
		kind       TEXT NOT NULL,
		target     TEXT NOT NULL DEFAULT '',
		body       TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL DEFAULT 'pending',
		attempts   INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`,
}

// migrate applies every migration statement in order, then applies any
// additive column migrations that CREATE TABLE IF NOT EXISTS cannot express
// (adding a column to a table that already existed before that column was
// introduced). Every step is idempotent, so calling migrate on an
// already-migrated database is a no-op.
func migrate(db *sql.DB) error {
	for i, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("applying migration %d: %w", i, err)
		}
	}
	if err := addColumnIfMissing(db, "runs", "proc_started_at", "INTEGER"); err != nil {
		return fmt.Errorf("applying migration: %w", err)
	}
	// title/description (Phase-2 issue-text plumbing): the CREATE TABLE
	// above already carries both for a fresh database; these retrofit a
	// database migrated before either column existed.
	if err := addColumnIfMissing(db, "issues", "title", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("applying migration: %w", err)
	}
	if err := addColumnIfMissing(db, "issues", "description", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("applying migration: %w", err)
	}
	// rework_count (Phase-3 cross-lane claiming, amendment C1): the CREATE
	// TABLE above already carries it for a fresh database; this retrofits a
	// database migrated before it existed. Existing rows default to 0, same
	// as a freshly inserted issue.
	if err := addColumnIfMissing(db, "issues", "rework_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("applying migration: %w", err)
	}
	// recover_attempts / blocked_until (auto-unblock layer 1): the CREATE
	// TABLE above already carries both for a fresh database; these retrofit a
	// database migrated before they existed. Existing rows default to 0
	// (recover_attempts) and 0 (blocked_until = not blocked), same as a
	// freshly inserted issue.
	if err := addColumnIfMissing(db, "issues", "recover_attempts", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("applying migration: %w", err)
	}
	if err := addColumnIfMissing(db, "issues", "blocked_until", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("applying migration: %w", err)
	}
	return nil
}

// addColumnIfMissing adds column to table with the given SQL type if it
// isn't already present, per PRAGMA table_info(table). SQLite's
// ALTER TABLE ... ADD COLUMN has no "IF NOT EXISTS" form, so this guard is
// what makes adding a column to a pre-existing table idempotent: fresh
// databases already get the column via the CREATE TABLE statement above, so
// this only ever fires (once) against a database migrated before the column
// existed.
func addColumnIfMissing(db *sql.DB, table, column, sqlType string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspecting table %s: %w", table, err)
	}

	var found bool
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scanning table_info(%s) row: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterating table_info(%s) rows: %w", table, err)
	}
	rows.Close()

	if found {
		return nil
	}

	if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, sqlType)); err != nil {
		return fmt.Errorf("adding column %s to %s: %w", column, table, err)
	}
	return nil
}
