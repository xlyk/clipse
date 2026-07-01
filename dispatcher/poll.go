package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"

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

	if !issue.ClaimLock.Valid {
		return d.adoptLinearMove(ctx, issueID, linearStatus, now)
	}
	return d.reassertOwnedState(ctx, issueID, issue.BoardStatus, now)
}

// adoptLinearMove folds a human's out-of-band Linear move into SQLite: a
// plain status update, no claim change, no run to close, and (deliberately)
// no outbox mirror, since Linear is already the source of this state.
func (d *Dispatcher) adoptLinearMove(ctx context.Context, issueID, linearStatus string, now int64) error {
	req := store.TransitionReq{
		IssueID:   issueID,
		NewStatus: linearStatus,
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
