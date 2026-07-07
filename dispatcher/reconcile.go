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
		// Put rr back on the channel rather than drop it: rr's Wait-goroutine
		// has already exited (a one-shot send), so if this error propagates
		// without holding onto rr somehow, that run's actual outcome is lost
		// forever -- inf stays in d.inflight, Heartbeat keeps renewing its
		// still-valid store claim every tick, and the lane-cap slot it
		// occupies never frees. d.results has exactly one reader (this Tick
		// goroutine), so sending back into it here is race-free; the next
		// tick's drainResults retries applyResult for rr from the top. A
		// GetIssue failure has no other realistic cause than a transient
		// store hiccup (nothing in production deletes issue rows), so this
		// self-heals within a tick or two.
		//
		// The send is non-blocking: this is the Tick goroutine, d.results'
		// ONLY reader, so a plain blocking send here would deadlock the whole
		// dispatcher forever if the buffer were ever full (nothing else could
		// drain it to make room). resultsBufferSize's cfg.Caps.Global+1 floor
		// (dispatcher.go) should make that unreachable, but if it happens
		// anyway, the fallback is neither to block nor to drop rr on the
		// floor: inf is simply left in d.inflight (already true -- it was
		// never deleted on this path), so reconcile's Heartbeat loop keeps
		// its claim alive and the run stays visible for a later retry,
		// instead of vanishing silently.
		select {
		case d.results <- rr:
		default:
			d.logger.Error("results channel full, could not requeue result; leaving run inflight for a later retry",
				"run_id", rr.runID, "issue_id", rr.issueID, "buffer_cap", cap(d.results))
		}
		return fmt.Errorf("loading issue %s for run %s: %w", rr.issueID, rr.runID, err)
	}

	if rr.res.Err != nil {
		delete(d.inflight, rr.runID)
		reason := blockReasonFor(rr.res.Err)
		// A run-level failure (crash / malformed result / timeout) is transient
		// by nature, so it is eligible for bounded auto-retry (auto-unblock
		// layer 1); parkOrRetry falls back to the plain blockRun park once the
		// budget is spent (or when RecoverCap is 0).
		return d.parkOrRetry(ctx, *issue, rr.runID, inf.lane, reason, contract.BlockKindTransient, d.now(), retryPayload{}, func() error {
			return d.blockRun(ctx, *issue, rr.runID, inf.lane, reason)
		})
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

// parkOrRetry is auto-unblock layer 1's single decision point. A *transient*
// failure with retry budget left (issue.recover_attempts < cfg.RecoverCap) is
// re-queued for a bounded, backed-off deterministic retry (scheduleRetry);
// everything else is parked via the caller-supplied park closure — so each
// call site keeps its exact park payload (a run-level blockRun, the terminal
// blocked transition with tokens + block-kind comment, or blockOnSpawnFailure).
//
// Only transient failures are eligible. Crash/malformed/timeout/spawn callers
// pass BlockKindTransient (they are transient by nature); a terminal worker
// block passes its own block_kind, so capability (token ceiling — a retry just
// burns another budget) and needs_input (needs a human answer) fall through to
// park. A RecoverCap of 0 disables retry entirely (every failure parks — the
// pre-layer-1 behavior). Callers that must NEVER auto-retry (turn-cap,
// illegal-transition, rework-cap, orphan max_attempts) simply do not route
// through here.
func (d *Dispatcher) parkOrRetry(ctx context.Context, issue store.Issue, runID, lane, reason string, blockKind contract.BlockKind, now int64, payload retryPayload, park func() error) error {
	if blockKind == contract.BlockKindTransient && issue.RecoverAttempts < d.cfg.RecoverCap {
		return d.scheduleRetry(ctx, issue, runID, lane, reason, now, payload)
	}
	return park()
}

// retryPayload carries the run-close fields scheduleRetry records on the
// retried run. Crash/malformed/timeout/spawn failures have no worker result and
// pass the zero value; only a terminal worker block carries tokens + result
// JSON worth preserving so board-wide token accounting isn't dropped when a
// blocked turn is retried.
type retryPayload struct {
	tokensIn   int
	tokensOut  int
	resultJSON string
}

// scheduleRetry re-queues issue for a bounded, deterministic auto-retry after a
// transient failure. It is a single store.Transition (the one Linear-mirroring
// writer) that: returns the card to its release column
// (store.ReleaseTargetColumn — running->ready for a coder-from-ready run, every
// downstream/rework column staying put so the same lane re-claims it), clears
// the claim, closes the run as "retry_scheduled", bumps recover_attempts, and
// sets blocked_until = now + RecoverBackoffS so the card is invisible to every
// claim/peek until the backoff passes (the anti-hot-loop half of the
// guarantee). SkipReworkBump is set because the release column can be "rework"
// (a coder re-run that transient-failed), which must not spend amendment C1's
// rework budget. The column change is mirrored to Linear only when it actually
// moves (a coder ready<-running requeue); a same-column requeue enqueues no
// redundant setstate (R5).
func (d *Dispatcher) scheduleRetry(ctx context.Context, issue store.Issue, runID, lane, reason string, now int64, payload retryPayload) error {
	target := store.ReleaseTargetColumn(issue.BoardStatus)
	attempt := issue.RecoverAttempts + 1
	blockedUntil := now + int64(d.cfg.RecoverBackoffS)
	req := store.TransitionReq{
		IssueID:             issue.ID,
		NewStatus:           target,
		ClearClaim:          true,
		SkipReworkBump:      true,
		BumpRecoverAttempts: true,
		SetBlockedUntil:     blockedUntil,
		CloseRunID:          runID,
		RunStatus:           "retry_scheduled",
		RunError:            reason,
		ResultJSON:          payload.resultJSON,
		TokensIn:            payload.tokensIn,
		TokensOut:           payload.tokensOut,
		EnqueueSetState:     target != issue.BoardStatus,
		Comment:             retryComment(attempt, d.cfg.RecoverCap, reason),
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issue.ID),
			RunID:   nullString(runID),
			Kind:    "retry_scheduled",
			Detail:  fmt.Sprintf("auto-retry %d/%d after transient failure: %s", attempt, d.cfg.RecoverCap, reason),
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("scheduling auto-retry for issue %s: %w", issue.ID, err)
	}
	d.logger.Info("transient failure auto-retry scheduled",
		"issue_id", issue.ID, "run_id", runID, "lane", lane,
		"recover_attempt", attempt, "recover_cap", d.cfg.RecoverCap,
		"target", target, "blocked_until", blockedUntil, "reason", reason)
	return nil
}

// blockKindOf returns result's block_kind, or "" when absent (block_kind is
// present iff outcome=="blocked", but "" reads as non-transient here, so an
// unexpectedly-absent kind parks rather than retries — the safe default).
func blockKindOf(result contract.WorkerResult) contract.BlockKind {
	if result.BlockKind != nil {
		return *result.BlockKind
	}
	return ""
}

// blockRun transitions issue to blocked after a run-level failure, clearing the
// claim, closing the run, and enqueuing both the Linear mirror and a reason
// comment. It is the terminal park: for turn-cap exhaustion and
// illegal-transition it is called directly; for a crash/malformed/timeout it is
// the park fallback parkOrRetry uses once the auto-retry budget is spent. A
// parked issue is never auto-requeued — a human must move it.
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
		Comment:         blockedComment("", reason),
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

	// A continuation resumes the same thread the previous turn checkpointed;
	// its context carries forward on that thread, so no fresh rework feedback
	// is injected here (the turn-cap continue path is unused for the DAC coder
	// anyway — see AGENTS.md).
	if err := d.spawnAttempt(ctx, issue, rr.runID, inf.lane, rr.res.Worker.ThreadId, newTurn, ""); err != nil {
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
	// Post the lane's structured handoff note on this terminal outcome (the
	// write side of the per-run handoff loop). When the outcome already has a
	// dispatcher comment (a gitops stale-base changes_requested, a block
	// reason), the handoff rides after it separated by a blank line; otherwise
	// it stands alone. A "continue" outcome never reaches here, so a handoff is
	// only ever posted on a genuinely terminal transition.
	if result.Handoff != nil && *result.Handoff != "" {
		hc := handoffComment(lane, outcome, *result.Handoff)
		if comment == "" {
			comment = hc
		} else {
			comment += "\n\n" + hc
		}
	}

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

	if outcome == string(contract.WorkerResultOutcomeBlocked) {
		// A worker-emitted block is eligible for bounded auto-retry only when
		// its block_kind is transient (capability/needs_input fall through to
		// park). The park path is exactly the generic blocked transition built
		// above — preserving the tokens, result JSON, and block-kind comment
		// that a plain blockRun would drop.
		return d.parkOrRetry(ctx, issue, runID, lane, result.Summary, blockKindOf(result), now,
			retryPayload{tokensIn: result.Tokens.In, tokensOut: result.Tokens.Out, resultJSON: resultJSON},
			func() error {
				if err := d.store.Transition(ctx, req); err != nil {
					return fmt.Errorf("transitioning issue %s: %w", issue.ID, err)
				}
				return nil
			})
	}

	// A normal (non-block) advance means the worker produced a real result, so
	// any transient-retry budget spent earlier is water under the bridge: reset
	// recover_attempts (and clear any backoff) so a later, independent
	// transient failure gets a full budget.
	req.ResetRecoverAttempts = true
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
		Comment:         reworkCapComment(d.cfg.ReworkCap, result),
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
		return changesRequestedComment(result.Summary)
	default:
		return ""
	}
}

// blockCommentFor renders the Linear comment body for a "blocked" outcome
// from the worker's own block_kind + summary (see comment.go's blockedComment
// for the markdown shape), so a human reviewing the Blocked column in Linear
// sees why without opening the store.
func blockCommentFor(w contract.WorkerResult) string {
	kind := ""
	if w.BlockKind != nil {
		kind = string(*w.BlockKind)
	}
	return blockedComment(kind, w.Summary)
}
