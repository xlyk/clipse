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
