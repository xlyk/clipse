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

func TestTransition_TerminalStatusAtomicallyQueuesCoderCleanup(t *testing.T) {
	for _, status := range []string{"done", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			seedRunningIssueWithRun(t, s, "issue-1", "run-1")
			coder := store.AgentWorkspace{
				OwnerKey: "daytona:xlyk/clipse:coder:issue-1", IssueID: "issue-1", RunID: "run-1",
				Provider: "daytona", Role: "coder", ExternalID: "sb-coder", WorkspacePath: "/workspace",
				State: store.WorkspaceActive, LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
			}
			reviewer := store.AgentWorkspace{
				OwnerKey: "daytona:xlyk/clipse:reviewer:issue-1:review-1", IssueID: "issue-1", RunID: "review-1",
				Provider: "daytona", Role: "reviewer", ExternalID: "sb-reviewer", WorkspacePath: "/workspace",
				State: store.WorkspaceActive, LastAction: "ensure", CreatedAt: 11, UpdatedAt: 11,
			}
			for _, workspace := range []store.AgentWorkspace{coder, reviewer} {
				if err := s.UpsertAgentWorkspace(ctx, workspace); err != nil {
					t.Fatal(err)
				}
			}

			if err := s.Transition(ctx, store.TransitionReq{
				IssueID: "issue-1", NewStatus: status, ClearClaim: true,
				CloseRunID: "run-1", RunStatus: "done", CleanupCoderWorkspace: true,
				Event: store.Event{Ts: 500, Kind: "terminal"},
			}); err != nil {
				t.Fatal(err)
			}

			issue, err := s.GetIssue(ctx, "issue-1")
			if err != nil {
				t.Fatal(err)
			}
			if issue.BoardStatus != status {
				t.Fatalf("board status = %q, want %q", issue.BoardStatus, status)
			}
			rows, err := s.AgentWorkspacesByIssue(ctx, "issue-1")
			if err != nil {
				t.Fatal(err)
			}
			if rows[0].State != store.WorkspaceCleanupPending || rows[0].UpdatedAt != 500 {
				t.Fatalf("coder workspace = %+v", rows[0])
			}
			if rows[1].State != store.WorkspaceActive {
				t.Fatalf("reviewer workspace changed = %+v", rows[1])
			}
		})
	}
}

func TestTransition_TerminalStatusQueuesLocalCoderCleanup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")
	workspace := store.AgentWorkspace{
		OwnerKey: "local:coder:issue-1", IssueID: "issue-1", Provider: "local", Role: "coder",
		WorkspacePath: "/workspace", State: store.WorkspaceActive, LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
	}
	if err := s.UpsertAgentWorkspace(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, store.TransitionReq{
		IssueID: "issue-1", NewStatus: "done", ClearClaim: true, CloseRunID: "run-1", RunStatus: "done",
		CleanupCoderWorkspace: true, CleanupWorkspaceProvider: "local", Event: store.Event{Ts: 500, Kind: "terminal"},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.AgentWorkspacesByIssue(ctx, "issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != store.WorkspaceCleanupPending {
		t.Fatalf("local terminal workspace = %+v", rows)
	}
}

func TestHasPendingLinearSetState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.EnqueueLinearSetState(ctx, "issue-1", "done", 10); err != nil {
		t.Fatal(err)
	}
	has, err := s.HasPendingLinearSetState(ctx, "issue-1")
	if err != nil || !has {
		t.Fatalf("issue-1 pending setstate = %v, err=%v", has, err)
	}
	has, err = s.HasPendingLinearSetStateTarget(ctx, "issue-1", "done")
	if err != nil || !has {
		t.Fatalf("issue-1 pending done setstate = %v, err=%v", has, err)
	}
	has, err = s.HasPendingLinearSetStateTarget(ctx, "issue-1", "ready")
	if err != nil || has {
		t.Fatalf("issue-1 pending ready setstate = %v, err=%v", has, err)
	}
	has, err = s.HasPendingLinearSetState(ctx, "issue-2")
	if err != nil || has {
		t.Fatalf("issue-2 pending setstate = %v, err=%v", has, err)
	}
	writes, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkLinearWriteDone(ctx, writes[0].ID, 20); err != nil {
		t.Fatal(err)
	}
	has, err = s.HasPendingLinearSetState(ctx, "issue-1")
	if err != nil || has {
		t.Fatalf("completed issue-1 setstate still pending = %v, err=%v", has, err)
	}
	has, err = s.HasPendingLinearSetStateTarget(ctx, "issue-1", "done")
	if err != nil || has {
		t.Fatalf("completed issue-1 done setstate still pending = %v, err=%v", has, err)
	}
}

func TestDrainPendingLinearWriteHeadsReturnsOldestPerIssue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, item := range []struct {
		issue  string
		target string
	}{
		{issue: "issue-a", target: "ready"},
		{issue: "issue-a", target: "done"},
		{issue: "issue-a", target: "blocked"},
		{issue: "issue-b", target: "review"},
	} {
		if err := s.EnqueueLinearSetState(ctx, item.issue, item.target, 10); err != nil {
			t.Fatal(err)
		}
	}
	heads, err := s.DrainPendingLinearWriteHeads(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != 2 {
		t.Fatalf("heads = %+v, want one row for each issue", heads)
	}
	if heads[0].IssueID != "issue-a" || heads[0].Target != "ready" {
		t.Fatalf("issue-a head = %+v, want oldest ready", heads[0])
	}
	if heads[1].IssueID != "issue-b" || heads[1].Target != "review" {
		t.Fatalf("issue-b head = %+v, want review despite three earlier issue-a rows", heads[1])
	}
}

func TestDrainPendingLinearWriteHeadsKeepsSameIssueBlockedUntilHeadSucceeds(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.EnqueueLinearSetState(ctx, "issue-a", "ready", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueLinearSetState(ctx, "issue-a", "done", 11); err != nil {
		t.Fatal(err)
	}
	heads, err := s.DrainPendingLinearWriteHeads(ctx, 10)
	if err != nil || len(heads) != 1 || heads[0].Target != "ready" {
		t.Fatalf("initial heads = %+v, err=%v", heads, err)
	}
	if err := s.MarkLinearWriteFailed(ctx, heads[0].ID, "still failing", 100); err != nil {
		t.Fatal(err)
	}
	heads, err = s.DrainPendingLinearWriteHeads(ctx, 10)
	if err != nil || len(heads) != 1 || heads[0].Target != "ready" {
		t.Fatalf("failed head was overtaken: heads=%+v err=%v", heads, err)
	}
	if err := s.MarkLinearWriteDone(ctx, heads[0].ID, 200); err != nil {
		t.Fatal(err)
	}
	heads, err = s.DrainPendingLinearWriteHeads(ctx, 10)
	if err != nil || len(heads) != 1 || heads[0].Target != "done" {
		t.Fatalf("next write did not become head after success: heads=%+v err=%v", heads, err)
	}
}

func TestTransition_CoderCleanupFailureRollsBackTerminalTransition(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")
	workspace := store.AgentWorkspace{
		OwnerKey: "daytona:xlyk/clipse:coder:issue-1", IssueID: "issue-1", RunID: "run-1",
		Provider: "daytona", Role: "coder", ExternalID: "sb-1", WorkspacePath: "/workspace",
		State: store.WorkspaceActive, LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
	}
	if err := s.UpsertAgentWorkspace(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_cleanup BEFORE UPDATE OF state ON agent_workspaces
		WHEN NEW.state = 'cleanup_pending'
		BEGIN SELECT RAISE(ABORT, 'reject cleanup'); END
	`); err != nil {
		t.Fatal(err)
	}

	err := s.Transition(ctx, store.TransitionReq{
		IssueID: "issue-1", NewStatus: "done", ClearClaim: true,
		CloseRunID: "run-1", RunStatus: "done", CleanupCoderWorkspace: true,
		Event: store.Event{Ts: 500, Kind: "terminal"},
	})
	if err == nil {
		t.Fatal("Transition succeeded despite cleanup scheduling failure")
	}
	issue, getErr := s.GetIssue(ctx, "issue-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if issue.BoardStatus != "running" || !issue.ClaimLock.Valid {
		t.Fatalf("terminal issue update was not rolled back: %+v", issue)
	}
	rows, getErr := s.AgentWorkspacesByIssue(ctx, "issue-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if rows[0].State != store.WorkspaceActive {
		t.Fatalf("workspace update was not rolled back: %+v", rows[0])
	}
	if events, getErr := s.ListEvents(ctx); getErr != nil || len(events) != 0 {
		t.Fatalf("events after rollback = %+v, err=%v", events, getErr)
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

// TestTransition_ReworkCount_IncrementsOnReworkAndResetsOnDone asserts that
// Transition bumps issues.rework_count every time a card lands in the
// rework column (amendment C1: the Reviewer lane's changes_requested from
// review, and the Git-operator lane's stale-base-conflict route from
// merging both land on NewStatus="rework", and both mean "the Coder lane
// gets another attempt") and resets it to zero once the card reaches done --
// all inside the same atomic transition, so no separate dispatcher-side
// bookkeeping call can fall out of sync with the board move itself. A
// Linear re-poll landing mid-cycle (UpsertIssue's conflict path) must not
// reset it either, since it is dispatcher-owned runtime state like
// board_status/claim_lock.
func TestTransition_ReworkCount_IncrementsOnReworkAndResetsOnDone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	reworkReq := func(ts int64) store.TransitionReq {
		return store.TransitionReq{
			IssueID:   "issue-1",
			NewStatus: "rework",
			Event:     store.Event{Ts: ts, Kind: "transitioned"},
		}
	}

	if err := s.Transition(ctx, reworkReq(1)); err != nil {
		t.Fatalf("Transition (1st rework): unexpected error: %v", err)
	}
	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.ReworkCount != 1 {
		t.Fatalf("ReworkCount after 1st rework = %d, want 1", got.ReworkCount)
	}

	// A Linear re-poll mid-cycle must not reset the count.
	if err := s.UpsertIssue(ctx, store.Issue{
		ID:          "issue-1",
		Identifier:  "issue-1",
		LaneLabel:   "coder",
		BoardStatus: "todo", // stale/irrelevant to this assertion; UpsertIssue never writes board_status/rework_count on conflict
		Deps:        `[]`,
		BranchName:  "issue-1-branch",
		UpdatedAt:   50,
		LastSeen:    50,
		CreatedAt:   100,
	}); err != nil {
		t.Fatalf("re-poll UpsertIssue: unexpected error: %v", err)
	}
	got, err = s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue after re-poll: unexpected error: %v", err)
	}
	if got.ReworkCount != 1 {
		t.Fatalf("ReworkCount after re-poll = %d, want preserved 1", got.ReworkCount)
	}

	if err := s.Transition(ctx, reworkReq(2)); err != nil {
		t.Fatalf("Transition (2nd rework): unexpected error: %v", err)
	}
	got, err = s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.ReworkCount != 2 {
		t.Fatalf("ReworkCount after 2nd rework = %d, want 2", got.ReworkCount)
	}

	if err := s.Transition(ctx, store.TransitionReq{
		IssueID:   "issue-1",
		NewStatus: "done",
		Event:     store.Event{Ts: 3, Kind: "transitioned"},
	}); err != nil {
		t.Fatalf("Transition (done): unexpected error: %v", err)
	}
	got, err = s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue after done: unexpected error: %v", err)
	}
	if got.ReworkCount != 0 {
		t.Errorf("ReworkCount after done = %d, want reset to 0", got.ReworkCount)
	}

	// ReadSnapshot must agree with GetIssue.
	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].ReworkCount != 0 {
		t.Errorf("ReadSnapshot ReworkCount = %d, want reset to 0", snap.Issues[0].ReworkCount)
	}
}

// TestTransition_AutoRetryRequeue_BumpsAttemptsAndSetsBackoff asserts the
// store side of auto-unblock layer 1's re-queue path (dispatcher.scheduleRetry):
// one Transition atomically returns the card to its release column, clears the
// claim, closes the run as retry_scheduled, bumps recover_attempts, sets a
// future blocked_until, and enqueues the Linear mirror + comment — while
// leaving rework_count untouched (SkipReworkBump), so a transient retry never
// spends amendment C1's rework budget even when the release column is "rework".
func TestTransition_AutoRetryRequeue_BumpsAttemptsAndSetsBackoff(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	const (
		now          = 1000
		backoff      = 30
		blockedUntil = now + backoff
	)
	req := store.TransitionReq{
		IssueID:             "issue-1",
		NewStatus:           "ready", // store.ReleaseTargetColumn("running")
		ClearClaim:          true,
		SkipReworkBump:      true, // release column may be "rework"; never double-count
		BumpRecoverAttempts: true,
		SetBlockedUntil:     blockedUntil,
		CloseRunID:          "run-1",
		RunStatus:           "retry_scheduled",
		RunError:            "worker exited nonzero: exit code 1",
		EnqueueSetState:     true,
		Comment:             "auto-retry 1/2 after transient failure: worker exited nonzero",
		Event: store.Event{
			Ts:      now,
			IssueID: sql.NullString{String: "issue-1", Valid: true},
			RunID:   sql.NullString{String: "run-1", Valid: true},
			Kind:    "retry_scheduled",
			Detail:  "auto-retry after transient failure",
		},
	}
	if err := s.Transition(ctx, req); err != nil {
		t.Fatalf("Transition: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Errorf("BoardStatus = %q, want ready", got.BoardStatus)
	}
	if got.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}
	if got.RecoverAttempts != 1 {
		t.Errorf("RecoverAttempts = %d, want 1 (bumped)", got.RecoverAttempts)
	}
	if got.BlockedUntil != blockedUntil {
		t.Errorf("BlockedUntil = %d, want %d", got.BlockedUntil, blockedUntil)
	}
	if got.ReworkCount != 0 {
		t.Errorf("ReworkCount = %d, want 0 (SkipReworkBump: a transient retry must not spend rework budget)", got.ReworkCount)
	}

	// The run closed as retry_scheduled (not blocked), carrying the reason.
	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	latest := snap.Issues[0].LatestRun
	if latest == nil || latest.Status != "retry_scheduled" {
		t.Fatalf("LatestRun = %+v, want status retry_scheduled", latest)
	}

	// The mirror + comment are enqueued in the same transaction.
	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	var sawSetState, sawComment bool
	for _, w := range pending {
		switch w.Kind {
		case "setstate":
			sawSetState = true
			if w.Target != "ready" {
				t.Errorf("setstate Target = %q, want ready", w.Target)
			}
		case "comment":
			sawComment = true
		}
	}
	if !sawSetState || !sawComment {
		t.Errorf("pending writes = %+v, want both a setstate and a comment", pending)
	}
}

// TestTransition_ResetRecoverAttempts_ClearsAttemptsAndBackoff asserts that a
// normal advance carrying ResetRecoverAttempts wipes both recover_attempts and
// blocked_until back to 0 (a clean recovery slate), so a later independent
// transient failure gets a full retry budget rather than inheriting a spent
// one. It also confirms reset takes priority over a NewStatus that would
// otherwise be a plain move.
func TestTransition_ResetRecoverAttempts_ClearsAttemptsAndBackoff(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Seed an issue mid-recovery: two prior transient retries + an active
	// backoff window.
	if err := s.UpsertIssue(ctx, store.Issue{
		ID:              "issue-1",
		Identifier:      "issue-1",
		LaneLabel:       "coder",
		BoardStatus:     "running",
		RecoverAttempts: 2,
		BlockedUntil:    500,
		Deps:            `[]`,
		BranchName:      "issue-1-branch",
		UpdatedAt:       100,
		LastSeen:        100,
		CreatedAt:       100,
	}); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	req := store.TransitionReq{
		IssueID:              "issue-1",
		NewStatus:            "review",
		ResetRecoverAttempts: true,
		Event: store.Event{
			Ts:      600,
			IssueID: sql.NullString{String: "issue-1", Valid: true},
			Kind:    "open_review",
			Detail:  "advanced to review",
		},
	}
	if err := s.Transition(ctx, req); err != nil {
		t.Fatalf("Transition: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "review" {
		t.Errorf("BoardStatus = %q, want review", got.BoardStatus)
	}
	if got.RecoverAttempts != 0 {
		t.Errorf("RecoverAttempts = %d, want 0 (reset on normal advance)", got.RecoverAttempts)
	}
	if got.BlockedUntil != 0 {
		t.Errorf("BlockedUntil = %d, want 0 (cleared on reset)", got.BlockedUntil)
	}
}

// TestUpsertIssue_ConflictPreservesRecoveryState asserts recover_attempts and
// blocked_until survive a Linear re-poll (UpsertIssue's conflict path), like
// board_status/rework_count/claim — a poll must never reset an in-flight
// auto-retry backoff.
func TestUpsertIssue_ConflictPreservesRecoveryState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.UpsertIssue(ctx, store.Issue{
		ID:              "issue-1",
		Identifier:      "issue-1",
		LaneLabel:       "coder",
		BoardStatus:     "ready",
		RecoverAttempts: 1,
		BlockedUntil:    1234,
		Deps:            `[]`,
		UpdatedAt:       100,
		LastSeen:        100,
		CreatedAt:       100,
	}); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	// A fresh poll carries the dispatcher-owned fields zero-valued.
	if err := s.UpsertIssue(ctx, store.Issue{
		ID:          "issue-1",
		Identifier:  "issue-1",
		LaneLabel:   "coder",
		BoardStatus: "ready",
		Deps:        `[]`,
		Priority:    2,
		UpdatedAt:   200,
		LastSeen:    200,
		CreatedAt:   100,
	}); err != nil {
		t.Fatalf("conflict UpsertIssue: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.RecoverAttempts != 1 {
		t.Errorf("RecoverAttempts = %d, want preserved 1 (poll must not reset)", got.RecoverAttempts)
	}
	if got.BlockedUntil != 1234 {
		t.Errorf("BlockedUntil = %d, want preserved 1234 (poll must not reset)", got.BlockedUntil)
	}
	if got.Priority != 2 {
		t.Errorf("Priority = %d, want updated 2 (Linear-sourced intent still updates)", got.Priority)
	}
}

// TestTransition_SkipReworkBump_ReassertingReworkDoesNotDoubleCount asserts
// the fix for the requeueOrphan/rework_count interaction
// (dispatcher.requeueOrphan): a Transition to NewStatus="rework" with
// SkipReworkBump set must NOT increment rework_count. SkipReworkBump exists
// specifically for a claim-release re-assert of a column the issue was
// ALREADY sitting in (store.ReleaseTargetColumn never changes a downstream
// column), never a genuine review/merging->rework edge — so it must not be
// double-counted against amendment C1's rework_cap.
func TestTransition_SkipReworkBump_ReassertingReworkDoesNotDoubleCount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	// A genuine edge into rework bumps the count normally.
	if err := s.Transition(ctx, store.TransitionReq{
		IssueID:   "issue-1",
		NewStatus: "rework",
		Event:     store.Event{Ts: 1, Kind: "transitioned"},
	}); err != nil {
		t.Fatalf("Transition (genuine rework edge): unexpected error: %v", err)
	}
	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.ReworkCount != 1 {
		t.Fatalf("ReworkCount after genuine edge = %d, want 1", got.ReworkCount)
	}

	// A re-assert of the SAME column (SkipReworkBump) must not bump it again.
	if err := s.Transition(ctx, store.TransitionReq{
		IssueID:        "issue-1",
		NewStatus:      "rework",
		SkipReworkBump: true,
		Event:          store.Event{Ts: 2, Kind: "transitioned"},
	}); err != nil {
		t.Fatalf("Transition (SkipReworkBump re-assert): unexpected error: %v", err)
	}
	got, err = s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.ReworkCount != 1 {
		t.Errorf("ReworkCount after SkipReworkBump re-assert = %d, want unchanged 1", got.ReworkCount)
	}
}

// TestTransition_ResetReworkCount_ForcesZeroRegardlessOfNewStatus asserts
// the fix for a stale rework_count surviving a human-driven blocked->ready
// requeue (dispatcher.adoptLinearMove): a Transition with ResetReworkCount
// set resets rework_count to 0 even though NewStatus is neither "rework"
// nor "done".
func TestTransition_ResetReworkCount_ForcesZeroRegardlessOfNewStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	if err := s.Transition(ctx, store.TransitionReq{
		IssueID:   "issue-1",
		NewStatus: "rework",
		Event:     store.Event{Ts: 1, Kind: "transitioned"},
	}); err != nil {
		t.Fatalf("Transition (seed rework_count=1): unexpected error: %v", err)
	}

	if err := s.Transition(ctx, store.TransitionReq{
		IssueID:          "issue-1",
		NewStatus:        "ready",
		ResetReworkCount: true,
		Event:            store.Event{Ts: 2, Kind: "transitioned"},
	}); err != nil {
		t.Fatalf("Transition (ResetReworkCount to ready): unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Errorf("BoardStatus = %q, want ready", got.BoardStatus)
	}
	if got.ReworkCount != 0 {
		t.Errorf("ReworkCount = %d, want reset to 0", got.ReworkCount)
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

	if err := s.MarkLinearWriteDone(ctx, pending[0].ID, 999); err != nil {
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

// TestMarkLinearWriteDone_UsesCallerNow asserts updated_at is set from the
// caller-supplied now argument, not a SQLite-side unixepoch() clock — the
// same convention ClaimReady/Heartbeat/Transition already follow. A done row
// no longer shows up in DrainPendingLinearWrites, so this reads updated_at
// back via s.DB() directly (the established pattern for ad-hoc assertions
// elsewhere in this package, e.g. claim_test.go).
func TestMarkLinearWriteDone_UsesCallerNow(t *testing.T) {
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

	const now int64 = 424242
	if err := s.MarkLinearWriteDone(ctx, pending[0].ID, now); err != nil {
		t.Fatalf("MarkLinearWriteDone: unexpected error: %v", err)
	}

	var gotUpdatedAt int64
	var gotStatus string
	if err := s.DB().QueryRowContext(ctx, `SELECT status, updated_at FROM linear_writes WHERE id = ?`, pending[0].ID).Scan(&gotStatus, &gotUpdatedAt); err != nil {
		t.Fatalf("querying linear_writes row: unexpected error: %v", err)
	}
	if gotStatus != "done" {
		t.Errorf("status = %q, want done", gotStatus)
	}
	if gotUpdatedAt != now {
		t.Errorf("updated_at = %d, want caller-supplied now (%d)", gotUpdatedAt, now)
	}
}

// TestEnqueueLinearSetState_AddsPendingSetstateWrite asserts
// EnqueueLinearSetState inserts a standalone pending 'setstate' linear_writes
// row outside of a Transition — used to mirror a fresh ClaimReady win
// ('running') to Linear, since ClaimReady itself doesn't enqueue any outbox
// row.
func TestEnqueueLinearSetState_AddsPendingSetstateWrite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRunningIssueWithRun(t, s, "issue-1", "run-1")

	if err := s.EnqueueLinearSetState(ctx, "issue-1", "running", 100); err != nil {
		t.Fatalf("EnqueueLinearSetState: unexpected error: %v", err)
	}

	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("len(pending) = %d, want 1", len(pending))
	}
	got := pending[0]
	if got.IssueID != "issue-1" || got.Kind != "setstate" || got.Target != "running" || got.Status != "pending" {
		t.Errorf("pending write = %+v, want IssueID=issue-1 Kind=setstate Target=running Status=pending", got)
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

	const firstNow int64 = 555
	if err := s.MarkLinearWriteFailed(ctx, id, "linear api 500", firstNow); err != nil {
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
	if got.UpdatedAt != firstNow {
		t.Errorf("UpdatedAt = %d, want caller-supplied now (%d)", got.UpdatedAt, firstNow)
	}

	// A second failure bumps attempts again and moves updated_at to the new
	// caller-supplied now.
	const secondNow int64 = 777
	if err := s.MarkLinearWriteFailed(ctx, id, "linear api 500 again", secondNow); err != nil {
		t.Fatalf("MarkLinearWriteFailed (2nd): unexpected error: %v", err)
	}
	after2, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if after2[0].Attempts != 2 {
		t.Errorf("Attempts after 2nd failure = %d, want 2", after2[0].Attempts)
	}
	if after2[0].UpdatedAt != secondNow {
		t.Errorf("UpdatedAt after 2nd failure = %d, want %d", after2[0].UpdatedAt, secondNow)
	}
}
