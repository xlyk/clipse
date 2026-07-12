package store_test

import (
	"context"
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

func TestOpen_MigratesPreAgentWorkspaceDBIdempotently(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: unexpected error: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE legacy_marker (value TEXT NOT NULL)`); err != nil {
		t.Fatalf("creating pre-feature marker table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO legacy_marker (value) VALUES ('preserve-me')`); err != nil {
		t.Fatalf("seeding pre-feature marker table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing pre-feature db: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		s, err := store.Open(path)
		if err != nil {
			t.Fatalf("Open attempt %d: unexpected error: %v", attempt, err)
		}
		var marker string
		if err := s.DB().QueryRow(`SELECT value FROM legacy_marker`).Scan(&marker); err != nil {
			s.Close()
			t.Fatalf("reading preserved marker after Open attempt %d: %v", attempt, err)
		}
		if marker != "preserve-me" {
			s.Close()
			t.Fatalf("marker after Open attempt %d = %q, want %q", attempt, marker, "preserve-me")
		}
		for _, name := range []string{"agent_workspaces", "idx_agent_workspaces_issue", "idx_agent_workspaces_cleanup"} {
			var got string
			if err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE name = ?`, name).Scan(&got); err != nil {
				s.Close()
				t.Fatalf("schema object %q missing after Open attempt %d: %v", name, attempt, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close attempt %d: unexpected error: %v", attempt, err)
		}
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

// TestOpen_MigratesEmptyDB_IssuesHasTitleAndDescription asserts that a fresh
// issues table includes the title/description columns the dispatcher needs
// to build a claimed issue's CLIPSE_ISSUE_TEXT (Phase-2 issue-text plumbing).
func TestOpen_MigratesEmptyDB_IssuesHasTitleAndDescription(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	defer s.Close()

	if !hasColumn(t, s.DB(), "issues", "title") {
		t.Errorf("issues table missing title column after migrating an empty db")
	}
	if !hasColumn(t, s.DB(), "issues", "description") {
		t.Errorf("issues table missing description column after migrating an empty db")
	}
}

// TestOpen_AddsTitleDescriptionToPreExistingIssuesTable simulates a database
// that was migrated before title/description existed on the issues table
// (i.e. a pre-existing issues table lacking both columns). Open must add
// them additively without erroring, and re-running Open again must remain a
// no-op.
func TestOpen_AddsTitleDescriptionToPreExistingIssuesTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	// Build a "legacy" issues table missing title/description, bypassing
	// the store package's own migrations entirely.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: unexpected error: %v", err)
	}
	const legacyIssuesTable = `CREATE TABLE issues (
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
	)`
	if _, err := db.Exec(legacyIssuesTable); err != nil {
		t.Fatalf("creating legacy issues table: %v", err)
	}
	if hasColumn(t, db, "issues", "title") || hasColumn(t, db, "issues", "description") {
		t.Fatalf("legacy issues table unexpectedly already has title/description")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing legacy db: %v", err)
	}

	// Opening through the store package must add the missing columns.
	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: unexpected error: %v", err)
	}
	if !hasColumn(t, s1.DB(), "issues", "title") {
		t.Fatalf("issues table still missing title after Open")
	}
	if !hasColumn(t, s1.DB(), "issues", "description") {
		t.Fatalf("issues table still missing description after Open")
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("first Close: unexpected error: %v", err)
	}

	// Re-opening (re-migrating) an already-patched database must be a no-op:
	// no error, both columns still present exactly once.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second Open: unexpected error: %v", err)
	}
	defer s2.Close()
	if !hasColumn(t, s2.DB(), "issues", "title") {
		t.Errorf("issues table missing title after re-Open")
	}
	if !hasColumn(t, s2.DB(), "issues", "description") {
		t.Errorf("issues table missing description after re-Open")
	}
}

// TestOpen_MigratesEmptyDB_IssuesHasReworkCount asserts that a fresh issues
// table includes the rework_count column amendment C1's rework_cap compares
// against (Phase-3 cross-lane claiming).
func TestOpen_MigratesEmptyDB_IssuesHasReworkCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	defer s.Close()

	if !hasColumn(t, s.DB(), "issues", "rework_count") {
		t.Errorf("issues table missing rework_count column after migrating an empty db")
	}
}

// TestOpen_MigratesEmptyDB_IssuesHasRecoveryColumns asserts that a fresh
// issues table includes the recover_attempts / blocked_until columns
// auto-unblock layer 1 relies on (recover_attempts against cfg.RecoverCap;
// blocked_until as the claim-skip backoff gate).
func TestOpen_MigratesEmptyDB_IssuesHasRecoveryColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	defer s.Close()

	if !hasColumn(t, s.DB(), "issues", "recover_attempts") {
		t.Errorf("issues table missing recover_attempts column after migrating an empty db")
	}
	if !hasColumn(t, s.DB(), "issues", "blocked_until") {
		t.Errorf("issues table missing blocked_until column after migrating an empty db")
	}
}

// TestOpen_AddsRecoveryColumnsToPreExistingIssuesTable simulates a database
// migrated before recover_attempts / blocked_until existed (a pre-existing
// issues table lacking both). Open must add them additively and default
// existing rows to 0, and re-running Open must remain a no-op.
func TestOpen_AddsRecoveryColumnsToPreExistingIssuesTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	// A "legacy" issues table carrying rework_count but not the recovery
	// columns, bypassing the store package's own migrations.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: unexpected error: %v", err)
	}
	const legacyIssuesTable = `CREATE TABLE issues (
		id            TEXT PRIMARY KEY,
		identifier    TEXT NOT NULL,
		title         TEXT NOT NULL DEFAULT '',
		description   TEXT NOT NULL DEFAULT '',
		lane_label    TEXT NOT NULL DEFAULT '',
		board_status  TEXT NOT NULL DEFAULT '',
		rework_count  INTEGER NOT NULL DEFAULT 0,
		deps          TEXT NOT NULL DEFAULT '[]',
		priority      INTEGER NOT NULL DEFAULT 0,
		branch_name   TEXT NOT NULL DEFAULT '',
		claim_lock    TEXT,
		claim_expires INTEGER,
		updated_at    INTEGER NOT NULL DEFAULT 0,
		last_seen     INTEGER NOT NULL DEFAULT 0,
		created_at    INTEGER NOT NULL DEFAULT 0
	)`
	if _, err := db.Exec(legacyIssuesTable); err != nil {
		t.Fatalf("creating legacy issues table: %v", err)
	}
	// Seed one legacy row so we can assert the additive column defaults to 0.
	if _, err := db.Exec(`INSERT INTO issues (id, identifier) VALUES ('issue-legacy', 'issue-legacy')`); err != nil {
		t.Fatalf("seeding legacy row: %v", err)
	}
	if hasColumn(t, db, "issues", "recover_attempts") || hasColumn(t, db, "issues", "blocked_until") {
		t.Fatalf("legacy issues table unexpectedly already has recovery columns")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing legacy db: %v", err)
	}

	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: unexpected error: %v", err)
	}
	if !hasColumn(t, s1.DB(), "issues", "recover_attempts") {
		t.Fatalf("issues table still missing recover_attempts after Open")
	}
	if !hasColumn(t, s1.DB(), "issues", "blocked_until") {
		t.Fatalf("issues table still missing blocked_until after Open")
	}
	// The retrofit must default existing rows to 0 for both columns.
	got, err := s1.GetIssue(context.Background(), "issue-legacy")
	if err != nil {
		t.Fatalf("GetIssue(issue-legacy): unexpected error: %v", err)
	}
	if got.RecoverAttempts != 0 || got.BlockedUntil != 0 {
		t.Errorf("legacy row recovery fields = (%d,%d), want (0,0)", got.RecoverAttempts, got.BlockedUntil)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("first Close: unexpected error: %v", err)
	}

	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second Open: unexpected error: %v", err)
	}
	defer s2.Close()
	if !hasColumn(t, s2.DB(), "issues", "recover_attempts") || !hasColumn(t, s2.DB(), "issues", "blocked_until") {
		t.Errorf("issues table missing recovery columns after re-Open")
	}
}

// TestOpen_AddsReworkCountToPreExistingIssuesTable simulates a database that
// was migrated before rework_count existed on the issues table. Open must add
// it additively without erroring, and re-running Open again must remain a
// no-op.
func TestOpen_AddsReworkCountToPreExistingIssuesTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	// Build a "legacy" issues table missing rework_count, bypassing the
	// store package's own migrations entirely.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: unexpected error: %v", err)
	}
	const legacyIssuesTable = `CREATE TABLE issues (
		id            TEXT PRIMARY KEY,
		identifier    TEXT NOT NULL,
		title         TEXT NOT NULL DEFAULT '',
		description   TEXT NOT NULL DEFAULT '',
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
	)`
	if _, err := db.Exec(legacyIssuesTable); err != nil {
		t.Fatalf("creating legacy issues table: %v", err)
	}
	if hasColumn(t, db, "issues", "rework_count") {
		t.Fatalf("legacy issues table unexpectedly already has rework_count")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing legacy db: %v", err)
	}

	// Opening through the store package must add the missing column.
	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: unexpected error: %v", err)
	}
	if !hasColumn(t, s1.DB(), "issues", "rework_count") {
		t.Fatalf("issues table still missing rework_count after Open")
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
	if !hasColumn(t, s2.DB(), "issues", "rework_count") {
		t.Errorf("issues table missing rework_count after re-Open")
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
