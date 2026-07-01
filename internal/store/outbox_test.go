package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/xlyk/clipse/internal/store"
)

// seedRunningIssueWithRun seeds an issue already claimed and running, with
// one open run, ready to be transitioned.
func seedRunningIssueWithRun(t *testing.T, s *store.Store, issueID, runID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertIssue(ctx, store.Issue{
		ID:           issueID,
		Identifier:   issueID,
		LaneLabel:    "agent:coder",
		BoardStatus:  "running",
		Deps:         `[]`,
		BranchName:   issueID + "-branch",
		ClaimLock:    sql.NullString{String: runID, Valid: true},
		ClaimExpires: sql.NullInt64{Int64: 9999, Valid: true},
		UpdatedAt:    100,
		LastSeen:     100,
		CreatedAt:    100,
	}); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}
	if err := s.InsertRun(ctx, store.Run{
		RunID:     runID,
		IssueID:   issueID,
		Lane:      "coder",
		Status:    "running",
		StartedAt: 100,
		Attempt:   1,
		ThreadID:  "thread-1",
	}); err != nil {
		t.Fatalf("seed InsertRun: unexpected error: %v", err)
	}
}

// TestTransition_HappyPath asserts that a single Transition call atomically:
// updates board_status, clears the claim (when requested), closes the run,
// appends the audit event, and enqueues the right linear_writes rows — all
// visible immediately after the call returns.
func TestTransition_HappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	req := store.TransitionReq{
		IssueID:    "issue-1",
		NewStatus:  "review",
		ClearClaim: true,
		CloseRunID: "run-1",
		RunStatus:  "done",
		ResultJSON: `{"outcome":"done"}`,
		TokensIn:   10,
		TokensOut:  20,
		Event: store.Event{
			Ts:      500,
			IssueID: sql.NullString{String: "issue-1", Valid: true},
			RunID:   sql.NullString{String: "run-1", Valid: true},
			Kind:    "transitioned",
			Detail:  "issue-1 -> review",
		},
		EnqueueSetState: true,
		Comment:         "worker finished, ready for review",
	}
	if err := s.Transition(ctx, req); err != nil {
		t.Fatalf("Transition: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if len(snap.Issues) != 1 {
		t.Fatalf("len(snap.Issues) = %d, want 1", len(snap.Issues))
	}
	got := snap.Issues[0]
	if got.BoardStatus != "review" {
		t.Errorf("BoardStatus = %q, want %q", got.BoardStatus, "review")
	}
	if got.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want false (ClearClaim requested)")
	}
	if got.ClaimExpires.Valid {
		t.Errorf("ClaimExpires.Valid = true, want false (ClearClaim requested)")
	}
	if got.LatestRun == nil {
		t.Fatalf("LatestRun = nil, want closed run")
	}
	if got.LatestRun.Status != "done" {
		t.Errorf("LatestRun.Status = %q, want %q", got.LatestRun.Status, "done")
	}
	if !got.LatestRun.ResultJSON.Valid || got.LatestRun.ResultJSON.String != `{"outcome":"done"}` {
		t.Errorf("LatestRun.ResultJSON = %+v, want valid result", got.LatestRun.ResultJSON)
	}
	if got.LatestRun.TokensIn != 10 || got.LatestRun.TokensOut != 20 {
		t.Errorf("LatestRun tokens = (%d,%d), want (10,20)", got.LatestRun.TokensIn, got.LatestRun.TokensOut)
	}

	events, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: unexpected error: %v", err)
	}
	var foundEvent bool
	for _, e := range events {
		if e.Kind == "transitioned" && e.Detail == "issue-1 -> review" {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Errorf("no 'transitioned' event found; events = %+v", events)
	}

	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("len(pending linear_writes) = %d, want 2 (setstate + comment)", len(pending))
	}
	var sawSetState, sawComment bool
	for _, w := range pending {
		if w.IssueID != "issue-1" {
			t.Errorf("linear_write.IssueID = %q, want %q", w.IssueID, "issue-1")
		}
		if w.Status != "pending" {
			t.Errorf("linear_write.Status = %q, want pending", w.Status)
		}
		switch w.Kind {
		case "setstate":
			sawSetState = true
			if w.Target != "review" {
				t.Errorf("setstate Target = %q, want %q", w.Target, "review")
			}
		case "comment":
			sawComment = true
			if w.Body != "worker finished, ready for review" {
				t.Errorf("comment Body = %q, want seeded comment", w.Body)
			}
		default:
			t.Errorf("unexpected linear_write.Kind = %q", w.Kind)
		}
	}
	if !sawSetState {
		t.Errorf("no setstate linear_write enqueued")
	}
	if !sawComment {
		t.Errorf("no comment linear_write enqueued")
	}
}

// TestTransition_WithoutCloseRunOrOutbox asserts optional fields (CloseRunID,
// EnqueueSetState, Comment) are all skippable: a transition that only moves
// board_status and clears no claim must not touch runs or linear_writes.
func TestTransition_WithoutCloseRunOrOutbox(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	req := store.TransitionReq{
		IssueID:   "issue-1",
		NewStatus: "blocked",
		Event: store.Event{
			Ts:      500,
			IssueID: sql.NullString{String: "issue-1", Valid: true},
			Kind:    "transitioned",
			Detail:  "issue-1 -> blocked",
		},
	}
	if err := s.Transition(ctx, req); err != nil {
		t.Fatalf("Transition: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	got := snap.Issues[0]
	if got.BoardStatus != "blocked" {
		t.Errorf("BoardStatus = %q, want %q", got.BoardStatus, "blocked")
	}
	// ClearClaim was false: claim survives the transition.
	if !got.ClaimLock.Valid || got.ClaimLock.String != "run-1" {
		t.Errorf("ClaimLock = %+v, want preserved %q", got.ClaimLock, "run-1")
	}
	if got.LatestRun == nil || got.LatestRun.Status != "running" {
		t.Errorf("LatestRun = %+v, want status still 'running' (CloseRunID unset)", got.LatestRun)
	}

	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("len(pending) = %d, want 0 (no outbox rows requested)", len(pending))
	}
}

// TestTransition_EmptyErrorAndResultStoredAsNull matches CloseRun's existing
// NULL-vs-empty-string convention: an empty RunError/ResultJSON on the
// TransitionReq must land as NULL, not "".
func TestTransition_EmptyErrorAndResultStoredAsNull(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	req := store.TransitionReq{
		IssueID:    "issue-1",
		NewStatus:  "blocked",
		ClearClaim: true,
		CloseRunID: "run-1",
		RunStatus:  "blocked",
		RunError:   "agent got stuck",
		Event: store.Event{
			Ts:      500,
			IssueID: sql.NullString{String: "issue-1", Valid: true},
			Kind:    "transitioned",
			Detail:  "issue-1 -> blocked",
		},
	}
	if err := s.Transition(ctx, req); err != nil {
		t.Fatalf("Transition: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	latest := snap.Issues[0].LatestRun
	if latest.ResultJSON.Valid {
		t.Errorf("LatestRun.ResultJSON.Valid = true, want false (empty result)")
	}
	if !latest.Error.Valid || latest.Error.String != "agent got stuck" {
		t.Errorf("LatestRun.Error = %+v, want valid %q", latest.Error, "agent got stuck")
	}
}

// TestDrainPendingLinearWrites_OrderedByID asserts drained rows come back in
// enqueue order (ascending id), so retries process in FIFO order.
func TestDrainPendingLinearWrites_OrderedByID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	for _, status := range []string{"in_progress", "review", "done"} {
		req := store.TransitionReq{
			IssueID:         "issue-1",
			NewStatus:       status,
			EnqueueSetState: true,
			Event: store.Event{
				Ts:      500,
				IssueID: sql.NullString{String: "issue-1", Valid: true},
				Kind:    "transitioned",
				Detail:  "issue-1 -> " + status,
			},
		}
		if err := s.Transition(ctx, req); err != nil {
			t.Fatalf("Transition(%s): unexpected error: %v", status, err)
		}
	}

	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("len(pending) = %d, want 3", len(pending))
	}
	wantOrder := []string{"in_progress", "review", "done"}
	for i, w := range pending {
		if w.Target != wantOrder[i] {
			t.Errorf("pending[%d].Target = %q, want %q", i, w.Target, wantOrder[i])
		}
		if i > 0 && pending[i-1].ID >= w.ID {
			t.Errorf("pending ids not ascending: [%d]=%d then [%d]=%d", i-1, pending[i-1].ID, i, w.ID)
		}
	}
}

// TestDrainPendingLinearWrites_RespectsLimit asserts the limit parameter
// caps how many rows are returned.
func TestDrainPendingLinearWrites_RespectsLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	for i := 0; i < 5; i++ {
		req := store.TransitionReq{
			IssueID:         "issue-1",
			NewStatus:       "review",
			EnqueueSetState: true,
			Event:           store.Event{Ts: int64(i), Kind: "transitioned"},
		}
		if err := s.Transition(ctx, req); err != nil {
			t.Fatalf("Transition: unexpected error: %v", err)
		}
	}

	pending, err := s.DrainPendingLinearWrites(ctx, 2)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("len(pending) = %d, want 2", len(pending))
	}
}

// TestMarkLinearWriteDone_RemovesFromPending asserts a marked-done row no
// longer shows up in DrainPendingLinearWrites.
func TestMarkLinearWriteDone_RemovesFromPending(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	req := store.TransitionReq{
		IssueID:         "issue-1",
		NewStatus:       "review",
		EnqueueSetState: true,
		Event:           store.Event{Ts: 1, Kind: "transitioned"},
	}
	if err := s.Transition(ctx, req); err != nil {
		t.Fatalf("Transition: unexpected error: %v", err)
	}

	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("DrainPendingLinearWrites precondition: pending=%v err=%v", pending, err)
	}

	if err := s.MarkLinearWriteDone(ctx, pending[0].ID); err != nil {
		t.Fatalf("MarkLinearWriteDone: unexpected error: %v", err)
	}

	after, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("len(pending after MarkDone) = %d, want 0", len(after))
	}
}

// TestMarkLinearWriteFailed_BumpsAttemptsAndStaysPending asserts a failed
// write increments attempts, records last_error, and remains pending so the
// dispatcher retries it on a later tick.
func TestMarkLinearWriteFailed_BumpsAttemptsAndStaysPending(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	req := store.TransitionReq{
		IssueID:         "issue-1",
		NewStatus:       "review",
		EnqueueSetState: true,
		Event:           store.Event{Ts: 1, Kind: "transitioned"},
	}
	if err := s.Transition(ctx, req); err != nil {
		t.Fatalf("Transition: unexpected error: %v", err)
	}

	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("DrainPendingLinearWrites precondition: pending=%v err=%v", pending, err)
	}
	id := pending[0].ID

	if err := s.MarkLinearWriteFailed(ctx, id, "linear api 500"); err != nil {
		t.Fatalf("MarkLinearWriteFailed: unexpected error: %v", err)
	}

	after, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("len(pending after MarkFailed) = %d, want 1 (still pending)", len(after))
	}
	got := after[0]
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}
	if !got.LastError.Valid || got.LastError.String != "linear api 500" {
		t.Errorf("LastError = %+v, want valid %q", got.LastError, "linear api 500")
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q, want pending", got.Status)
	}

	// A second failure bumps attempts again.
	if err := s.MarkLinearWriteFailed(ctx, id, "linear api 500 again"); err != nil {
		t.Fatalf("MarkLinearWriteFailed (2nd): unexpected error: %v", err)
	}
	after2, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if after2[0].Attempts != 2 {
		t.Errorf("Attempts after 2nd failure = %d, want 2", after2[0].Attempts)
	}
}
