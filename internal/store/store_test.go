package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/xlyk/clipse/internal/store"
)

// openTestStore opens a Store backed by a fresh SQLite file in a temp dir.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: unexpected error: %v", err)
		}
	})
	return s
}

func TestOpen_MigratesEmptyDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	defer s.Close()

	db := s.DB()
	wantTables := []string{"issues", "runs", "events", "dispatcher_control"}
	for _, name := range wantTables {
		var got string
		row := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name)
		if err := row.Scan(&got); err != nil {
			t.Errorf("table %q: not found after migrate: %v", name, err)
			continue
		}
		if got != name {
			t.Errorf("table %q: got %q", name, got)
		}
	}

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestOpen_ReMigrateIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")

	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: unexpected error: %v", err)
	}

	ctx := context.Background()
	if err := s1.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1"}); err != nil {
		t.Fatalf("UpsertIssue: unexpected error: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("first Close: unexpected error: %v", err)
	}

	// Re-opening (and thus re-migrating) an already-migrated DB must not
	// wipe or error on existing data.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second Open: unexpected error: %v", err)
	}
	defer s2.Close()

	snap, err := s2.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if len(snap.Issues) != 1 {
		t.Fatalf("len(snap.Issues) = %d, want 1", len(snap.Issues))
	}
	if snap.Issues[0].Identifier != "CLP-1" {
		t.Errorf("Issues[0].Identifier = %q, want %q", snap.Issues[0].Identifier, "CLP-1")
	}
}

func TestUpsertIssue_InsertThenReadBack(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	issue := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		LaneLabel:   "agent:coder",
		BoardStatus: "ready",
		Deps:        `["issue-0"]`,
		Priority:    1,
		BranchName:  "clp-1-do-thing",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("UpsertIssue: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if len(snap.Issues) != 1 {
		t.Fatalf("len(snap.Issues) = %d, want 1", len(snap.Issues))
	}
	got := snap.Issues[0]
	if got.ID != issue.ID || got.Identifier != issue.Identifier || got.LaneLabel != issue.LaneLabel ||
		got.BoardStatus != issue.BoardStatus || got.Deps != issue.Deps || got.Priority != issue.Priority ||
		got.BranchName != issue.BranchName {
		t.Errorf("round-tripped issue = %+v, want %+v", got, issue)
	}
	if got.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true on fresh insert, want false")
	}
}

func TestUpsertIssue_ConflictPreservesDispatcherState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// A dispatcher-claimed, running issue: board_status/claim fields are
	// runtime state the dispatcher owns.
	seeded := store.Issue{
		ID:           "issue-1",
		Identifier:   "CLP-1",
		LaneLabel:    "agent:coder",
		BoardStatus:  "running",
		Deps:         `[]`,
		Priority:     1,
		BranchName:   "clp-1-do-thing",
		ClaimLock:    sql.NullString{String: "worker-abc", Valid: true},
		ClaimExpires: sql.NullInt64{Int64: 12345, Valid: true},
		UpdatedAt:    100,
		LastSeen:     100,
		CreatedAt:    100,
	}
	if err := s.UpsertIssue(ctx, seeded); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	// Simulate a fresh Linear poll: Linear still shows the card in its old
	// column ("ready") and the poller passes the dispatcher-owned fields
	// zero-valued. Neither the stale status nor the zero claim fields may
	// overwrite the live runtime state.
	fresh := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		LaneLabel:   "agent:coder",
		BoardStatus: "ready",
		Deps:        `["issue-0"]`,
		Priority:    2,
		BranchName:  "clp-1-do-thing",
		UpdatedAt:   200,
		LastSeen:    200,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, fresh); err != nil {
		t.Fatalf("conflict UpsertIssue: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if len(snap.Issues) != 1 {
		t.Fatalf("len(snap.Issues) = %d, want 1", len(snap.Issues))
	}
	got := snap.Issues[0]
	// Dispatcher-owned runtime state must survive a re-poll.
	if !got.ClaimLock.Valid || got.ClaimLock.String != "worker-abc" {
		t.Errorf("ClaimLock = %+v, want preserved %q", got.ClaimLock, "worker-abc")
	}
	if !got.ClaimExpires.Valid || got.ClaimExpires.Int64 != 12345 {
		t.Errorf("ClaimExpires = %+v, want preserved 12345", got.ClaimExpires)
	}
	if got.BoardStatus != "running" {
		t.Errorf("BoardStatus = %q, want preserved %q (dispatcher-owned; a poll must not reset it)", got.BoardStatus, "running")
	}
	// Linear-sourced intent fields must update.
	if got.Deps != `["issue-0"]` {
		t.Errorf("Deps = %q, want updated value", got.Deps)
	}
	if got.Priority != 2 {
		t.Errorf("Priority = %d, want 2", got.Priority)
	}
	if got.LastSeen != 200 {
		t.Errorf("LastSeen = %d, want 200", got.LastSeen)
	}
}

func TestAppendEvent_ThenReadBack(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1"}); err != nil {
		t.Fatalf("UpsertIssue: unexpected error: %v", err)
	}

	ev := store.Event{
		Ts:      100,
		IssueID: sql.NullString{String: "issue-1", Valid: true},
		RunID:   sql.NullString{},
		Kind:    "claimed",
		Detail:  "claimed by worker-abc",
	}
	if err := s.AppendEvent(ctx, ev); err != nil {
		t.Fatalf("AppendEvent: unexpected error: %v", err)
	}

	events, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	got := events[0]
	if got.ID == 0 {
		t.Errorf("ID = 0, want autoincrement to assign a nonzero id")
	}
	if got.Kind != "claimed" || got.Detail != "claimed by worker-abc" {
		t.Errorf("event = %+v, want Kind=claimed Detail=%q", got, "claimed by worker-abc")
	}
	if !got.IssueID.Valid || got.IssueID.String != "issue-1" {
		t.Errorf("IssueID = %+v, want valid %q", got.IssueID, "issue-1")
	}
	if got.RunID.Valid {
		t.Errorf("RunID.Valid = true, want false")
	}
}

func TestInsertRun_CloseRun_ReflectedInSnapshot(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "running"}); err != nil {
		t.Fatalf("UpsertIssue: unexpected error: %v", err)
	}

	run := store.Run{
		RunID:       "run-1",
		IssueID:     "issue-1",
		Lane:        "coder",
		WorkerPID:   sql.NullInt64{Int64: 4242, Valid: true},
		Status:      "running",
		StartedAt:   100,
		HeartbeatAt: 100,
		Attempt:     1,
		TurnCount:   1,
		ThreadID:    "thread-1",
		TokensIn:    0,
		TokensOut:   0,
	}
	if err := s.InsertRun(ctx, run); err != nil {
		t.Fatalf("InsertRun: unexpected error: %v", err)
	}

	if err := s.CloseRun(ctx, "run-1", "done", `{"outcome":"done"}`, "", 10, 20); err != nil {
		t.Fatalf("CloseRun: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if len(snap.Issues) != 1 {
		t.Fatalf("len(snap.Issues) = %d, want 1", len(snap.Issues))
	}
	latest := snap.Issues[0].LatestRun
	if latest == nil {
		t.Fatalf("LatestRun = nil, want the closed run")
	}
	if latest.RunID != "run-1" {
		t.Errorf("LatestRun.RunID = %q, want %q", latest.RunID, "run-1")
	}
	if latest.Status != "done" {
		t.Errorf("LatestRun.Status = %q, want %q", latest.Status, "done")
	}
	if !latest.ResultJSON.Valid || latest.ResultJSON.String != `{"outcome":"done"}` {
		t.Errorf("LatestRun.ResultJSON = %+v, want valid %q", latest.ResultJSON, `{"outcome":"done"}`)
	}
	if latest.Error.Valid {
		t.Errorf("LatestRun.Error.Valid = true, want false (no error on success)")
	}
	if latest.TokensIn != 10 || latest.TokensOut != 20 {
		t.Errorf("LatestRun tokens = (%d,%d), want (10,20)", latest.TokensIn, latest.TokensOut)
	}

	if snap.CountsByStatus["running"] != 1 {
		t.Errorf("CountsByStatus[running] = %d, want 1", snap.CountsByStatus["running"])
	}
}

func TestCloseRun_WithError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1"}); err != nil {
		t.Fatalf("UpsertIssue: unexpected error: %v", err)
	}
	run := store.Run{
		RunID:     "run-1",
		IssueID:   "issue-1",
		Lane:      "coder",
		Status:    "running",
		StartedAt: 100,
		Attempt:   1,
		ThreadID:  "thread-1",
	}
	if err := s.InsertRun(ctx, run); err != nil {
		t.Fatalf("InsertRun: unexpected error: %v", err)
	}

	if err := s.CloseRun(ctx, "run-1", "blocked", "", "agent got stuck", 5, 6); err != nil {
		t.Fatalf("CloseRun: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	latest := snap.Issues[0].LatestRun
	if latest == nil {
		t.Fatalf("LatestRun = nil, want the closed run")
	}
	if latest.ResultJSON.Valid {
		t.Errorf("LatestRun.ResultJSON.Valid = true, want false (empty result on block)")
	}
	if !latest.Error.Valid || latest.Error.String != "agent got stuck" {
		t.Errorf("LatestRun.Error = %+v, want valid %q", latest.Error, "agent got stuck")
	}
}
