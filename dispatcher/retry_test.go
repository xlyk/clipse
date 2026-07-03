package dispatcher_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// retryConfig returns testConfig with auto-unblock layer 1 enabled: a recover
// cap and a backoff. Most existing dispatcher tests leave RecoverCap at its
// zero value (retry disabled), so the auto-retry tests opt in explicitly.
func retryConfig(recoverCap, backoffS int) config.Config {
	cfg := testConfig()
	cfg.RecoverCap = recoverCap
	cfg.RecoverBackoffS = backoffS
	return cfg
}

// tickUntilCond re-ticks the dispatcher (at whatever the clock currently
// reports) until cond holds or a deadline passes. It absorbs the asynchronous
// delivery of a spawned worker's result on d.results (the Wait-goroutine may
// not have posted by the time a single follow-up tick drains), the same way
// tickUntilBlocked does — but on an arbitrary condition so the auto-retry tests
// can wait for a re-queue, a re-claim, or a park.
func tickUntilCond(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("Tick while waiting for %s: unexpected error: %v", what, err)
		}
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition %q not met within deadline", what)
}

func getIssue(t *testing.T, s *store.Store, id string) *store.Issue {
	t.Helper()
	issue, err := s.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatalf("GetIssue(%s): unexpected error: %v", id, err)
	}
	return issue
}

// TestTick_TransientBlock_RetryLifecycle drives the full auto-unblock layer 1
// arc for a coder issue whose worker always reports blocked/transient with
// RecoverCap=2: each failure re-queues the card to ready with a bumped
// recover_attempts and a future blocked_until (NOT parked), the card is NOT
// re-claimed until the clock passes that backoff deadline, and the
// (RecoverCap+1)-th failure — once the budget is spent — parks it in blocked.
func TestTick_TransientBlock_RetryLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	bk := contract.BlockKindTransient
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{
			Outcome:   contract.WorkerResultOutcomeBlocked,
			BlockKind: &bk,
			Summary:   "npm registry timed out",
		},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}

	const backoff = 30
	clockVal := int64(1000)
	clock := func() int64 { return clockVal }
	d := dispatcher.New(retryConfig(2, backoff), s, lc, spawner, ws,
		dispatcher.WithClock(clock), dispatcher.WithRunIDGenerator(sequentialRunIDs()))

	// First failure -> re-queue #1 (not parked).
	tickUntilCond(t, ctx, d, "first auto-retry", func() bool {
		return getIssue(t, s, "issue-1").RecoverAttempts == 1
	})
	issue := getIssue(t, s, "issue-1")
	if issue.BoardStatus != string(contract.ColumnReady) {
		t.Fatalf("after retry #1: BoardStatus = %q, want ready (re-queued, not parked)", issue.BoardStatus)
	}
	if issue.BlockedUntil != clockVal+backoff {
		t.Errorf("after retry #1: BlockedUntil = %d, want %d (now+backoff)", issue.BlockedUntil, clockVal+backoff)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("after retry #1: ClaimLock still valid, want cleared")
	}
	spawnsAfterRetry1 := spawner.SpawnCount()
	if spawnsAfterRetry1 != 1 {
		t.Fatalf("after retry #1: SpawnCount = %d, want 1 (initial attempt only)", spawnsAfterRetry1)
	}

	// Within the backoff window the card is NOT re-claimable: several ticks at
	// the same clock must not spawn again or bump the counter.
	for i := 0; i < 3; i++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("backoff-window tick %d: unexpected error: %v", i, err)
		}
	}
	if spawner.SpawnCount() != spawnsAfterRetry1 {
		t.Errorf("SpawnCount grew to %d during backoff window, want no re-claim (%d)", spawner.SpawnCount(), spawnsAfterRetry1)
	}
	if got := getIssue(t, s, "issue-1"); got.RecoverAttempts != 1 || got.BoardStatus != string(contract.ColumnReady) {
		t.Errorf("during backoff: recover_attempts=%d status=%q, want 1/ready (still waiting out backoff)", got.RecoverAttempts, got.BoardStatus)
	}

	// Advance past the backoff deadline: the card becomes claimable, re-runs,
	// fails again -> re-queue #2 (recover_attempts=2, still not parked).
	clockVal = 1000 + backoff
	tickUntilCond(t, ctx, d, "second auto-retry", func() bool {
		return getIssue(t, s, "issue-1").RecoverAttempts == 2
	})
	issue = getIssue(t, s, "issue-1")
	if issue.BoardStatus != string(contract.ColumnReady) {
		t.Fatalf("after retry #2: BoardStatus = %q, want ready", issue.BoardStatus)
	}
	if issue.BlockedUntil != clockVal+backoff {
		t.Errorf("after retry #2: BlockedUntil = %d, want %d", issue.BlockedUntil, clockVal+backoff)
	}

	// Advance past the second backoff: the card re-runs and fails a third time,
	// but the budget (RecoverCap=2) is now spent, so it PARKS instead.
	clockVal += backoff
	tickUntilCond(t, ctx, d, "park after cap", func() bool {
		return getIssue(t, s, "issue-1").BoardStatus == string(contract.ColumnBlocked)
	})
	issue = getIssue(t, s, "issue-1")
	if issue.RecoverAttempts != 2 {
		t.Errorf("after park: RecoverAttempts = %d, want 2 (bumped exactly RecoverCap times)", issue.RecoverAttempts)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("after park: ClaimLock still valid, want cleared")
	}
	if spawner.SpawnCount() != 3 {
		t.Errorf("total SpawnCount = %d, want 3 (initial + 2 retries; the cap-exceeding failure parks)", spawner.SpawnCount())
	}
	// The park posted a block comment; the last retry closed its run as
	// retry_scheduled before the park.
	if len(lc.CommentCalls) == 0 {
		t.Errorf("no Comment call recorded across the lifecycle, want retry + block comments")
	}

	// A further tick must not re-claim a parked issue.
	parkedSpawns := spawner.SpawnCount()
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("post-park tick: unexpected error: %v", err)
	}
	if spawner.SpawnCount() != parkedSpawns {
		t.Errorf("SpawnCount grew to %d after park, want no re-claim (%d)", spawner.SpawnCount(), parkedSpawns)
	}
}

// TestTick_NonTransientBlock_ParksImmediately asserts a capability or
// needs_input block parks the issue on the first failure even when a full
// recover budget is available: only transient blocks auto-retry. recover_attempts
// stays untouched (0).
func TestTick_NonTransientBlock_ParksImmediately(t *testing.T) {
	cases := []struct {
		name string
		kind contract.BlockKind
	}{
		{"capability", contract.BlockKindCapability},
		{"needs_input", contract.BlockKindNeedsInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

			spawner := newFakeSpawner()
			bk := tc.kind
			spawner.Results["issue-1"] = spawn.Result{
				Worker: contract.WorkerResult{
					Outcome:   contract.WorkerResultOutcomeBlocked,
					BlockKind: &bk,
					Summary:   "not a transient failure",
				},
			}
			ws := newStubWorkspacer(t.TempDir())
			lc := &linear.MockClient{}
			// A generous recover budget that must go UNUSED for a non-transient block.
			d := newTestDispatcher(t, retryConfig(3, 30), s, lc, spawner, ws, fixedClock(1000))

			tickUntilCond(t, ctx, d, "park", func() bool {
				return getIssue(t, s, "issue-1").BoardStatus == string(contract.ColumnBlocked)
			})
			issue := getIssue(t, s, "issue-1")
			if issue.RecoverAttempts != 0 {
				t.Errorf("RecoverAttempts = %d, want 0 (non-transient block must not consume the retry budget)", issue.RecoverAttempts)
			}
			if issue.BlockedUntil != 0 {
				t.Errorf("BlockedUntil = %d, want 0 (no backoff on a park)", issue.BlockedUntil)
			}

			spawnsAfterBlock := spawner.SpawnCount()
			if err := d.Tick(ctx); err != nil {
				t.Fatalf("post-park tick: unexpected error: %v", err)
			}
			if spawner.SpawnCount() != spawnsAfterBlock {
				t.Errorf("SpawnCount grew to %d after park, want no re-claim (%d)", spawner.SpawnCount(), spawnsAfterBlock)
			}
		})
	}
}

// TestTick_RunLevelFailure_AutoRetries asserts each run-level failure mode
// (nonzero exit, malformed result, context-deadline timeout) is treated as
// transient and auto-retried (re-queued to ready with recover_attempts=1),
// rather than parked, when a recover budget is available.
func TestTick_RunLevelFailure_AutoRetries(t *testing.T) {
	cases := []struct {
		name string
		res  spawn.Result
	}{
		{"worker_exit", spawn.Result{Err: fmt.Errorf("%w: exit code 1", spawn.ErrWorkerExit)}},
		{"malformed_result", spawn.Result{Err: fmt.Errorf("%w: unexpected EOF", spawn.ErrMalformedResult)}},
		{"timeout", spawn.Result{Err: context.DeadlineExceeded}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

			spawner := newFakeSpawner()
			spawner.Results["issue-1"] = tc.res
			ws := newStubWorkspacer(t.TempDir())
			lc := &linear.MockClient{}
			cfg := retryConfig(2, 30)
			cfg.MaxRuntimeS = 1 // for the timeout case; harmless for the others
			d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

			tickUntilCond(t, ctx, d, "auto-retry", func() bool {
				return getIssue(t, s, "issue-1").RecoverAttempts == 1
			})
			issue := getIssue(t, s, "issue-1")
			if issue.BoardStatus != string(contract.ColumnReady) {
				t.Errorf("BoardStatus = %q, want ready (run-level failure auto-retried, not parked)", issue.BoardStatus)
			}
			if issue.BlockedUntil != 1000+30 {
				t.Errorf("BlockedUntil = %d, want 1030 (now+backoff)", issue.BlockedUntil)
			}
			if issue.ClaimLock.Valid {
				t.Errorf("ClaimLock still valid, want cleared on re-queue")
			}
		})
	}
}

// TestTick_TransientRetryThenSuccess_ResetsRecoverAttempts asserts recover_attempts
// resets to 0 once the card advances normally: a transient failure spends one
// unit of budget, but a subsequent successful turn (needs_review) wipes the
// counter so a later, independent transient failure gets a full budget again.
func TestTick_TransientRetryThenSuccess_ResetsRecoverAttempts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	bk := contract.BlockKindTransient
	spawner := newFakeSpawner()
	spawner.ResultsQueue["issue-1"] = []spawn.Result{
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeBlocked, BlockKind: &bk, Summary: "flaky network"}},
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "PR opened on retry"}},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	// Reviewer cap 0 so the freshly-opened review card isn't claimed and run
	// through the single scripted queue (which would drain the wrong result).
	cfg := retryConfig(2, 30)
	cfg.Caps.PerLane.Reviewer = 0

	const backoff = 30
	clockVal := int64(1000)
	clock := func() int64 { return clockVal }
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(clock), dispatcher.WithRunIDGenerator(sequentialRunIDs()))

	// Transient failure -> re-queue #1.
	tickUntilCond(t, ctx, d, "auto-retry", func() bool {
		return getIssue(t, s, "issue-1").RecoverAttempts == 1
	})

	// Past the backoff, the retry re-runs and succeeds (needs_review) -> review.
	clockVal = 1000 + backoff
	tickUntilCond(t, ctx, d, "advance to review", func() bool {
		return getIssue(t, s, "issue-1").BoardStatus == string(contract.ColumnReview)
	})
	issue := getIssue(t, s, "issue-1")
	if issue.RecoverAttempts != 0 {
		t.Errorf("RecoverAttempts = %d, want 0 (reset on normal advance)", issue.RecoverAttempts)
	}
	if issue.BlockedUntil != 0 {
		t.Errorf("BlockedUntil = %d, want 0 (cleared on reset)", issue.BlockedUntil)
	}
}

// TestTick_TransientBlock_RecoverCapZeroParksImmediately asserts the kill
// switch: with RecoverCap=0, even a transient block parks immediately (the
// pre-layer-1 behavior), with recover_attempts untouched.
func TestTick_TransientBlock_RecoverCapZeroParksImmediately(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	bk := contract.BlockKindTransient
	spawner := newFakeSpawner()
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeBlocked, BlockKind: &bk, Summary: "transient, but retry disabled"},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, retryConfig(0, 30), s, lc, spawner, ws, fixedClock(1000))

	tickUntilCond(t, ctx, d, "park", func() bool {
		return getIssue(t, s, "issue-1").BoardStatus == string(contract.ColumnBlocked)
	})
	if got := getIssue(t, s, "issue-1"); got.RecoverAttempts != 0 {
		t.Errorf("RecoverAttempts = %d, want 0 (RecoverCap=0 disables auto-retry)", got.RecoverAttempts)
	}
}

// TestTick_IllegalTransition_ParksWithoutRetry asserts a defensive
// illegal-transition park (a reviewer emitting needs_review from the review
// column, which board.Next rejects) parks the issue and does NOT auto-retry,
// even with a recover budget available: an illegal transition is a kernel-level
// inconsistency, not a transient failure.
func TestTick_IllegalTransition_ParksWithoutRetry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedColumnIssue(t, s, "issue-1", "review", 1, 100)

	spawner := newFakeSpawner()
	// needs_review is illegal from the review column (board.Next has no such edge).
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "bogus from a reviewer"},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, retryConfig(3, 30), s, lc, spawner, ws, fixedClock(1000))

	tickUntilCond(t, ctx, d, "park", func() bool {
		return getIssue(t, s, "issue-1").BoardStatus == string(contract.ColumnBlocked)
	})
	issue := getIssue(t, s, "issue-1")
	if issue.RecoverAttempts != 0 {
		t.Errorf("RecoverAttempts = %d, want 0 (an illegal transition must not auto-retry)", issue.RecoverAttempts)
	}

	spawnsAfterBlock := spawner.SpawnCount()
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("post-park tick: unexpected error: %v", err)
	}
	if spawner.SpawnCount() != spawnsAfterBlock {
		t.Errorf("SpawnCount grew to %d after illegal-transition park, want no re-claim (%d)", spawner.SpawnCount(), spawnsAfterBlock)
	}
}

// TestTick_ReworkCapBlock_ParksWithoutRetry asserts the rework-cap park
// (blockIfReworkCapExceeded) is NOT routed through auto-retry even with a
// recover budget: a convergence failure is bounded by rework_cap, not
// recover_cap, so it parks and leaves recover_attempts untouched.
func TestTick_ReworkCapBlock_ParksWithoutRetry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedColumnIssue(t, s, "issue-1", "review", 1, 100)

	prURL := "https://github.com/x/y/pull/1"
	spawner := newFakeSpawner()
	spawner.ResultsQueue["issue-1"] = []spawn.Result{
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeChangesRequested, Summary: "review 1", PrUrl: &prURL}},
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "coder 1"}},
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeChangesRequested, Summary: "review 2 (cap-exceeding)", PrUrl: &prURL}},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := retryConfig(2, 30)
	cfg.ReworkCap = 1
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	tickUntilCond(t, ctx, d, "rework-cap park", func() bool {
		return getIssue(t, s, "issue-1").BoardStatus == string(contract.ColumnBlocked)
	})
	issue := getIssue(t, s, "issue-1")
	if issue.RecoverAttempts != 0 {
		t.Errorf("RecoverAttempts = %d, want 0 (rework-cap park must not touch the recover budget)", issue.RecoverAttempts)
	}
	if issue.ReworkCount != cfg.ReworkCap {
		t.Errorf("ReworkCount = %d, want %d (capped)", issue.ReworkCount, cfg.ReworkCap)
	}
}
