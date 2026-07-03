package dispatcher_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// zeroCapConfig returns a Config identical to testConfig but with every cap
// zeroed, so a Tick's selectAndClaim phase never claims anything — letting
// poll-focused tests observe pollAndUpsert's effect in isolation from the
// same-tick claim it would otherwise trigger.
func zeroCapConfig() config.Config {
	cfg := testConfig()
	cfg.Caps = config.Caps{}
	return cfg
}

// TestTick_PollCachesCandidatesFromLinear asserts pollAndUpsert caches every
// candidate Linear returns, mapping Lane->lane_label and Status->board_status
// on the initial insert. issue-2 has an unresolved dependency so promote
// leaves it in todo, isolating pollAndUpsert's own mapping from promotion's
// separate effect.
func TestTick_PollCachesCandidatesFromLinear(t *testing.T) {
	s := openTestStore(t)
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Title: "Add the thing", Description: "Implement the thing.", Status: "ready", Lane: "coder", Priority: 1, BranchName: "clp-1", UpdatedAt: 100},
			{ID: "issue-2", Identifier: "CLP-2", Status: "todo", Lane: "", Deps: []string{"issue-1"}, Priority: 0, BranchName: "clp-2", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	byID := map[string]string{}
	byLane := map[string]string{}
	for _, is := range snap.Issues {
		byID[is.ID] = is.BoardStatus
		byLane[is.ID] = is.LaneLabel
	}
	if byID["issue-1"] != "ready" {
		t.Errorf("issue-1 board_status = %q, want ready", byID["issue-1"])
	}
	if byLane["issue-1"] != "coder" {
		t.Errorf("issue-1 lane_label = %q, want coder", byLane["issue-1"])
	}
	// issue-2 has no lane, so it's cached but inert for dispatch; its
	// dependency (issue-1) isn't terminal, so promote leaves it in todo.
	if byID["issue-2"] != "todo" {
		t.Errorf("issue-2 board_status = %q, want todo", byID["issue-2"])
	}
	if byLane["issue-2"] != "" {
		t.Errorf("issue-2 lane_label = %q, want empty", byLane["issue-2"])
	}

	// title/description must flow from Linear into the store (Phase-2
	// issue-text plumbing): this is what lets a later claim carry them into
	// the worker's CLIPSE_ISSUE_TEXT. ReadSnapshot's Issue projection
	// doesn't select these columns, so read back via GetIssue instead.
	got1, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue(issue-1): unexpected error: %v", err)
	}
	if got1.Title != "Add the thing" {
		t.Errorf("issue-1 Title = %q, want %q", got1.Title, "Add the thing")
	}
	if got1.Description != "Implement the thing." {
		t.Errorf("issue-1 Description = %q, want %q", got1.Description, "Implement the thing.")
	}
}

// TestTick_RepollPreservesRunningBoardStatus asserts that once an issue is
// claimed and running, a later poll returning its old Linear status (e.g.
// "ready", since Linear hasn't been mirrored to "running" from Linear's own
// perspective at poll time) does not reset board_status away from running.
// UpsertIssue's own conflict semantics guarantee this; this test exercises
// it through a full Tick pass.
func TestTick_RepollPreservesRunningBoardStatus(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "clp-1-branch", UpdatedAt: 100},
		},
	}
	spawner := newFakeSpawner()
	// Every re-spawn of issue-1 (Linear identifier CLP-1) reports "continue"
	// with a distant turn cap, so the run stays inflight/running across both
	// ticks rather than resolving to a terminal transition mid-test.
	spawner.Results["CLP-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeContinue, ThreadId: "thread-1"},
	}
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.TurnCap = 1000
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	// First tick: claims and spawns issue-1 (moves it to running).
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].BoardStatus != "running" {
		t.Fatalf("after first tick, board_status = %q, want running", snap.Issues[0].BoardStatus)
	}

	// Second tick re-polls the same stale "ready" status from Linear.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: unexpected error: %v", err)
	}

	snap2, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap2.Issues[0].BoardStatus != "running" {
		t.Errorf("after second tick (repoll), board_status = %q, want preserved running", snap2.Issues[0].BoardStatus)
	}
}

// TestTick_PollAdoptsHumanMoveWhenUnclaimed asserts A3's adoption rule: when
// an existing issue's SQLite board_status diverges from what Linear now
// reports, and the issue holds no active claim, the poll adopts the human
// move — SQLite is updated to match Linear (no run to close, since nothing
// was in flight) — and the issue becomes claimable in that new state on the
// very same tick (zero caps here isolate the adoption from any claim, but a
// direct read confirms the adopted status is 'ready').
func TestTick_PollAdoptsHumanMoveWhenUnclaimed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Seed issue-1 as 'blocked' with no claim, as if a prior run blocked it.
	issue := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		LaneLabel:   "coder",
		BoardStatus: "blocked",
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "issue-1-branch",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	// A human moved the issue back to Ready in Linear.
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	// Zero caps so this tick's own selectAndClaim doesn't immediately claim
	// the newly-adopted ready issue — we want to observe the adoption alone.
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Errorf("BoardStatus = %q, want ready (adopted human move)", got.BoardStatus)
	}

	// Adoption does not mirror back to Linear (Linear already holds this
	// state) and does not close/open any run.
	pending, err := s.DrainPendingLinearWrites(ctx, 100)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending linear writes = %d, want 0 (adoption does not mirror back)", len(pending))
	}

	// The adoption is claimable on a later tick: re-tick with real caps and
	// confirm it gets claimed.
	cfg := testConfig()
	d2 := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))
	if err := d2.Tick(ctx); err != nil {
		t.Fatalf("second Tick: unexpected error: %v", err)
	}
	got2, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue after second tick: unexpected error: %v", err)
	}
	if got2.BoardStatus != "running" {
		t.Errorf("BoardStatus after second tick = %q, want running (adopted issue was claimable)", got2.BoardStatus)
	}
}

// TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsReworkCount asserts the
// fix for a stale rework_count surviving a human requeue: adopting a
// blocked->ready move (A3, unclaimed) resets issues.rework_count to zero.
// Without this, an issue blocked after tripping amendment C1's rework_cap
// keeps whatever rework_count it accumulated on its PRIOR review/rework
// cycle, so a human's very next requeue could immediately re-trip
// blockIfReworkCapExceeded on the first subsequent changes_requested —
// defeating the point of requeuing it by hand.
func TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsReworkCount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Seed issue-1 as 'blocked' with a stale rework_count left over from a
	// prior rework-cap trip, and no active claim.
	issue := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		LaneLabel:   "coder",
		BoardStatus: "blocked",
		ReworkCount: 3,
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "issue-1-branch",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	// A human moved the issue back to Ready in Linear.
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Fatalf("BoardStatus = %q, want ready (adopted human move)", got.BoardStatus)
	}
	if got.ReworkCount != 0 {
		t.Errorf("ReworkCount = %d, want reset to 0 on human requeue from blocked", got.ReworkCount)
	}
}

// TestTick_PollAdoptsHumanMove_FromNonBlocked_DoesNotResetReworkCount
// asserts the reset above is scoped to a blocked->{ready,todo} requeue
// specifically: an ordinary human-adopted move that doesn't originate from
// Blocked must leave rework_count untouched.
func TestTick_PollAdoptsHumanMove_FromNonBlocked_DoesNotResetReworkCount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	issue := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		LaneLabel:   "coder",
		BoardStatus: "todo",
		ReworkCount: 2,
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "issue-1-branch",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Fatalf("BoardStatus = %q, want ready (adopted human move)", got.BoardStatus)
	}
	if got.ReworkCount != 2 {
		t.Errorf("ReworkCount = %d, want unchanged 2 (adoption did not originate from blocked)", got.ReworkCount)
	}
}

// TestTick_PollReassertsDispatcherOwnedStateWhenClaimed asserts A3's other
// half: when an issue's SQLite board_status diverges from Linear's polled
// status BUT the issue holds an active claim (the dispatcher owns it right
// now), the dispatcher does not adopt Linear's stale view — it re-asserts
// its own truth by enqueueing a setstate mirror back to the SQLite status,
// and board_status itself is left untouched.
func TestTick_PollReassertsDispatcherOwnedStateWhenClaimed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	claim, err := s.ClaimReady(ctx, "coder", "run-1", 100, 3600)
	if err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}
	if claim.Issue.BoardStatus != "running" {
		t.Fatalf("precondition: claimed issue status = %q, want running", claim.Issue.BoardStatus)
	}

	// Linear still reports the pre-claim "ready" status (its own mirror
	// write for the claim hasn't landed/been observed yet from Linear's
	// perspective at poll time).
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	// The inflight run must not resolve mid-test: script "continue" so
	// nothing else changes board_status out from under this assertion.
	spawner.Results["CLP-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeContinue, ThreadId: "thread-1"},
	}
	ws := newStubWorkspacer(t.TempDir())
	cfg := zeroCapConfig()
	cfg.TurnCap = 1000
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "running" {
		t.Errorf("BoardStatus = %q, want running (dispatcher-owned state preserved, not reset to Linear's stale ready)", got.BoardStatus)
	}

	// The reassert enqueue is drained within the same Tick (drainOutbox is
	// the last phase), so assert on the MockClient's recorded SetState calls
	// rather than on still-pending rows.
	var sawRunningReassert bool
	for _, c := range lc.SetStateCalls {
		if c.IssueID == "issue-1" && c.TargetColumn == "running" {
			sawRunningReassert = true
		}
	}
	if !sawRunningReassert {
		t.Errorf("SetStateCalls = %+v, want a setstate -> running reassertion", lc.SetStateCalls)
	}
}
