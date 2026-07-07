package dispatcher_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// TestTick_StaleClaimReleasedAndRequeued asserts that a running issue whose
// claim expired in the past (a lost heartbeat, e.g. from a dispatcher this
// instance never spawned) is released back to ready by reconcile's
// ReleaseStaleClaims step, so it becomes claimable again.
func TestTick_StaleClaimReleasedAndRequeued(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Claim an issue via the store directly (simulating a run from a prior
	// dispatcher process this instance has no inflight record for), with a
	// claim that expires well before the dispatcher's clock.
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	claim, err := s.ClaimReady(ctx, "coder", "run-orphan", 100, 60) // expires at 160
	if err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}
	if claim.Issue.BoardStatus != "running" {
		t.Fatalf("precondition: claimed issue status = %q, want running", claim.Issue.BoardStatus)
	}

	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	// Zero caps so the tick's own selectAndClaim doesn't immediately re-claim
	// the released issue — we want to observe the release itself.
	cfg := zeroCapConfig()
	lc := &linear.MockClient{}
	// Clock is far past the claim's expiry (160), so reconcile releases it.
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(100000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].BoardStatus != string(contract.ColumnReady) {
		t.Errorf("BoardStatus = %q, want ready (stale claim requeued)", snap.Issues[0].BoardStatus)
	}
	if snap.Issues[0].ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared after stale release")
	}
}

// TestTick_OutboxDrainRetriesFailedWrite asserts A2's at-least-once outbox
// guarantee end-to-end: a SetState that fails on the first drain stays
// pending and is retried on the next tick, so Linear eventually records
// exactly one successful SetState for the final state — no lost transition,
// no duplicate.
//
// Caps.PerLane.Reviewer is zeroed so a same-tick reviewer claim on the
// freshly-opened review card can't spawn issue-1 again and reuse this fake
// Spawner's single scripted needs_review result (illegal from "review",
// which would defensively block the issue instead of leaving it at review
// for this test's outbox assertions) — see happy_test.go's
// TestTick_HappyPath_NeedsReviewMovesToReview for the same isolation.
func TestTick_OutboxDrainRetriesFailedWrite(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "PR opened"},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{
		// Fail every SetState on the first drain.
		SetStateErr: errFakeLinearDown,
	}
	cfg := testConfig()
	cfg.Caps.PerLane.Reviewer = 0
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	// Tick 1: claim + spawn + enqueue the running mirror; the drain attempts
	// the running SetState and fails (stays pending). This is the outage.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: unexpected error: %v", err)
	}

	// Recover Linear BEFORE the review transition is applied, so the running
	// mirror retries and succeeds and the review mirror (enqueued in tick 2's
	// reconcile) is attempted exactly once, successfully — letting us assert
	// exactly-once for the final state with no duplicate.
	lc.SetStateErr = nil

	// Tick 2: reconcile applies needs_review -> review (enqueues the review
	// mirror); the drain now succeeds on both the retried running write and
	// the fresh review write.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: unexpected error: %v", err)
	}
	// One more tick to drain anything still pending.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 3: unexpected error: %v", err)
	}

	// Count successful SetState calls per target: after recovery, exactly one
	// running and one review SetState must have succeeded (the failed
	// attempts during the outage were recorded as calls too, so we assert on
	// the final board state's convergence instead of raw call counts).
	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].BoardStatus != string(contract.ColumnReview) {
		t.Errorf("BoardStatus = %q, want review", snap.Issues[0].BoardStatus)
	}

	// No pending linear_writes remain: every enqueued mirror was eventually
	// drained successfully (no lost transition).
	pending, err := s.DrainPendingLinearWrites(context.Background(), 100)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending linear_writes = %d, want 0 (all drained)", len(pending))
	}

	// The final SetState target recorded is 'review', and 'review' was
	// recorded exactly once as a SUCCESS: assert the last successful call is
	// review and that review appears exactly once across all calls (SetState
	// is idempotent, but we must not have enqueued it twice).
	var runningCount, reviewCount int
	for _, c := range lc.SetStateCalls {
		switch c.TargetColumn {
		case "running":
			runningCount++
		case string(contract.ColumnReview):
			reviewCount++
		}
	}
	// running may have been ATTEMPTED multiple times (it failed during the
	// outage and retried), but review was only enqueued once, so it should
	// appear exactly once total.
	if reviewCount != 1 {
		t.Errorf("review SetState calls = %d, want exactly 1 (no duplicate enqueue)", reviewCount)
	}
	if runningCount < 1 {
		t.Errorf("running SetState calls = %d, want at least 1", runningCount)
	}
}

// TestTick_ContinueRespawnFailureDoesNotLeakInflight asserts the fix for the
// Critical inflight-map leak: a "continue" outcome's respawn failure must not
// leave a stale d.inflight entry behind. Before the fix, the next tick's
// Heartbeat loop finds the store claim already cleared (by the respawn
// failure's own park/retry transition) and errors "no active claim",
// aborting reconcile -- and with it promote/selectAndClaim/drainOutbox --
// permanently (the SAME stale entry keeps tripping it on every later tick,
// not just once).
func TestTick_ContinueRespawnFailureDoesNotLeakInflight(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	// Turn 1 (spawn call 1) succeeds and reports "continue". Turn 2's
	// respawn (spawn call 2, triggered by applyContinue) fails at the
	// Spawner level -- exactly the path that used to leak d.inflight.
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeContinue, ThreadId: "thread-continue"},
	}
	spawner.SpawnErr = errors.New("fake exec failure: no such file or directory")
	spawner.FailOnCall = 2

	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig() // RecoverCap defaults to 0 (zero value): parkOrRetry always parks.
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil { // tick 1: claim + spawn turn 1
		t.Fatalf("tick 1: unexpected error: %v", err)
	}

	// Tick 2 drains turn 1's continue result (applyContinue), whose respawn
	// (spawn call 2) fails and parks issue-1 via blockOnSpawnFailure -- all
	// within reconcile's drainResults step. The SAME reconcile() call's
	// heartbeat loop runs immediately after: this is where the leak used to
	// surface, in this very tick, not a later one.
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 2: unexpected error: %v (a leaked inflight record must not wedge the heartbeat loop in the same tick the respawn failed)", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != string(contract.ColumnBlocked) {
		t.Fatalf("BoardStatus = %q, want blocked (the respawn failure parked it, RecoverCap=0)", got.BoardStatus)
	}

	// A second, independent ready issue proves the tick loop is still fully
	// alive on LATER ticks too, not just spared once: pre-fix, the leaked
	// entry keeps failing Heartbeat every tick, aborting reconcile before
	// promote/selectAndClaim/drainOutbox ever run again.
	seedReadyIssue(t, s, "issue-2", "coder", 1, 1000)
	spawner.Results["issue-2"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "PR opened"},
	}

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 3: unexpected error: %v", err)
	}
	if spawner.SpawnCount() != 3 {
		t.Fatalf("SpawnCount = %d, want 3 (turn 1, the failed respawn, and issue-2's fresh claim)", spawner.SpawnCount())
	}

	// And progress keeps happening on tick 4: issue-2's needs_review result
	// gets drained and applied normally.
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 4: unexpected error: %v", err)
	}
	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	var issue2 *store.IssueSnapshot
	for i := range snap.Issues {
		if snap.Issues[i].ID == "issue-2" {
			issue2 = &snap.Issues[i]
		}
	}
	if issue2 == nil {
		t.Fatalf("issue-2 missing from snapshot")
	}
	if issue2.BoardStatus != string(contract.ColumnReview) {
		t.Errorf("issue-2 BoardStatus = %q, want review", issue2.BoardStatus)
	}
}
