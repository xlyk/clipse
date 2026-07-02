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
	return d.applyTerminalWorkerOutcome(ctx, *issue, rr.runID, inf.lane, rr.res.Worker)
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

// applyTerminalWorkerOutcome handles every non-"continue" outcome
// (needs_review/changes_requested/blocked/done): computes the next column
// via board.Next and applies the resulting transition, or blocks defensively
// if board.Next reports the transition as illegal. It is the single path
// shared by a spawned worker's finished run (applyResult, above) and an
// inline gitops pass over a claimed "merging" card (applyGitopsResult, in
// gitops.go) — both a Reviewer lane's changes_requested from review and a
// Git-operator lane's stale-base-conflict route from merging land here
// identically, so both go through the exact same rework-cap check (amendment
// C1), Linear-mirror enqueue, and event trail.
func (d *Dispatcher) applyTerminalWorkerOutcome(ctx context.Context, issue store.Issue, runID, lane string, result contract.WorkerResult) error {
	outcome := string(result.Outcome)
	next, action, err := board.Next(outcome, issue.BoardStatus)
	if err != nil {
		reason := fmt.Sprintf("illegal transition: outcome %q from column %q: %s", outcome, issue.BoardStatus, err.Error())
		return d.blockRun(ctx, issue, runID, lane, reason)
	}

	if next == string(contract.ColumnRework) {
		blocked, err := d.blockIfReworkCapExceeded(ctx, issue, runID, lane, result)
		if err != nil {
			return err
		}
		if blocked {
			return nil
		}
	}

	resultJSON, err := marshalWorkerResult(result)
	if err != nil {
		return fmt.Errorf("marshaling result for run %s: %w", runID, err)
	}

	now := d.now()
	comment := commentFor(outcome, lane, result)

	req := store.TransitionReq{
		IssueID:         issue.ID,
		NewStatus:       next,
		ClearClaim:      true,
		CloseRunID:      runID,
		RunStatus:       outcome,
		ResultJSON:      resultJSON,
		TokensIn:        result.Tokens.In,
		TokensOut:       result.Tokens.Out,
		EnqueueSetState: true,
		Comment:         comment,
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issue.ID),
			RunID:   nullString(runID),
			Kind:    action,
			Detail:  result.Summary,
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("transitioning issue %s: %w", issue.ID, err)
	}
	return nil
}

// blockIfReworkCapExceeded intercepts a would-be transition to rework once
// issue has already cycled through rework cfg.ReworkCap times (amendment
// C1): store.Transition increments issues.rework_count unconditionally on
// any transition to "rework" (see internal/store/outbox.go), so this check
// must run BEFORE that transition is applied. Once incrementing would push
// rework_count past the cap, it parks the issue in Blocked instead — with a
// comment naming the cap, the PR (if any), and the last review/conflict
// reason — rather than ever calling Transition with NewStatus="rework"
// again, so rework_count never ticks past the cap and a permanently
// disagreeing Reviewer (or a repeatedly-conflicting stale base) can't loop
// the issue between Coder and Reviewer forever.
//
// This applies identically regardless of which lane routed the card to
// rework: the Reviewer lane's changes_requested from review, or the
// Git-operator lane's stale-base-conflict route from merging (R1) — both
// increment the SAME issues.rework_count counter.
func (d *Dispatcher) blockIfReworkCapExceeded(ctx context.Context, issue store.Issue, runID, lane string, result contract.WorkerResult) (blocked bool, err error) {
	if issue.ReworkCount+1 <= d.cfg.ReworkCap {
		return false, nil
	}

	resultJSON, err := marshalWorkerResult(result)
	if err != nil {
		return false, fmt.Errorf("marshaling result for run %s: %w", runID, err)
	}

	now := d.now()
	reason := reworkCapExceededReason(d.cfg.ReworkCap, result)
	req := store.TransitionReq{
		IssueID:         issue.ID,
		NewStatus:       string(contract.ColumnBlocked),
		ClearClaim:      true,
		CloseRunID:      runID,
		RunStatus:       string(result.Outcome),
		ResultJSON:      resultJSON,
		TokensIn:        result.Tokens.In,
		TokensOut:       result.Tokens.Out,
		EnqueueSetState: true,
		Comment:         reason,
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issue.ID),
			RunID:   nullString(runID),
			Kind:    "rework_cap_exceeded",
			Detail:  reason,
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return false, fmt.Errorf("blocking issue %s at rework cap: %w", issue.ID, err)
	}
	d.logger.Warn("rework cap exceeded, issue blocked", "issue_id", issue.ID, "run_id", runID, "lane", lane, "rework_cap", d.cfg.ReworkCap)
	return true, nil
}

// reworkCapExceededReason renders the Linear comment body for a rework-cap
// block: the cap itself, the PR under review (if the result carries one),
// and the last review/conflict summary that tipped it over — so a human
// looking at the Blocked column doesn't need to open the store to see why.
func reworkCapExceededReason(cap int, result contract.WorkerResult) string {
	reason := fmt.Sprintf("rework cap (%d) exceeded", cap)
	if result.PrUrl != nil && *result.PrUrl != "" {
		reason += fmt.Sprintf("; PR: %s", *result.PrUrl)
	}
	if result.Summary != "" {
		reason += fmt.Sprintf("; last review: %s", result.Summary)
	}
	return reason
}

// commentFor decides whether a terminal transition gets a Linear comment,
// and what it says. "blocked" always gets one (blockCommentFor). A
// changes_requested from the Git-operator lane also gets one: unlike the
// Reviewer lane (which posts its own inline PR review comments — see
// graphs/reviewer.py's post_comments — before ever emitting
// changes_requested), internal/gitops's stale-base-conflict route never
// posts anything to the PR itself, so this comment (naming the conflicting
// files, folded into result.Summary by staleBaseConflictSummary) is the
// only place a human sees why the card landed back in Rework. Every other
// outcome (done, needs_review, and a Reviewer's own changes_requested) gets
// no dispatcher-authored comment.
func commentFor(outcome, lane string, result contract.WorkerResult) string {
	switch {
	case outcome == string(contract.WorkerResultOutcomeBlocked):
		return blockCommentFor(result)
	case outcome == string(contract.WorkerResultOutcomeChangesRequested) && lane == string(contract.LaneGitOperator):
		if result.Summary != "" {
			return fmt.Sprintf("rework: %s", result.Summary)
		}
		return "rework: stale base conflict"
	default:
		return ""
	}
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
