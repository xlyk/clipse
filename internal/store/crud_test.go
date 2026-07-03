package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/xlyk/clipse/internal/store"
)

// TestReadSnapshot_CumulativeTokensAcrossRuns asserts token totals sum EVERY
// run of an issue, not just its latest. An issue accrues one run per lane
// (coder, then reviewer, then git-operator), so counting only LatestRun (as
// the TUI header used to) silently dropped every earlier lane's tokens —
// which read as "token counts not updating" once a card moved past its coder run.
func TestReadSnapshot_CumulativeTokensAcrossRuns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "done"}); err != nil {
		t.Fatalf("UpsertIssue issue-1: %v", err)
	}
	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-2", Identifier: "CLP-2", BoardStatus: "running"}); err != nil {
		t.Fatalf("UpsertIssue issue-2: %v", err)
	}
	// issue-1: three completed lane runs.
	for _, r := range []store.Run{
		{RunID: "r1", IssueID: "issue-1", Lane: "coder", Status: "needs_review", StartedAt: 10, Attempt: 1, TokensIn: 100, TokensOut: 200},
		{RunID: "r2", IssueID: "issue-1", Lane: "reviewer", Status: "done", StartedAt: 20, Attempt: 1, TokensIn: 50, TokensOut: 60},
		{RunID: "r3", IssueID: "issue-1", Lane: "git_operator", Status: "done", StartedAt: 30, Attempt: 1, TokensIn: 30, TokensOut: 40},
	} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun %s: %v", r.RunID, err)
		}
	}
	// issue-2: one run.
	if err := s.InsertRun(ctx, store.Run{RunID: "r4", IssueID: "issue-2", Lane: "coder", Status: "running", StartedAt: 40, Attempt: 1, TokensIn: 7, TokensOut: 3}); err != nil {
		t.Fatalf("InsertRun r4: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}

	byID := map[string]store.IssueSnapshot{}
	for _, is := range snap.Issues {
		byID[is.ID] = is
	}
	if got := byID["issue-1"].TokensInTotal; got != 180 {
		t.Errorf("issue-1 TokensInTotal = %d, want 180 (100+50+30, all runs)", got)
	}
	if got := byID["issue-1"].TokensOutTotal; got != 300 {
		t.Errorf("issue-1 TokensOutTotal = %d, want 300 (200+60+40)", got)
	}
	if snap.TotalTokensIn != 187 {
		t.Errorf("Snapshot.TotalTokensIn = %d, want 187 (180+7, board-wide)", snap.TotalTokensIn)
	}
	if snap.TotalTokensOut != 303 {
		t.Errorf("Snapshot.TotalTokensOut = %d, want 303 (300+3)", snap.TotalTokensOut)
	}
}

// TestReadSnapshot_RecentEventsAndLastEventAt asserts ReadSnapshot loads the
// trailing events newest-first, capped at recentEventLimit (15), and reports
// LastEventAt as the max ts across them — the data the TUI liveness signal and
// activity feed read.
func TestReadSnapshot_RecentEventsAndLastEventAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "running"}); err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}

	// Append 20 events with increasing ts so the newest-first cap (15) drops
	// the 5 oldest, and LastEventAt lands on the final append.
	const n = 20
	for i := 1; i <= n; i++ {
		if err := s.AppendEvent(ctx, store.Event{
			Ts:      int64(1000 + i),
			IssueID: sql.NullString{String: "issue-1", Valid: true},
			Kind:    "claimed",
			Detail:  fmt.Sprintf("event %d", i),
		}); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}

	if got, want := len(snap.RecentEvents), 15; got != want {
		t.Fatalf("len(RecentEvents) = %d, want %d (recentEventLimit cap)", got, want)
	}
	// Newest-first: the first element is the last-appended event (ts 1020).
	if got, want := snap.RecentEvents[0].Detail, "event 20"; got != want {
		t.Errorf("RecentEvents[0].Detail = %q, want %q (newest-first)", got, want)
	}
	if got, want := snap.RecentEvents[14].Detail, "event 6"; got != want {
		t.Errorf("RecentEvents[14].Detail = %q, want %q (oldest retained)", got, want)
	}
	if got, want := snap.LastEventAt, int64(1000+n); got != want {
		t.Errorf("LastEventAt = %d, want %d (max ts)", got, want)
	}
}

// TestReadSnapshot_NoEvents asserts an empty events table yields no recent
// events and a zero LastEventAt (the "no activity yet" liveness reading).
func TestReadSnapshot_NoEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "ready"}); err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if len(snap.RecentEvents) != 0 {
		t.Errorf("len(RecentEvents) = %d, want 0", len(snap.RecentEvents))
	}
	if snap.LastEventAt != 0 {
		t.Errorf("LastEventAt = %d, want 0", snap.LastEventAt)
	}
}

// TestReadSnapshot_IssueRuns asserts each IssueSnapshot carries every one of
// its runs in chronological order (oldest lane first), so the TUI detail view
// can render the full per-lane history — while LatestRun still points at the
// most recent run.
func TestReadSnapshot_IssueRuns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "done"}); err != nil {
		t.Fatalf("UpsertIssue issue-1: %v", err)
	}
	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-2", Identifier: "CLP-2", BoardStatus: "ready"}); err != nil {
		t.Fatalf("UpsertIssue issue-2: %v", err)
	}

	// issue-1: three lane runs, inserted out of chronological order to prove
	// the query sorts by started_at rather than insertion order.
	for _, r := range []store.Run{
		{RunID: "r3", IssueID: "issue-1", Lane: "git_operator", Status: "done", StartedAt: 30, Attempt: 1},
		{RunID: "r1", IssueID: "issue-1", Lane: "coder", Status: "needs_review", StartedAt: 10, Attempt: 1},
		{RunID: "r2", IssueID: "issue-1", Lane: "reviewer", Status: "done", StartedAt: 20, Attempt: 1},
	} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun %s: %v", r.RunID, err)
		}
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}

	byID := map[string]store.IssueSnapshot{}
	for _, is := range snap.Issues {
		byID[is.ID] = is
	}

	got := byID["issue-1"].Runs
	if len(got) != 3 {
		t.Fatalf("issue-1 Runs len = %d, want 3", len(got))
	}
	wantOrder := []string{"coder", "reviewer", "git_operator"}
	for i, lane := range wantOrder {
		if got[i].Lane != lane {
			t.Errorf("issue-1 Runs[%d].Lane = %q, want %q (chronological)", i, got[i].Lane, lane)
		}
	}
	if lr := byID["issue-1"].LatestRun; lr == nil || lr.RunID != "r3" {
		t.Errorf("issue-1 LatestRun = %+v, want run r3 (most recent)", lr)
	}
	if got := byID["issue-2"].Runs; len(got) != 0 {
		t.Errorf("issue-2 Runs len = %d, want 0 (no runs)", len(got))
	}
}

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
		Title:       "Add the thing",
		Description: "Implement the thing that does the stuff.",
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
		got.Deps != issue.Deps || got.Priority != issue.Priority ||
		got.Title != issue.Title || got.Description != issue.Description {
		t.Errorf("GetIssue = %+v, want %+v", got, issue)
	}
}

// TestUpsertIssue_TitleDescription_InsertAndConflictUpdate asserts title and
// description are written on the initial insert, updated on a later
// conflicting upsert (a re-poll picking up an edited Linear issue) alongside
// the other Linear-sourced fields, while board_status -- dispatcher-owned
// runtime state -- stays preserved across that same conflict. Title/
// description feed the worker's CLIPSE_ISSUE_TEXT (Phase-2 issue-text
// plumbing), so they must behave like every other Linear-sourced field, not
// like claim state.
func TestUpsertIssue_TitleDescription_InsertAndConflictUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	seeded := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		Title:       "Original title",
		Description: "Original description.",
		LaneLabel:   "coder",
		BoardStatus: "running",
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "clp-1-do-thing",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, seeded); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.Title != "Original title" || got.Description != "Original description." {
		t.Errorf("after insert: Title=%q Description=%q, want %q / %q",
			got.Title, got.Description, "Original title", "Original description.")
	}

	// A re-poll of Linear: the issue was edited (new title/description) and
	// Linear still reports its pre-claim status ("ready"), the same kind of
	// staleness TestUpsertIssue_ConflictPreservesDispatcherState covers for
	// the other fields.
	edited := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		Title:       "Edited title",
		Description: "Edited description.",
		LaneLabel:   "coder",
		BoardStatus: "ready",
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "clp-1-do-thing",
		UpdatedAt:   200,
		LastSeen:    200,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, edited); err != nil {
		t.Fatalf("conflict UpsertIssue: unexpected error: %v", err)
	}

	got2, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue after conflict: unexpected error: %v", err)
	}
	if got2.Title != "Edited title" {
		t.Errorf("Title = %q, want updated %q", got2.Title, "Edited title")
	}
	if got2.Description != "Edited description." {
		t.Errorf("Description = %q, want updated %q", got2.Description, "Edited description.")
	}
	if got2.BoardStatus != "running" {
		t.Errorf("BoardStatus = %q, want preserved %q (dispatcher-owned; a poll must not reset it)", got2.BoardStatus, "running")
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

// TestLatestReworkFeedback covers the read the dispatcher uses to thread the
// most recent review/rework verdict into a Coder re-run's prompt: the newest
// changes_requested run's summary (from result_json), an error fallback when
// result_json carries no summary, and "" when the issue never had a
// changes_requested run at all (so a fresh coder claim threads nothing).
func TestLatestReworkFeedback(t *testing.T) {
	ns := func(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }

	tests := []struct {
		name string
		runs []store.Run
		want string
	}{
		{
			name: "newest changes_requested summary wins over older ones and non-rework runs",
			runs: []store.Run{
				{RunID: "r1", Lane: "coder", Status: "needs_review", StartedAt: 10, ResultJSON: ns(`{"summary":"coder turn 1"}`)},
				{RunID: "r2", Lane: "reviewer", Status: "changes_requested", StartedAt: 20, ResultJSON: ns(`{"summary":"first review: needs work"}`)},
				{RunID: "r3", Lane: "coder", Status: "needs_review", StartedAt: 25, ResultJSON: ns(`{"summary":"coder turn 2"}`)},
				{RunID: "r4", Lane: "reviewer", Status: "changes_requested", StartedAt: 30, ResultJSON: ns(`{"summary":"remove the fabricated clipse.yaml section"}`)},
			},
			want: "remove the fabricated clipse.yaml section",
		},
		{
			name: "falls back to error when result_json is absent",
			runs: []store.Run{
				{RunID: "r1", Lane: "git_operator", Status: "changes_requested", StartedAt: 10, Error: ns("stale base conflict (conflicting files: a.go)")},
			},
			want: "stale base conflict (conflicting files: a.go)",
		},
		{
			name: "empty when the issue never had a changes_requested run",
			runs: []store.Run{
				{RunID: "r1", Lane: "coder", Status: "needs_review", StartedAt: 10, ResultJSON: ns(`{"summary":"coder turn 1"}`)},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "rework"}); err != nil {
				t.Fatalf("UpsertIssue: unexpected error: %v", err)
			}
			for _, r := range tt.runs {
				r.IssueID = "issue-1"
				if err := s.InsertRun(ctx, r); err != nil {
					t.Fatalf("InsertRun %s: unexpected error: %v", r.RunID, err)
				}
			}

			got, err := s.LatestReworkFeedback(ctx, "issue-1")
			if err != nil {
				t.Fatalf("LatestReworkFeedback: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("LatestReworkFeedback = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLatestReworkFeedback_UnknownIssue returns "" (no error) for an issue
// with no runs at all, matching the "fresh claim threads nothing" contract.
func TestLatestReworkFeedback_UnknownIssue(t *testing.T) {
	s := openTestStore(t)
	got, err := s.LatestReworkFeedback(context.Background(), "no-such-issue")
	if err != nil {
		t.Fatalf("LatestReworkFeedback: unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("LatestReworkFeedback = %q, want empty", got)
	}
}
