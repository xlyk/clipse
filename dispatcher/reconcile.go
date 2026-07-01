package dispatcher

import (
	"context"
	"errors"
	"fmt"

	"github.com/xlyk/clipse/internal/board"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// reconcile drains finished-run results, heartbeats every claim still
// in-flight after that drain, and releases any claim that has gone stale
// (missed heartbeats past its TTL). max_runtime kills are not a separate
// step here: the spawn context deadline set in spawnAttempt already causes
// handle.Wait() to return a context.DeadlineExceeded result, which arrives
// through the normal drain -> applyResult path and is mapped to blocked.
func (d *Dispatcher) reconcile(ctx context.Context) error {
	if err := d.drainResults(ctx); err != nil {
		return fmt.Errorf("draining results: %w", err)
	}

	now := d.now()
	for runID := range d.inflight {
		if err := d.store.Heartbeat(ctx, runID, now, d.ttl()); err != nil {
			return fmt.Errorf("heartbeating run %s: %w", runID, err)
		}
	}

	if _, err := d.store.ReleaseStaleClaims(ctx, now); err != nil {
		return fmt.Errorf("releasing stale claims: %w", err)
	}
	return nil
}

// drainResults applies every result currently buffered on d.results without
// blocking: it is what keeps Tick from ever waiting on a still-running
// worker.
func (d *Dispatcher) drainResults(ctx context.Context) error {
	for {
		select {
		case rr := <-d.results:
			if err := d.applyResult(ctx, rr); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// applyResult maps one finished run's result onto a board transition (or a
// continuation re-spawn). It runs only on the Tick goroutine, so it is free
// to read and mutate d.inflight directly.
func (d *Dispatcher) applyResult(ctx context.Context, rr runResult) error {
	inf, ok := d.inflight[rr.runID]
	if !ok {
		// No inflight record (e.g. reconciled already via some other path);
		// nothing left to do for this result.
		return nil
	}
	inf.cancel()

	issue, err := d.store.GetIssue(ctx, rr.issueID)
	if err != nil {
		return fmt.Errorf("loading issue %s for run %s: %w", rr.issueID, rr.runID, err)
	}

	if rr.res.Err != nil {
		delete(d.inflight, rr.runID)
		return d.blockRun(ctx, *issue, rr.runID, inf.lane, blockReasonFor(rr.res.Err))
	}

	outcome := string(rr.res.Worker.Outcome)

	if outcome == string(contract.WorkerResultOutcomeContinue) {
		return d.applyContinue(ctx, *issue, rr, inf)
	}

	delete(d.inflight, rr.runID)
	return d.applyTerminalOutcome(ctx, *issue, rr, inf, outcome)
}

// blockReasonFor renders a human-readable reason for a Spawner/RunHandle
// failure mode (crash, malformed result, or timeout), for the Comment
// enqueued on the resulting blocked transition.
func blockReasonFor(err error) string {
	switch {
	case errors.Is(err, spawn.ErrWorkerExit):
		return fmt.Sprintf("worker exited nonzero: %s", err.Error())
	case errors.Is(err, spawn.ErrMalformedResult):
		return fmt.Sprintf("worker produced a malformed result: %s", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("worker exceeded max runtime: %s", err.Error())
	default:
		return err.Error()
	}
}

// blockRun transitions issue to blocked after a run-level failure (crash,
// malformed result, timeout, or turn-cap exhaustion): clears the claim,
// closes the run, and enqueues both the Linear mirror and a comment
// explaining why. No auto-retry: a human must requeue a blocked issue.
func (d *Dispatcher) blockRun(ctx context.Context, issue store.Issue, runID, lane, reason string) error {
	now := d.now()
	req := store.TransitionReq{
		IssueID:         issue.ID,
		NewStatus:       "blocked",
		ClearClaim:      true,
		CloseRunID:      runID,
		RunStatus:       "blocked",
		RunError:        reason,
		EnqueueSetState: true,
		Comment:         fmt.Sprintf("blocked: %s", reason),
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issue.ID),
			RunID:   nullString(runID),
			Kind:    "blocked",
			Detail:  reason,
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("blocking issue %s: %w", issue.ID, err)
	}
	d.logger.Info("run blocked", "issue_id", issue.ID, "run_id", runID, "lane", lane, "reason", reason)
	return nil
}

// applyContinue handles a "continue" outcome: re-spawn the same run for
// another turn (reusing the worktree and the worker's returned thread id) if
// under cfg.TurnCap, otherwise block for reaching the turn cap.
func (d *Dispatcher) applyContinue(ctx context.Context, issue store.Issue, rr runResult, inf inflightRun) error {
	if inf.turn >= d.cfg.TurnCap {
		delete(d.inflight, rr.runID)
		return d.blockRun(ctx, issue, rr.runID, inf.lane, "turn cap reached")
	}

	newTurn, err := d.store.BumpRunTurn(ctx, rr.runID)
	if err != nil {
		return fmt.Errorf("bumping turn for run %s: %w", rr.runID, err)
	}

	if err := d.spawnAttempt(ctx, issue, rr.runID, inf.lane, rr.res.Worker.ThreadId, newTurn); err != nil {
		return fmt.Errorf("respawning run %s for continuation: %w", rr.runID, err)
	}
	return nil
}

// applyTerminalOutcome handles every non-"continue" outcome
// (needs_review/changes_requested/blocked/done): computes the next column
// via board.Next and applies the resulting transition, or blocks defensively
// if board.Next reports the transition as illegal.
func (d *Dispatcher) applyTerminalOutcome(ctx context.Context, issue store.Issue, rr runResult, inf inflightRun, outcome string) error {
	next, action, err := board.Next(outcome, issue.BoardStatus)
	if err != nil {
		reason := fmt.Sprintf("illegal transition: outcome %q from column %q: %s", outcome, issue.BoardStatus, err.Error())
		return d.blockRun(ctx, issue, rr.runID, inf.lane, reason)
	}

	resultJSON, err := marshalWorkerResult(rr.res.Worker)
	if err != nil {
		return fmt.Errorf("marshaling result for run %s: %w", rr.runID, err)
	}

	now := d.now()
	comment := ""
	if outcome == string(contract.WorkerResultOutcomeBlocked) {
		comment = blockCommentFor(rr.res.Worker)
	}

	req := store.TransitionReq{
		IssueID:         issue.ID,
		NewStatus:       next,
		ClearClaim:      true,
		CloseRunID:      rr.runID,
		RunStatus:       outcome,
		ResultJSON:      resultJSON,
		TokensIn:        rr.res.Worker.Tokens.In,
		TokensOut:       rr.res.Worker.Tokens.Out,
		EnqueueSetState: true,
		Comment:         comment,
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issue.ID),
			RunID:   nullString(rr.runID),
			Kind:    action,
			Detail:  rr.res.Worker.Summary,
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("transitioning issue %s: %w", issue.ID, err)
	}
	return nil
}

// blockCommentFor renders the Linear comment body for a "blocked" outcome
// from the worker's own block_kind + summary, so a human reviewing the
// Blocked column in Linear sees why without opening the store.
func blockCommentFor(w contract.WorkerResult) string {
	kind := "unknown"
	if w.BlockKind != nil {
		kind = string(*w.BlockKind)
	}
	if w.Summary != "" {
		return fmt.Sprintf("blocked (%s): %s", kind, w.Summary)
	}
	return fmt.Sprintf("blocked (%s)", kind)
}
