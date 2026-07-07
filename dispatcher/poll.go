package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/store"
)

// pollAndUpsert fetches candidate issues from Linear and caches them in the
// store via UpsertIssue. UpsertIssue's own conflict semantics (preserve
// board_status/claim on conflict) are what make a re-poll safe against an
// in-flight claim — this method just needs to map fields, not worry about
// clobbering dispatcher-owned state.
//
// Issues with no lane assigned (Lane == "") are cached (so their identifier/
// deps/priority stay current for when a lane label is eventually added) but
// are otherwise inert: selectAndClaim only ever claims from a specific lane
// label, so an empty lane_label issue is never a candidate to claim.
//
// After each upsert, reconcileLinearDivergence (A3) checks the freshly
// polled Linear status against the resulting SQLite row and either adopts a
// human-driven Linear move or re-asserts the dispatcher's own state,
// depending on whether the issue currently holds an active claim.
func (d *Dispatcher) pollAndUpsert(ctx context.Context) error {
	issues, err := d.linear.CandidateIssues(ctx)
	if err != nil {
		return fmt.Errorf("polling candidate issues: %w", err)
	}

	now := d.now()
	for _, li := range issues {
		deps, err := json.Marshal(li.Deps)
		if err != nil {
			return fmt.Errorf("encoding deps for issue %s: %w", li.Identifier, err)
		}

		row := store.Issue{
			ID:          li.ID,
			Identifier:  li.Identifier,
			Title:       li.Title,
			Description: li.Description,
			LaneLabel:   li.Lane,
			BoardStatus: li.Status,
			Deps:        string(deps),
			Priority:    li.Priority,
			BranchName:  li.BranchName,
			UpdatedAt:   li.UpdatedAt,
			LastSeen:    now,
			CreatedAt:   now,
		}
		if err := d.store.UpsertIssue(ctx, row); err != nil {
			return fmt.Errorf("caching issue %s: %w", li.Identifier, err)
		}

		if err := d.reconcileLinearDivergence(ctx, li.ID, li.Status, now); err != nil {
			return fmt.Errorf("reconciling linear divergence for issue %s: %w", li.Identifier, err)
		}
	}
	return nil
}

// reconcileLinearDivergence implements A3: after caching a freshly polled
// Linear status, compare it against the SQLite row's current board_status
// (which UpsertIssue's conflict semantics may have left unchanged, e.g. a
// running claim). The two states can differ for two very different
// reasons, handled oppositely:
//
//   - No active claim: nobody but a human could have moved this issue in
//     Linear (the dispatcher only changes board_status via ClaimReady/
//     Transition, both of which also enqueue their own outbox mirror). This
//     is a human move — adopt it into SQLite. No outbox mirror: Linear
//     already holds this state, so mirroring it back would be a no-op write
//     at best.
//   - An active claim: the dispatcher currently owns this issue, and
//     linearStatus reflects a state Linear held before the claim's own
//     mirror write was applied/observed. Re-assert the dispatcher's truth by
//     enqueueing a fresh setstate mirror, without touching board_status.
//
// Equal states need no action either way.
func (d *Dispatcher) reconcileLinearDivergence(ctx context.Context, issueID, linearStatus string, now int64) error {
	issue, err := d.store.GetIssue(ctx, issueID)
	if err != nil {
		return fmt.Errorf("loading issue %s: %w", issueID, err)
	}

	if issue.BoardStatus == linearStatus {
		return nil
	}

	// board_status="running" is entered ONLY via the CAS claim
	// (store.ClaimReady/ClaimColumn) -- see AGENTS.md's kernel invariant.
	// Reaching here at all already means issue.BoardStatus != linearStatus,
	// so an observed linearStatus=="running" can NEVER be backed by a real
	// claim on THIS issue's own row: either a human dragged the card to
	// Running by hand, or Linear's own mirror of a prior (now-released)
	// claim hasn't caught up yet (a restart-requeue race). Adopting it would
	// write board_status='running' with claim_lock left NULL -- a row
	// ClaimReady's CAS can never claim again (it requires board_status=
	// 'ready') and ReleaseStaleClaims can never release (it only looks at
	// claim_expires, permanently NULL here). Treat it exactly like the
	// dispatcher-owns-this-claim case below: re-assert the store's real
	// status instead of trusting Linear's.
	if !issue.ClaimLock.Valid && linearStatus != string(contract.ColumnRunning) {
		return d.adoptLinearMove(ctx, issueID, issue.BoardStatus, linearStatus, now)
	}
	return d.reassertOwnedState(ctx, issueID, issue.BoardStatus, now)
}

// adoptLinearMove folds a human's out-of-band Linear move into SQLite: a
// plain status update, no claim change, no run to close, and (deliberately)
// no outbox mirror, since Linear is already the source of this state.
//
// A blocked->{ready,todo} move additionally resets issues.rework_count
// (TransitionReq.ResetReworkCount) and issues.recover_attempts (plus its
// paired blocked_until backoff deadline, via TransitionReq.
// ResetRecoverAttempts): once a human pulls an issue back out of Blocked,
// whatever rework_count/recover_attempts it accumulated on its prior review/
// rework or auto-retry cycle no longer bounds anything relevant. Without
// this, an issue blocked after tripping amendment C1's rework_cap (or after
// auto-unblock layer 1 exhausted RecoverCap) would keep that stale count
// forever, and a human's very next requeue could immediately re-trip
// blockIfReworkCapExceeded or park again on the very first subsequent
// transient failure — defeating the point of requeuing it by hand. Scoped
// specifically to a requeue FROM blocked (priorStatus) so an ordinary human
// move that never touched Blocked at all doesn't reset counts it has no
// bearing on.
func (d *Dispatcher) adoptLinearMove(ctx context.Context, issueID, priorStatus, linearStatus string, now int64) error {
	humanRequeueFromBlocked := priorStatus == string(contract.ColumnBlocked) && isHumanRequeueTarget(linearStatus)
	req := store.TransitionReq{
		IssueID:              issueID,
		NewStatus:            linearStatus,
		ResetReworkCount:     humanRequeueFromBlocked,
		ResetRecoverAttempts: humanRequeueFromBlocked,
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issueID),
			Kind:    "adopted",
			Detail:  fmt.Sprintf("adopted human move in linear: board_status -> %s", linearStatus),
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("adopting linear move for issue %s: %w", issueID, err)
	}
	return nil
}

// isHumanRequeueTarget reports whether linearStatus is one of the columns a
// human-driven Blocked requeue can land on: back into the active pipeline,
// either immediately claimable (ready) or pending its dependencies (todo).
func isHumanRequeueTarget(linearStatus string) bool {
	return linearStatus == string(contract.ColumnReady) || linearStatus == string(contract.ColumnTodo)
}

// reassertOwnedState pushes the dispatcher's current board_status back to
// Linear via the outbox, without changing SQLite: the dispatcher holds an
// active claim on this issue, so its own state is the truth, and
// drainOutbox will correct Linear's stale view on this tick's later phase.
func (d *Dispatcher) reassertOwnedState(ctx context.Context, issueID, boardStatus string, now int64) error {
	if err := d.store.EnqueueLinearSetState(ctx, issueID, boardStatus, now); err != nil {
		return fmt.Errorf("reasserting owned state for issue %s: %w", issueID, err)
	}
	return nil
}
