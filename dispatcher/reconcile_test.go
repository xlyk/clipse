package dispatcher_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
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
	d := newTestDispatcher(t, testConfig(), s, lc, spawner, ws, fixedClock(1000))

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
