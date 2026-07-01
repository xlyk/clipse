package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no cgo)

	"github.com/xlyk/clipse/internal/store"
)

// TestOpen_MigratesEmptyDB_CreatesLinearWrites asserts that a fresh database
// gets the linear_writes outbox table (A2) in addition to the original three.
func TestOpen_MigratesEmptyDB_CreatesLinearWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	defer s.Close()

	var got string
	row := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='linear_writes'`)
	if err := row.Scan(&got); err != nil {
		t.Fatalf("table %q: not found after migrate: %v", "linear_writes", err)
	}
}

// TestOpen_MigratesEmptyDB_RunsHasProcStartedAt asserts that a fresh runs
// table includes the proc_started_at column used for PID-identity checks on
// dispatcher-restart orphan recovery (A1).
func TestOpen_MigratesEmptyDB_RunsHasProcStartedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	defer s.Close()

	if !hasColumn(t, s.DB(), "runs", "proc_started_at") {
		t.Errorf("runs table missing proc_started_at column after migrating an empty db")
	}
}

// TestOpen_AddsProcStartedAtToPreExistingRunsTable simulates a database that
// was migrated before proc_started_at existed (i.e. a pre-existing runs
// table lacking the column). Open must add the column additively without
// erroring, and re-running Open again must remain a no-op.
func TestOpen_AddsProcStartedAtToPreExistingRunsTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	// Build a "legacy" runs table missing proc_started_at, bypassing the
	// store package's own migrations entirely.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: unexpected error: %v", err)
	}
	const legacyRunsTable = `CREATE TABLE runs (
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
	)`
	if _, err := db.Exec(legacyRunsTable); err != nil {
		t.Fatalf("creating legacy runs table: %v", err)
	}
	if hasColumn(t, db, "runs", "proc_started_at") {
		t.Fatalf("legacy runs table unexpectedly already has proc_started_at")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing legacy db: %v", err)
	}

	// Opening through the store package must add the missing column.
	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: unexpected error: %v", err)
	}
	if !hasColumn(t, s1.DB(), "runs", "proc_started_at") {
		t.Fatalf("runs table still missing proc_started_at after Open")
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("first Close: unexpected error: %v", err)
	}

	// Re-opening (re-migrating) an already-patched database must be a no-op:
	// no error, column still present exactly once.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second Open: unexpected error: %v", err)
	}
	defer s2.Close()
	if !hasColumn(t, s2.DB(), "runs", "proc_started_at") {
		t.Errorf("runs table missing proc_started_at after re-Open")
	}
}

// hasColumn reports whether table has a column named col, using
// PRAGMA table_info.
func hasColumn(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()

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
			t.Fatalf("scanning table_info row: %v", err)
		}
		if name == col {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating table_info rows: %v", err)
	}
	return false
}
