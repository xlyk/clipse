package dispatcher

import (
	"context"
	"fmt"

	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// RecoverOrphans is called exactly once at daemon startup, before Tick's
// loop begins (and before any stale-claim release), to close the
// double-run hole a dispatcher restart would otherwise open: every run this
// store still records as 'running' was left behind by a dispatcher process
// that is no longer around to reconcile it (this process just started, so
// none of them are in d.inflight). Each such run's worker process is
// checked and — if it is still alive and verified to be the same process
// (not one the OS has since reused the pid for) — killed, then the run is
// closed and the issue is either requeued to ready (to be re-claimed) or
// blocked, depending on whether it has exhausted cfg.MaxAttempts.
//
// This is decision H's restart case: a dispatcher restart is an
// infrastructure event, not a worker failure, so it requeues under the
// attempt cap rather than unconditionally blocking. Re-running is safe
// because PR creation is idempotent and the worktree persists across the
// restart.
func (d *Dispatcher) RecoverOrphans(ctx context.Context) error {
	runs, err := d.store.ListOpenRuns(ctx)
	if err != nil {
		return fmt.Errorf("recovering orphans: listing open runs: %w", err)
	}

	for _, run := range runs {
		if err := d.recoverOrphanRun(ctx, run); err != nil {
			return fmt.Errorf("recovering orphan run %s: %w", run.RunID, err)
		}
	}
	return nil
}

// recoverOrphanRun reaps (if still alive) run's worker process, then closes
// the run and either requeues or blocks the owning issue.
func (d *Dispatcher) recoverOrphanRun(ctx context.Context, run store.Run) error {
	reaped, err := d.reapRunProcess(run)
	if err != nil {
		return fmt.Errorf("reaping process: %w", err)
	}
	d.logger.Info("orphan run recovery: process check", "run_id", run.RunID, "issue_id", run.IssueID, "outcome", reaped.String())

	issue, err := d.store.GetIssue(ctx, run.IssueID)
	if err != nil {
		return fmt.Errorf("loading issue %s: %w", run.IssueID, err)
	}

	if run.Attempt >= d.cfg.MaxAttempts {
		return d.blockOrphan(ctx, *issue, run)
	}
	return d.requeueOrphan(ctx, *issue, run)
}

// reapRunProcess checks run's recorded worker process and kills it if still
// alive and identity-verified. A run with no recorded pid (WorkerPID not
// valid — e.g. it never got past spawnAttempt's Spawn call) is treated as
// already gone: there is nothing to check or kill.
func (d *Dispatcher) reapRunProcess(run store.Run) (spawn.Reaped, error) {
	if !run.WorkerPID.Valid {
		return spawn.ReapedAlreadyGone, nil
	}
	var expectedProcStartedAt int64
	if run.ProcStartedAt.Valid {
		expectedProcStartedAt = run.ProcStartedAt.Int64
	}
	return spawn.ReapOrphan(int(run.WorkerPID.Int64), expectedProcStartedAt)
}

// requeueOrphan closes run as 'orphaned' and moves its issue back to ready
// (clearing the claim), so the next tick's selectAndClaim can re-claim it —
// ClaimReady computes the new attempt as prior-max+1, so the re-claim
// naturally becomes attempt+1.
func (d *Dispatcher) requeueOrphan(ctx context.Context, issue store.Issue, run store.Run) error {
	now := d.now()
	req := store.TransitionReq{
		IssueID:         issue.ID,
		NewStatus:       "ready",
		ClearClaim:      true,
		CloseRunID:      run.RunID,
		RunStatus:       "orphaned",
		EnqueueSetState: true,
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issue.ID),
			RunID:   nullString(run.RunID),
			Kind:    "orphan_requeue",
			Detail:  fmt.Sprintf("orphaned by dispatcher restart; requeued (attempt %d/%d)", run.Attempt, d.cfg.MaxAttempts),
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("requeuing orphaned issue %s: %w", issue.ID, err)
	}
	d.logger.Warn("orphan run requeued", "issue_id", issue.ID, "run_id", run.RunID, "attempt", run.Attempt)
	return nil
}

// blockOrphan closes run as 'orphaned' and blocks its issue: the attempt cap
// has already been exhausted, so a further requeue would just be
// re-claimed and re-fail (or re-orphan) indefinitely. A human must requeue a
// blocked issue.
func (d *Dispatcher) blockOrphan(ctx context.Context, issue store.Issue, run store.Run) error {
	now := d.now()
	reason := "orphaned by dispatcher restart; max_attempts reached"
	req := store.TransitionReq{
		IssueID:         issue.ID,
		NewStatus:       "blocked",
		ClearClaim:      true,
		CloseRunID:      run.RunID,
		RunStatus:       "orphaned",
		EnqueueSetState: true,
		Comment:         reason,
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issue.ID),
			RunID:   nullString(run.RunID),
			Kind:    "orphaned",
			Detail:  reason,
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("blocking orphaned issue %s: %w", issue.ID, err)
	}
	d.logger.Warn("orphan run blocked (max attempts reached)", "issue_id", issue.ID, "run_id", run.RunID, "attempt", run.Attempt)
	return nil
}
