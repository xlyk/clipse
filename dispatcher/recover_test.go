package dispatcher_test

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// spawnRealOrphan spawns a real hanging testworker subprocess (a
// group-leader, exactly as the production LocalSpawner spawns workers). It
// returns the live handle so the caller can read its pid/proc_started_at and
// later reap the zombie left behind once the process has been killed.
func spawnRealOrphan(t *testing.T, boardDir, issueID, lane string) spawn.RunHandle {
	t.Helper()
	ctx := context.Background()

	bin := buildTestworker(t)
	spawner := spawn.NewLocalSpawner([]string{bin}, boardDir)
	spec := spawn.WorkerSpec{
		Issue:     issueID,
		Lane:      lane,
		RunID:     "orphan-run",
		Workspace: t.TempDir(),
		Env:       append(os.Environ(), "TESTWORKER_SCENARIO=hang"),
	}

	spawnCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	t.Cleanup(cancel)

	h, err := spawner.Spawn(spawnCtx, spec)
	if err != nil {
		t.Fatalf("spawning real orphan worker: unexpected error: %v", err)
	}
	// Give the process group leader a moment to fully establish before any
	// caller reads its identity or tries to signal it.
	time.Sleep(100 * time.Millisecond)

	return h
}

// seedOrphanRun seeds issueID as 'running' with an active claim held by
// runID, and a runs row at status='running' carrying pid/procStartedAt and
// attempt, mirroring exactly what a real dispatcher leaves behind mid-run.
func seedOrphanRun(t *testing.T, s *store.Store, issueID, lane, runID string, pid int, procStartedAt int64, attempt int, now int64) {
	t.Helper()
	ctx := context.Background()
	seedReadyIssue(t, s, issueID, lane, 1, now)

	claim, err := s.ClaimReady(ctx, lane, runID, now, 3600)
	if err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}
	if claim.Issue.ID != issueID {
		t.Fatalf("ClaimReady claimed %q, want %q", claim.Issue.ID, issueID)
	}

	if err := s.SetRunProcess(ctx, runID, pid, procStartedAt); err != nil {
		t.Fatalf("SetRunProcess: unexpected error: %v", err)
	}

	// ClaimReady always inserts attempt = prior_max+1 (starting at 1); bump
	// the run's attempt directly for tests that need attempt >= MaxAttempts,
	// since ClaimReady itself has no way to seed an arbitrary attempt.
	if attempt != 1 {
		if _, err := s.DB().ExecContext(ctx, `UPDATE runs SET attempt = ? WHERE run_id = ?`, attempt, runID); err != nil {
			t.Fatalf("bumping seeded run attempt: unexpected error: %v", err)
		}
	}
}

// TestRecoverOrphans_KillsLiveOrphanAndRequeues asserts that a running issue
// left behind by a dead dispatcher, with attempt below MaxAttempts, has its
// orphaned worker process killed and the issue requeued to ready (claim
// cleared, run closed 'orphaned', a setstate mirror enqueued) so the next
// tick can re-claim it.
func TestRecoverOrphans_KillsLiveOrphanAndRequeues(t *testing.T) {
	s := openTestStore(t)
	boardDir := t.TempDir()

	handle := spawnRealOrphan(t, boardDir, "issue-1", "coder")
	pid := handle.PID()
	startedAt := handle.ProcStartedAt()
	if startedAt <= 0 {
		t.Fatalf("precondition: ProcStartedAt() = %d, want > 0", startedAt)
	}

	const runID = "orphan-run"
	seedOrphanRun(t, s, "issue-1", "coder", runID, pid, startedAt, 1, 1000)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(2000))

	if err := d.RecoverOrphans(context.Background()); err != nil {
		t.Fatalf("RecoverOrphans: unexpected error: %v", err)
	}

	// Reap the zombie left by the SIGKILL (mirrors what a real Wait-goroutine
	// would do); without this the pid stays a zombie and still answers
	// kill(pid, 0), which would make the liveness assertion below unreliable.
	_, _ = handle.Wait()

	if !isProcessGone(t, pid) {
		t.Errorf("orphaned process %d still alive after RecoverOrphans", pid)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnReady) {
		t.Errorf("BoardStatus = %q, want ready (requeued)", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}

	run := getRunStatus(t, s, runID)
	if run != "orphaned" {
		t.Errorf("run status = %q, want orphaned", run)
	}

	pending, err := s.DrainPendingLinearWrites(context.Background(), 100)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending linear writes = %d, want exactly 1 (ready setstate)", len(pending))
	}
	if pending[0].Kind != "setstate" || pending[0].Target != string(contract.ColumnReady) {
		t.Errorf("pending write = %+v, want setstate -> ready", pending[0])
	}

	// Exactly one requeue: no duplicate close/requeue for the same run.
	events, err := s.ListEvents(context.Background())
	if err != nil {
		t.Fatalf("ListEvents: unexpected error: %v", err)
	}
	var requeueCount int
	for _, e := range events {
		if e.Kind == "orphan_requeue" {
			requeueCount++
		}
	}
	if requeueCount != 1 {
		t.Errorf("orphan_requeue events = %d, want exactly 1", requeueCount)
	}
}

// TestRecoverOrphans_BlocksWhenAttemptAtMax asserts that an orphaned run at
// attempt >= MaxAttempts is blocked (not requeued), with a comment enqueued
// explaining why.
func TestRecoverOrphans_BlocksWhenAttemptAtMax(t *testing.T) {
	s := openTestStore(t)
	boardDir := t.TempDir()

	handle := spawnRealOrphan(t, boardDir, "issue-1", "coder")
	pid := handle.PID()
	startedAt := handle.ProcStartedAt()

	cfg := testConfig()
	cfg.MaxAttempts = 2
	const runID = "orphan-run"
	seedOrphanRun(t, s, "issue-1", "coder", runID, pid, startedAt, cfg.MaxAttempts, 1000)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(2000))

	if err := d.RecoverOrphans(context.Background()); err != nil {
		t.Fatalf("RecoverOrphans: unexpected error: %v", err)
	}
	_, _ = handle.Wait()

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnBlocked) {
		t.Errorf("BoardStatus = %q, want blocked (max attempts reached)", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}

	run := getRunStatus(t, s, runID)
	if run != "orphaned" {
		t.Errorf("run status = %q, want orphaned", run)
	}

	pending, err := s.DrainPendingLinearWrites(context.Background(), 100)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	var sawComment, sawSetState bool
	for _, w := range pending {
		if w.Kind == "comment" {
			sawComment = true
		}
		if w.Kind == "setstate" && w.Target == string(contract.ColumnBlocked) {
			sawSetState = true
		}
	}
	if !sawComment {
		t.Errorf("pending writes = %+v, want a comment explaining the block", pending)
	}
	if !sawSetState {
		t.Errorf("pending writes = %+v, want a setstate -> blocked mirror", pending)
	}
}

// seedOrphanColumnRun mirrors seedOrphanRun for a downstream lane-entry
// column (claimed via ClaimColumn, not ClaimReady): seeds issueID sitting in
// column, claims it for lane, and records the run's pid/procStartedAt/attempt
// exactly as a real dispatcher would mid-run.
func seedOrphanColumnRun(t *testing.T, s *store.Store, issueID, column, lane, runID string, pid int, procStartedAt int64, attempt int, now int64) {
	t.Helper()
	ctx := context.Background()
	seedColumnIssue(t, s, issueID, column, 1, now)

	claim, err := s.ClaimColumn(ctx, column, lane, runID, now, 3600)
	if err != nil {
		t.Fatalf("ClaimColumn: unexpected error: %v", err)
	}
	if claim.Issue.ID != issueID {
		t.Fatalf("ClaimColumn claimed %q, want %q", claim.Issue.ID, issueID)
	}

	if err := s.SetRunProcess(ctx, runID, pid, procStartedAt); err != nil {
		t.Fatalf("SetRunProcess: unexpected error: %v", err)
	}
	if attempt != 1 {
		if _, err := s.DB().ExecContext(ctx, `UPDATE runs SET attempt = ? WHERE run_id = ?`, attempt, runID); err != nil {
			t.Fatalf("bumping seeded run attempt: unexpected error: %v", err)
		}
	}
}

// TestRecoverOrphans_DownstreamColumnRequeuesToItsOwnColumn asserts R2: an
// orphaned run claimed via ClaimColumn (review/rework/merging, not
// ClaimReady's ready->running) requeues to its OWN column —
// not to 'ready' — via the same store.ReleaseTargetColumn rule
// ReleaseStaleClaims uses, so the two release paths cannot drift apart.
func TestRecoverOrphans_DownstreamColumnRequeuesToItsOwnColumn(t *testing.T) {
	s := openTestStore(t)
	boardDir := t.TempDir()

	handle := spawnRealOrphan(t, boardDir, "issue-1", "reviewer")
	pid := handle.PID()
	startedAt := handle.ProcStartedAt()
	if startedAt <= 0 {
		t.Fatalf("precondition: ProcStartedAt() = %d, want > 0", startedAt)
	}

	const runID = "orphan-run"
	seedOrphanColumnRun(t, s, "issue-1", "review", "reviewer", runID, pid, startedAt, 1, 1000)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(2000))

	if err := d.RecoverOrphans(context.Background()); err != nil {
		t.Fatalf("RecoverOrphans: unexpected error: %v", err)
	}
	_, _ = handle.Wait()

	if !isProcessGone(t, pid) {
		t.Errorf("orphaned process %d still alive after RecoverOrphans", pid)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != "review" {
		t.Errorf("BoardStatus = %q, want unchanged %q (downstream orphan requeues to its own column, not ready)", issue.BoardStatus, "review")
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}

	run := getRunStatus(t, s, runID)
	if run != "orphaned" {
		t.Errorf("run status = %q, want orphaned", run)
	}

	pending, err := s.DrainPendingLinearWrites(context.Background(), 100)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending linear writes = %d, want exactly 1 (review setstate)", len(pending))
	}
	if pending[0].Kind != "setstate" || pending[0].Target != "review" {
		t.Errorf("pending write = %+v, want setstate -> review", pending[0])
	}
}

// TestRecoverOrphans_DownstreamColumnBlocksWhenAttemptAtMax mirrors
// TestRecoverOrphans_BlocksWhenAttemptAtMax for a downstream column: the
// attempt cap still applies (and still blocks, not requeues) regardless of
// which column the orphaned claim was in.
func TestRecoverOrphans_DownstreamColumnBlocksWhenAttemptAtMax(t *testing.T) {
	s := openTestStore(t)
	boardDir := t.TempDir()

	handle := spawnRealOrphan(t, boardDir, "issue-1", "reviewer")
	pid := handle.PID()
	startedAt := handle.ProcStartedAt()

	cfg := testConfig()
	cfg.MaxAttempts = 2
	const runID = "orphan-run"
	seedOrphanColumnRun(t, s, "issue-1", "review", "reviewer", runID, pid, startedAt, cfg.MaxAttempts, 1000)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(2000))

	if err := d.RecoverOrphans(context.Background()); err != nil {
		t.Fatalf("RecoverOrphans: unexpected error: %v", err)
	}
	_, _ = handle.Wait()

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnBlocked) {
		t.Errorf("BoardStatus = %q, want blocked (max attempts reached, even from a downstream column)", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}
}

// TestRecoverOrphans_GitOperatorLaneIgnoresAttemptCap asserts the fix for
// finding 5: the git-operator lane's "attempt" counter inflates on every
// CI-pending recheck cycle (claimAndRunGitops re-claims "merging" roughly
// every PollIntervalS -- see mergingTTL's doc comment), not on a genuine
// failure, so a restart mid-CI-wait must not mistake a high accumulated
// attempt count for N real failures and park an otherwise-healthy card.
// Unlike TestRecoverOrphans_DownstreamColumnBlocksWhenAttemptAtMax (the
// reviewer lane, where the cap correctly still applies), a git_operator
// orphan always requeues back to merging regardless of Attempt.
func TestRecoverOrphans_GitOperatorLaneIgnoresAttemptCap(t *testing.T) {
	s := openTestStore(t)
	boardDir := t.TempDir()

	handle := spawnRealOrphan(t, boardDir, "issue-1", "git_operator")
	pid := handle.PID()
	startedAt := handle.ProcStartedAt()

	cfg := testConfig()
	cfg.MaxAttempts = 2
	const runID = "orphan-run"
	// Attempt is far past MaxAttempts -- exactly what dozens of CI-pending
	// recheck cycles would accumulate on a slow CI run, not a real failure.
	seedOrphanColumnRun(t, s, "issue-1", "merging", "git_operator", runID, pid, startedAt, 50, 1000)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(2000))

	if err := d.RecoverOrphans(context.Background()); err != nil {
		t.Fatalf("RecoverOrphans: unexpected error: %v", err)
	}
	_, _ = handle.Wait()

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnMerging) {
		t.Errorf("BoardStatus = %q, want merging (git-operator orphan requeues regardless of attempt)", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}

	run := getRunStatus(t, s, runID)
	if run != "orphaned" {
		t.Errorf("run status = %q, want orphaned", run)
	}
}

// TestRecoverOrphans_AlreadyDeadPIDRequeuesWithoutError asserts that a run
// whose worker_pid is already gone (the process exited or was never
// recorded) is requeued (or blocked, per the attempt cap) without
// RecoverOrphans erroring — ReapOrphan's AlreadyGone case flows through the
// same close+requeue path as an actively-killed orphan.
func TestRecoverOrphans_AlreadyDeadPIDRequeuesWithoutError(t *testing.T) {
	s := openTestStore(t)

	const bogusPID = 999999
	seedOrphanRun(t, s, "issue-1", "coder", "dead-run", bogusPID, 12345, 1, 1000)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(2000))

	if err := d.RecoverOrphans(context.Background()); err != nil {
		t.Fatalf("RecoverOrphans: unexpected error: %v", err)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnReady) {
		t.Errorf("BoardStatus = %q, want ready (requeued)", issue.BoardStatus)
	}

	run := getRunStatus(t, s, "dead-run")
	if run != "orphaned" {
		t.Errorf("run status = %q, want orphaned", run)
	}
}

// TestRecoverOrphans_ReworkColumnRequeue_DoesNotDoubleCountReworkCount
// asserts the fix for requeueOrphan's interaction with rework_count: an
// issue that was ALREADY sitting in the "rework" column (a genuine prior
// review->rework edge already bumped rework_count once) when its
// ClaimColumn claim orphaned must not have rework_count bumped AGAIN just
// because requeueOrphan's Transition re-asserts NewStatus="rework" (the
// SAME column, per store.ReleaseTargetColumn) — that is a claim release,
// not a fresh review/merging->rework edge, and must not count against
// amendment C1's rework_cap.
func TestRecoverOrphans_ReworkColumnRequeue_DoesNotDoubleCountReworkCount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const runID = "orphan-run"
	// A bogus/already-dead pid (mirrors
	// TestRecoverOrphans_AlreadyDeadPIDRequeuesWithoutError) keeps this test
	// process-free — the double-count reproduces regardless of whether the
	// orphaned worker process itself is still alive.
	seedOrphanColumnRun(t, s, "issue-1", "rework", "coder", runID, 999999, 12345, 1, 1000)

	// Simulate the issue having already cycled into rework once for real
	// (rework_count=1) before this now-orphaned claim was ever taken.
	if _, err := s.DB().ExecContext(ctx, `UPDATE issues SET rework_count = 1 WHERE id = ?`, "issue-1"); err != nil {
		t.Fatalf("seeding rework_count: unexpected error: %v", err)
	}

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(2000))

	if err := d.RecoverOrphans(ctx); err != nil {
		t.Fatalf("RecoverOrphans: unexpected error: %v", err)
	}

	issue, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != "rework" {
		t.Errorf("BoardStatus = %q, want unchanged rework (orphan requeue re-asserts its own column)", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}
	if issue.ReworkCount != 1 {
		t.Errorf("ReworkCount = %d, want unchanged 1 (orphan requeue must not double-count)", issue.ReworkCount)
	}
}

// TestRecoverOrphans_TerminalIssueTerminalizesRunWithoutFlapping asserts that
// an open run whose issue already finished (board_status done/cancelled) is
// just closed as leftover restart debris -- NOT flapped back to blocked and
// mirrored to Linear (the Reflex retro: done tickets un-done by every
// restart). The check must win even when the run is past max_attempts (which
// would otherwise block it).
func TestRecoverOrphans_TerminalIssueTerminalizesRunWithoutFlapping(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const runID = "orphan-run"
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	if _, err := s.ClaimReady(ctx, "coder", runID, 1000, 3600); err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}
	cfg := testConfig()
	// Past max_attempts so, absent the terminal-state guard, blockOrphan would
	// flap the done issue to blocked.
	if _, err := s.DB().ExecContext(ctx, `UPDATE runs SET attempt = ? WHERE run_id = ?`, cfg.MaxAttempts, runID); err != nil {
		t.Fatalf("bumping run attempt: unexpected error: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE issues SET board_status = 'done' WHERE id = ?`, "issue-1"); err != nil {
		t.Fatalf("marking issue done: unexpected error: %v", err)
	}

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(2000))

	if err := d.RecoverOrphans(ctx); err != nil {
		t.Fatalf("RecoverOrphans: unexpected error: %v", err)
	}

	issue, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnDone) {
		t.Errorf("BoardStatus = %q, want unchanged done (terminal issue must not flap)", issue.BoardStatus)
	}

	if run := getRunStatus(t, s, runID); run != "terminalized" {
		t.Errorf("run status = %q, want terminalized", run)
	}

	pending, err := s.DrainPendingLinearWrites(ctx, 100)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending linear writes = %+v, want 0 (no set-state or comment for a terminal issue)", pending)
	}
}

// getRunStatus fetches a single run's status by id via the store's raw DB
// handle (there is no dedicated GetRun accessor for a single row).
func getRunStatus(t *testing.T, s *store.Store, runID string) string {
	t.Helper()
	var status string
	row := s.DB().QueryRowContext(context.Background(), `SELECT status FROM runs WHERE run_id = ?`, runID)
	if err := row.Scan(&status); err != nil {
		t.Fatalf("querying run %s status: %v", runID, err)
	}
	return status
}

// isProcessGone reports whether pid no longer exists (signal 0 fails),
// polling briefly since SIGKILL delivery is not instantaneous.
func isProcessGone(t *testing.T, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
