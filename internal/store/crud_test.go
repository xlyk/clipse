package store_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/store"
)

// TestSetRunProcess_RoundTrip asserts that SetRunProcess writes worker_pid
// and proc_started_at for a run, visible via the run row (through
// ReadSnapshot's LatestRun and via ListOpenRuns).
func TestSetRunProcess_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "running"}); err != nil {
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

	if err := s.SetRunProcess(ctx, "run-1", 4242, 999); err != nil {
		t.Fatalf("SetRunProcess: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	latest := snap.Issues[0].LatestRun
	if latest == nil {
		t.Fatalf("LatestRun = nil, want the run")
	}
	if !latest.WorkerPID.Valid || latest.WorkerPID.Int64 != 4242 {
		t.Errorf("WorkerPID = %+v, want valid 4242", latest.WorkerPID)
	}
	if !latest.ProcStartedAt.Valid || latest.ProcStartedAt.Int64 != 999 {
		t.Errorf("ProcStartedAt = %+v, want valid 999", latest.ProcStartedAt)
	}
}

func TestSetRunProcess_NoSuchRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SetRunProcess(ctx, "no-such-run", 1, 2); err == nil {
		t.Fatalf("SetRunProcess on nonexistent run: err = nil, want error")
	}
}

// TestListOpenRuns_ReturnsOnlyRunningRows asserts ListOpenRuns returns every
// run row with status='running' (dispatcher startup orphan recovery, A1),
// and excludes closed/stale runs.
func TestListOpenRuns_ReturnsOnlyRunningRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"issue-1", "issue-2", "issue-3"} {
		if err := s.UpsertIssue(ctx, store.Issue{ID: id, Identifier: id}); err != nil {
			t.Fatalf("UpsertIssue(%s): unexpected error: %v", id, err)
		}
	}

	running1 := store.Run{RunID: "run-running-1", IssueID: "issue-1", Lane: "coder", Status: "running", StartedAt: 100, Attempt: 1}
	running2 := store.Run{RunID: "run-running-2", IssueID: "issue-2", Lane: "coder", Status: "running", StartedAt: 200, Attempt: 1}
	done := store.Run{RunID: "run-done", IssueID: "issue-3", Lane: "coder", Status: "running", StartedAt: 50, Attempt: 1}

	for _, r := range []store.Run{running1, running2, done} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun(%s): unexpected error: %v", r.RunID, err)
		}
	}
	if err := s.CloseRun(ctx, "run-done", "done", `{"outcome":"done"}`, "", 1, 1); err != nil {
		t.Fatalf("CloseRun: unexpected error: %v", err)
	}

	open, err := s.ListOpenRuns(ctx)
	if err != nil {
		t.Fatalf("ListOpenRuns: unexpected error: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("len(ListOpenRuns) = %d, want 2", len(open))
	}
	gotIDs := map[string]bool{}
	for _, r := range open {
		if r.Status != "running" {
			t.Errorf("open run %s status = %q, want running", r.RunID, r.Status)
		}
		gotIDs[r.RunID] = true
	}
	if !gotIDs["run-running-1"] || !gotIDs["run-running-2"] {
		t.Errorf("ListOpenRuns = %v, want run-running-1 and run-running-2", gotIDs)
	}
	if gotIDs["run-done"] {
		t.Errorf("ListOpenRuns included closed run-done")
	}
}

func TestListOpenRuns_EmptyWhenNoneRunning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	open, err := s.ListOpenRuns(ctx)
	if err != nil {
		t.Fatalf("ListOpenRuns: unexpected error: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("len(ListOpenRuns) = %d, want 0", len(open))
	}
}

// TestGetIssue_RoundTrip asserts GetIssue fetches the same row UpsertIssue
// wrote (the dispatcher's applyResult needs an issue's current board_status
// without re-reading the whole snapshot).
func TestGetIssue_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	issue := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		LaneLabel:   "agent:coder",
		BoardStatus: "running",
		Deps:        `["issue-0"]`,
		Priority:    2,
		BranchName:  "clp-1-do-thing",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("UpsertIssue: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.ID != issue.ID || got.Identifier != issue.Identifier || got.BoardStatus != issue.BoardStatus ||
		got.Deps != issue.Deps || got.Priority != issue.Priority {
		t.Errorf("GetIssue = %+v, want %+v", got, issue)
	}
}

func TestGetIssue_NoSuchIssue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.GetIssue(ctx, "no-such-issue"); err == nil {
		t.Fatalf("GetIssue on nonexistent issue: err = nil, want error")
	}
}

// TestBumpRunTurn_IncrementsAndReturnsNewValue asserts BumpRunTurn increments
// turn_count and returns the new value, so the dispatcher can track
// continuation turns against cfg.TurnCap without a separate read.
func TestBumpRunTurn_IncrementsAndReturnsNewValue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "running"}); err != nil {
		t.Fatalf("UpsertIssue: unexpected error: %v", err)
	}
	if err := s.InsertRun(ctx, store.Run{
		RunID:     "run-1",
		IssueID:   "issue-1",
		Lane:      "coder",
		Status:    "running",
		StartedAt: 100,
		Attempt:   1,
		TurnCount: 1,
	}); err != nil {
		t.Fatalf("InsertRun: unexpected error: %v", err)
	}

	newTurn, err := s.BumpRunTurn(ctx, "run-1")
	if err != nil {
		t.Fatalf("BumpRunTurn: unexpected error: %v", err)
	}
	if newTurn != 2 {
		t.Fatalf("BumpRunTurn = %d, want 2", newTurn)
	}

	newTurn2, err := s.BumpRunTurn(ctx, "run-1")
	if err != nil {
		t.Fatalf("BumpRunTurn (2nd): unexpected error: %v", err)
	}
	if newTurn2 != 3 {
		t.Fatalf("BumpRunTurn (2nd) = %d, want 3", newTurn2)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].LatestRun == nil || snap.Issues[0].LatestRun.TurnCount != 3 {
		t.Errorf("LatestRun.TurnCount = %+v, want 3", snap.Issues[0].LatestRun)
	}
}

func TestBumpRunTurn_NoSuchRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.BumpRunTurn(ctx, "no-such-run"); err == nil {
		t.Fatalf("BumpRunTurn on nonexistent run: err = nil, want error")
	}
}

// TestReadSnapshot_FlagsUnmirroredIssues asserts that ReadSnapshot marks an
// issue Unmirrored=true iff it has at least one pending linear_writes row
// (A2's outbox), and that Snapshot.UnmirroredCount reflects the total number
// of such issues. issue-2's write is marked done, so it must not be flagged
// even though it has a linear_writes row at all.
func TestReadSnapshot_FlagsUnmirroredIssues(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, issue := range []store.Issue{
		{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "running"},
		{ID: "issue-2", Identifier: "CLP-2", BoardStatus: "review"},
		{ID: "issue-3", Identifier: "CLP-3", BoardStatus: "ready"},
	} {
		if err := s.UpsertIssue(ctx, issue); err != nil {
			t.Fatalf("UpsertIssue(%s): unexpected error: %v", issue.ID, err)
		}
	}

	// issue-1 has a pending mirror write (Linear was unreachable).
	if err := s.EnqueueLinearSetState(ctx, "issue-1", "running", 100); err != nil {
		t.Fatalf("EnqueueLinearSetState(issue-1): unexpected error: %v", err)
	}

	// issue-2 has a write too, but it already mirrored successfully.
	if err := s.EnqueueLinearSetState(ctx, "issue-2", "review", 100); err != nil {
		t.Fatalf("EnqueueLinearSetState(issue-2): unexpected error: %v", err)
	}
	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	var issue2WriteID int64
	for _, w := range pending {
		if w.IssueID == "issue-2" {
			issue2WriteID = w.ID
		}
	}
	if issue2WriteID == 0 {
		t.Fatalf("no pending linear_writes row found for issue-2")
	}
	if err := s.MarkLinearWriteDone(ctx, issue2WriteID, 200); err != nil {
		t.Fatalf("MarkLinearWriteDone: unexpected error: %v", err)
	}

	// issue-3 has no linear_writes row at all.

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}

	byID := make(map[string]store.IssueSnapshot)
	for _, is := range snap.Issues {
		byID[is.ID] = is
	}

	if !byID["issue-1"].Unmirrored {
		t.Errorf("issue-1.Unmirrored = false, want true (has pending linear_writes row)")
	}
	if byID["issue-2"].Unmirrored {
		t.Errorf("issue-2.Unmirrored = true, want false (write marked done)")
	}
	if byID["issue-3"].Unmirrored {
		t.Errorf("issue-3.Unmirrored = true, want false (no linear_writes row)")
	}

	if snap.UnmirroredCount != 1 {
		t.Errorf("UnmirroredCount = %d, want 1", snap.UnmirroredCount)
	}
}
