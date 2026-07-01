package dispatcher_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
)

// TestTick_BlockedOutcome_ParksAndDoesNotReclaim asserts a blocked worker
// outcome parks the issue in blocked with a reason comment, clears the
// claim, and is NOT re-claimed on the next tick (no failure auto-retry).
func TestTick_BlockedOutcome_ParksAndDoesNotReclaim(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	bk := contract.BlockKindNeedsInput
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{
			Outcome:   contract.WorkerResultOutcomeBlocked,
			BlockKind: &bk,
			Summary:   "needs a decision from a human",
		},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, testConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: unexpected error: %v", err)
	}
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2 (applies block): unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].BoardStatus != string(contract.ColumnBlocked) {
		t.Fatalf("BoardStatus = %q, want blocked", snap.Issues[0].BoardStatus)
	}
	if snap.Issues[0].ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}

	// A block posts a reason comment derived from block_kind + summary.
	if len(lc.CommentCalls) != 1 {
		t.Fatalf("Comment calls = %d, want 1", len(lc.CommentCalls))
	}
	body := lc.CommentCalls[0].Body
	if body == "" {
		t.Errorf("comment body empty, want a block reason")
	}

	spawnsAfterBlock := spawner.SpawnCount()

	// A third tick must NOT re-claim the blocked issue (no auto-retry).
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 3: unexpected error: %v", err)
	}
	if spawner.SpawnCount() != spawnsAfterBlock {
		t.Errorf("SpawnCount grew to %d after a blocked issue's later tick, want no re-claim (%d)", spawner.SpawnCount(), spawnsAfterBlock)
	}
	snap2, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap2.Issues[0].BoardStatus != string(contract.ColumnBlocked) {
		t.Errorf("BoardStatus after tick 3 = %q, want still blocked", snap2.Issues[0].BoardStatus)
	}
}

// TestTick_ContinueRespawnsUntilTurnCapThenBlocks asserts that repeated
// 'continue' outcomes re-spawn the same run (reusing worktree + thread id)
// up to exactly cfg.TurnCap turns, then park the issue in blocked with a
// "turn cap reached" reason.
func TestTick_ContinueRespawnsUntilTurnCapThenBlocks(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	// Every spawn of CLP-1 (the polled identifier) reports continue.
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{
			Outcome:  contract.WorkerResultOutcomeContinue,
			ThreadId: "thread-continue",
			Summary:  "made progress, more to do",
		},
	}
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.TurnCap = 3
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	// Tick 1 claims + spawns turn 1. Each subsequent tick drains the prior
	// continue result and re-spawns the next turn, until turn == TurnCap,
	// after which the issue blocks. Run enough ticks to cover the whole arc.
	for i := 0; i < cfg.TurnCap+3; i++ {
		if err := d.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: unexpected error: %v", i, err)
		}
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	issue := snap.Issues[0]
	if issue.BoardStatus != string(contract.ColumnBlocked) {
		t.Errorf("BoardStatus = %q, want blocked at turn cap", issue.BoardStatus)
	}
	// Exactly TurnCap spawns happened: turns 1..TurnCap. The TurnCap-th
	// continue result does NOT re-spawn (turn >= cap -> block).
	if spawner.SpawnCount() != cfg.TurnCap {
		t.Errorf("SpawnCount = %d, want exactly TurnCap (%d)", spawner.SpawnCount(), cfg.TurnCap)
	}
	// The final run's turn_count reached TurnCap.
	if issue.LatestRun == nil {
		t.Fatalf("LatestRun = nil")
	}
	if issue.LatestRun.TurnCount != cfg.TurnCap {
		t.Errorf("LatestRun.TurnCount = %d, want TurnCap (%d)", issue.LatestRun.TurnCount, cfg.TurnCap)
	}
	// Every re-spawn after the first reused the worker's returned thread id.
	specs := spawner.Specs()
	for i, sp := range specs {
		if i == 0 {
			if sp.ThreadID != "" {
				t.Errorf("spawn 0 ThreadID = %q, want empty (fresh claim)", sp.ThreadID)
			}
			continue
		}
		if sp.ThreadID != "thread-continue" {
			t.Errorf("spawn %d ThreadID = %q, want thread-continue (continuation resume)", i, sp.ThreadID)
		}
		if sp.RunID != specs[0].RunID {
			t.Errorf("spawn %d RunID = %q, want same run id as spawn 0 (%q)", i, sp.RunID, specs[0].RunID)
		}
	}
}

// TestTick_SpawnFailureBlocksWithoutRetry asserts that when the
// Spawner/Workspacer machinery itself fails to produce a process to Wait on
// (fakeSpawner.SpawnErr), the issue is blocked directly via
// blockOnSpawnFailure rather than through the normal applyResult/runResult
// path: no run is ever inflight, so there is nothing for reconcile to drain
// on a later tick. This exercises the ws.Ensure/spawner.Spawn error branch in
// dispatcher/spawn.go's spawnAttempt, which TestTick_FailureResultsBlockWithoutRetry
// does not cover (that test only exercises worker-process failures reported
// through a spawned handle's Wait()).
func TestTick_SpawnFailureBlocksWithoutRetry(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	spawner.SpawnErr = errors.New("fake exec failure: no such file or directory")
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, testConfig(), s, lc, spawner, ws, fixedClock(1000))

	// A single tick both claims the issue and calls spawnAttempt, which fails
	// immediately (SpawnErr, not a Wait()-reported result) and blocks the
	// issue synchronously within the same Tick — unlike a worker-process
	// failure, there is no inflight run/goroutine to drain on a later tick.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].BoardStatus != string(contract.ColumnBlocked) {
		t.Fatalf("BoardStatus = %q, want blocked", snap.Issues[0].BoardStatus)
	}
	if snap.Issues[0].ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}
	if snap.Issues[0].LatestRun == nil || !snap.Issues[0].LatestRun.Error.Valid {
		t.Errorf("run error not recorded, want the spawn failure reason stored")
	}
	if snap.Issues[0].LatestRun != nil && snap.Issues[0].LatestRun.Status != "blocked" {
		t.Errorf("LatestRun.Status = %q, want blocked", snap.Issues[0].LatestRun.Status)
	}

	// blockOnSpawnFailure enqueues a blocked-reason comment (Comment field on
	// the TransitionReq), same as the worker-failure path.
	if len(lc.CommentCalls) == 0 {
		t.Fatalf("no Comment call recorded, want a block-reason comment")
	}
	if lc.CommentCalls[0].Body == "" {
		t.Errorf("comment body empty, want a block reason")
	}

	// blockOnSpawnFailure never spawns a process to Wait on, so there is
	// nothing inflight; a bare spawn count of 1 (the failed attempt itself)
	// confirms spawnAttempt actually reached d.spawner.Spawn.
	if spawner.SpawnCount() != 1 {
		t.Fatalf("SpawnCount = %d, want 1 (the failed spawn attempt)", spawner.SpawnCount())
	}

	// A second tick must NOT re-claim the blocked issue (no auto-retry).
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: unexpected error: %v", err)
	}
	if spawner.SpawnCount() != 1 {
		t.Errorf("SpawnCount grew to %d after a blocked issue's later tick, want no re-claim (1)", spawner.SpawnCount())
	}
	snap2, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap2.Issues[0].BoardStatus != string(contract.ColumnBlocked) {
		t.Errorf("BoardStatus after tick 2 = %q, want still blocked", snap2.Issues[0].BoardStatus)
	}
}

// TestTick_FailureResultsBlockWithoutRetry asserts each Spawner-reported
// failure mode (nonzero exit, malformed result, context-deadline timeout)
// lands the issue in blocked with a comment and no auto-retry.
func TestTick_FailureResultsBlockWithoutRetry(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"worker_exit", fmt.Errorf("%w: exit code 1", spawn.ErrWorkerExit)},
		{"malformed_result", fmt.Errorf("%w: unexpected EOF", spawn.ErrMalformedResult)},
		{"timeout", fmt.Errorf("worker killed after context deadline: %w", context.DeadlineExceeded)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

			spawner := newFakeSpawner()
			if errors.Is(tc.err, context.DeadlineExceeded) {
				// The timeout path is driven by fakeHandle blocking on
				// ctx.Done(); use a tiny MaxRuntimeS so the spawn context
				// deadline fires quickly.
				spawner.Results["issue-1"] = spawn.Result{Err: context.DeadlineExceeded}
			} else {
				spawner.Results["issue-1"] = spawn.Result{Err: tc.err}
			}
			ws := newStubWorkspacer(t.TempDir())
			cfg := testConfig()
			cfg.MaxRuntimeS = 1 // for the timeout case; harmless for the others
			lc := &linear.MockClient{}
			d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

			if err := d.Tick(context.Background()); err != nil {
				t.Fatalf("tick 1: unexpected error: %v", err)
			}
			// Re-tick until the issue leaves 'running' (its result has been
			// drained + applied). The timeout case only posts its result
			// after the 1s spawn-context deadline fires, so this loop both
			// waits for that and drains it. A generous deadline keeps the
			// test robust without a fixed sleep.
			tickUntilBlocked(t, context.Background(), d, s, "issue-1")

			snap, err := s.ReadSnapshot(context.Background())
			if err != nil {
				t.Fatalf("ReadSnapshot: unexpected error: %v", err)
			}
			if snap.Issues[0].BoardStatus != string(contract.ColumnBlocked) {
				t.Errorf("BoardStatus = %q, want blocked", snap.Issues[0].BoardStatus)
			}
			if len(lc.CommentCalls) == 0 {
				t.Errorf("no Comment call recorded, want a block-reason comment")
			}
			if snap.Issues[0].LatestRun == nil || !snap.Issues[0].LatestRun.Error.Valid {
				t.Errorf("run error not recorded, want the failure reason stored")
			}

			spawnsAfterBlock := spawner.SpawnCount()
			if err := d.Tick(context.Background()); err != nil {
				t.Fatalf("tick 3: unexpected error: %v", err)
			}
			if spawner.SpawnCount() != spawnsAfterBlock {
				t.Errorf("re-spawn after failure block: SpawnCount = %d, want %d (no auto-retry)", spawner.SpawnCount(), spawnsAfterBlock)
			}
		})
	}
}
