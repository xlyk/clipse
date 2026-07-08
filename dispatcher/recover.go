package dispatcher

import (
	"context"
	"fmt"

	"github.com/xlyk/clipse/internal/contract"
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

	// "cancelled" (like "done") is a genuinely terminal issue whose leftover
	// run row is restart debris, not a real orphan -- see promote.go's
	// terminalStatuses for how a store row actually reaches this string.
	if issue.BoardStatus == "done" || issue.BoardStatus == "cancelled" {
		// The issue already finished; this run row is just restart debris.
		// Blocking here would flap a terminal ticket back to blocked and
		// mirror that to Linear (Reflex retro: done tickets un-done by every
		// restart). Close the run and leave the issue alone. CloseRun's extra
		// args (resultJSON/errStr/tokens) are empty here -- there is no worker
		// result to record for leftover debris.
		if err := d.store.CloseRun(ctx, run.RunID, "terminalized", "", "", 0, 0); err != nil {
			return fmt.Errorf("terminalizing leftover run %s on %s issue %s: %w", run.RunID, issue.BoardStatus, issue.ID, err)
		}
		d.logger.Info("orphan run terminalized (issue already terminal)", "issue_id", issue.ID, "run_id", run.RunID, "board_status", issue.BoardStatus)
		return nil
	}

	if run.Lane == string(contract.LaneGitOperator) {
		// The git-operator lane's "attempt" counter inflates on every
		// CI-pending recheck cycle (claimAndRunGitops re-claims "merging"
		// roughly every PollIntervalS via mergingTTL's short natural expiry
		// -- see mergingTTL's doc comment), NOT on a genuine failure: gitops
		// itself decides retriability per outcome (applyGitopsResult's
		// OutcomeNotMergeable branch, bounded separately and persistently by
		// issues.recover_attempts via parkOrRetry -- a store column, unlike
		// this run-row-derived MaxAttempts check, so it survives a restart
		// intact). A restart mid-CI-wait must not mistake "still waiting" for
		// "N failed attempts" and park an otherwise-healthy card -- always
		// requeue it back to merging regardless of Attempt.
		return d.requeueOrphan(ctx, *issue, run)
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

// requeueOrphan closes run as 'orphaned' and returns its issue to a
// claimable state (clearing the claim) so a later tick's selectAndClaim can
// re-claim it — the re-claim's next attempt is computed as prior-max+1
// (issue-global, R5), so it naturally becomes attempt+1 regardless of which
// column/lane re-claims it.
//
// The target column is column-aware (R2), via the exact same
// store.ReleaseTargetColumn rule ReleaseStaleClaims uses, so the two release
// paths cannot drift apart: an orphaned 'running' (Coder) claim returns to
// 'ready', while an orphaned downstream claim (review/rework/merging,
// entered via ClaimColumn) keeps its own column — it never left
// that column in the first place, only claim_lock did.
func (d *Dispatcher) requeueOrphan(ctx context.Context, issue store.Issue, run store.Run) error {
	now := d.now()
	target := store.ReleaseTargetColumn(issue.BoardStatus)
	req := store.TransitionReq{
		IssueID:    issue.ID,
		NewStatus:  target,
		ClearClaim: true,
		// requeueOrphan's target is always either "ready" (released from
		// "running") or issue.BoardStatus unchanged (every downstream
		// column, via ReleaseTargetColumn) — never a genuine edge INTO
		// rework from some other column. When target=="rework" this is a
		// claim-release re-assert of a column the card was already sitting
		// in, so it must not double-count against amendment C1's
		// rework_cap (see TransitionReq.SkipReworkBump).
		SkipReworkBump:  true,
		CloseRunID:      run.RunID,
		RunStatus:       "orphaned",
		EnqueueSetState: true,
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issue.ID),
			RunID:   nullString(run.RunID),
			Kind:    "orphan_requeue",
			Detail:  fmt.Sprintf("orphaned by dispatcher restart; requeued to %s (attempt %d/%d)", target, run.Attempt, d.cfg.MaxAttempts),
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("requeuing orphaned issue %s: %w", issue.ID, err)
	}
	d.logger.Warn("orphan run requeued", "issue_id", issue.ID, "run_id", run.RunID, "attempt", run.Attempt, "target", target)
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
		Comment:         blockedComment("", reason),
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
