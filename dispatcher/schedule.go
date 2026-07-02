package dispatcher

import (
	"context"
	"errors"
	"fmt"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/store"
)

// selectAndClaim claims and spawns as much ready/downstream work as the
// configured caps allow, one pool at a time:
//
//   - the coder pool (ready -> running, plus rework -> running re-runs)
//     shares Caps.PerLane.Coder between both columns (R4 — see
//     claimCoderPool);
//   - review -> reviewer and documentation -> scribe are simple per-column
//     claims (decision O's cross-lane claiming — see claimSimpleColumn);
//   - merging runs internal/gitops INLINE rather than spawning a worker
//     (decision J amendment / O — see claimAndRunGitops).
//
// The global cap is re-checked before every single claim across all four
// pools, so a claim made for one pool immediately counts against the
// global cap for every pool checked afterward in the same pass.
func (d *Dispatcher) selectAndClaim(ctx context.Context) error {
	now := d.now()

	if err := d.claimCoderPool(ctx, now); err != nil {
		return err
	}
	if err := d.claimSimpleColumn(ctx, now, string(contract.ColumnReview), string(contract.LaneReviewer), d.cfg.Caps.PerLane.Reviewer); err != nil {
		return err
	}
	if err := d.claimSimpleColumn(ctx, now, string(contract.ColumnDocumentation), string(contract.LaneScribe), d.cfg.Caps.PerLane.Scribe); err != nil {
		return err
	}
	if err := d.claimAndRunGitops(ctx, now); err != nil {
		return err
	}
	return nil
}

// claimCoderPool claims up to the shared Caps.PerLane.Coder cap (and the
// global cap) from BOTH the ready column (fresh coder claims, via
// ClaimReady) and the rework column (coder re-runs after review/merging
// feedback, via ClaimColumn) as ONE pool ordered by (priority, created_at,
// identifier) (R4): before every single claim it peeks the current top
// candidate in each column (store.PeekReadyCandidate / PeekColumnCandidate)
// and claims whichever sorts first, so a continuous stream of ready work can
// never starve a card stuck in rework, or vice versa — unlike exhausting one
// column before ever trying the other.
//
// issues.lane_label / runs.lane store the BARE lane ("coder"), not the
// "agent:coder" Linear label — LaneLabelPrefix is only relevant when parsing
// Linear labels (internal/linear).
func (d *Dispatcher) claimCoderPool(ctx context.Context, now int64) error {
	const coderLane = string(contract.LaneCoder)
	coderCap := d.cfg.Caps.PerLane.Coder

	for {
		global, perLane := d.inflightLaneCounts()
		if global >= d.cfg.Caps.Global || perLane[coderLane] >= coderCap {
			return nil
		}

		fromRework, ok, err := d.pickCoderCandidate(ctx, coderLane, now)
		if err != nil {
			return fmt.Errorf("picking coder pool candidate: %w", err)
		}
		if !ok {
			return nil
		}

		claim, err := d.claimCoderCandidate(ctx, coderLane, fromRework, now)
		if errors.Is(err, store.ErrNoReady) {
			// The peeked candidate was claimed out from under us (only
			// possible if Tick's single-goroutine invariant is ever
			// violated) — re-peek rather than erroring the whole tick.
			continue
		}
		if err != nil {
			return fmt.Errorf("claiming coder pool candidate: %w", err)
		}

		// A rework re-run must carry the feedback that routed the card back
		// here (the reviewer's changes_requested, or a gitops stale-base
		// conflict) so the Coder lane can actually address it rather than
		// re-emitting the same diff. A fresh ready claim has no such feedback.
		reviewFeedback := ""
		if fromRework {
			reviewFeedback, err = d.store.LatestReworkFeedback(ctx, claim.Issue.ID)
			if err != nil {
				return fmt.Errorf("loading rework feedback for issue %s: %w", claim.Issue.ID, err)
			}
		} else {
			// Only the ready -> running move changes board_status, so
			// only it needs a Linear mirror (R5): ClaimReady's CAS is not
			// itself a Transition call, so nothing else enqueues this
			// outbox row. A rework claim (ClaimColumn) leaves board_status
			// untouched — Linear already shows "rework" from the
			// Transition that put the card there — so it enqueues
			// nothing.
			if err := d.store.EnqueueLinearSetState(ctx, claim.Issue.ID, "running", now); err != nil {
				return fmt.Errorf("enqueueing running mirror for issue %s: %w", claim.Issue.ID, err)
			}
		}

		if err := d.spawnClaim(ctx, *claim, reviewFeedback); err != nil {
			return fmt.Errorf("spawning claim for issue %s: %w", claim.Issue.ID, err)
		}
	}
}

// pickCoderCandidate peeks the current top ready and rework candidates for
// the coder pool and reports which column the next claim should come from:
// fromRework=true means the rework candidate sorts first (issueLess);
// ok=false means neither column currently has a candidate.
func (d *Dispatcher) pickCoderCandidate(ctx context.Context, coderLane string, now int64) (fromRework, ok bool, err error) {
	readyIssue, readyErr := d.store.PeekReadyCandidate(ctx, coderLane, now)
	if readyErr != nil && !errors.Is(readyErr, store.ErrNoReady) {
		return false, false, fmt.Errorf("peeking ready candidate: %w", readyErr)
	}
	reworkIssue, reworkErr := d.store.PeekColumnCandidate(ctx, string(contract.ColumnRework), now)
	if reworkErr != nil && !errors.Is(reworkErr, store.ErrNoReady) {
		return false, false, fmt.Errorf("peeking rework candidate: %w", reworkErr)
	}

	haveReady := readyErr == nil
	haveRework := reworkErr == nil
	switch {
	case !haveReady && !haveRework:
		return false, false, nil
	case haveReady && !haveRework:
		return false, true, nil
	case !haveReady && haveRework:
		return true, true, nil
	default:
		return issueLess(reworkIssue, readyIssue), true, nil
	}
}

// claimCoderCandidate performs the actual CAS claim for whichever column
// pickCoderCandidate chose.
func (d *Dispatcher) claimCoderCandidate(ctx context.Context, coderLane string, fromRework bool, now int64) (*store.Claim, error) {
	if fromRework {
		return d.store.ClaimColumn(ctx, string(contract.ColumnRework), coderLane, d.newRunID(), now, d.ttl())
	}
	return d.store.ClaimReady(ctx, coderLane, d.newRunID(), now, d.ttl())
}

// issueLess reports whether a sorts before b under selectClaimCandidate's
// ordering rule (priority, created_at, identifier) — see that function's
// doc comment in internal/store/claim.go. pickCoderCandidate uses this to
// decide which of the top ready candidate and the top rework candidate to
// actually claim next, mirroring the store's own tie-break rule exactly so
// the combined ready+rework pool behaves as if it were one ordered query.
func issueLess(a, b store.Issue) bool {
	pa, pb := priorityRank(a.Priority), priorityRank(b.Priority)
	if pa != pb {
		return pa < pb
	}
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt < b.CreatedAt
	}
	return a.Identifier < b.Identifier
}

// priorityRank maps Linear's priority encoding (0=none,1=urgent,...,4=low)
// onto selectClaimCandidate's own CASE expression (0 sorts LAST, as if it
// were the highest possible number), so issueLess agrees with the store's
// SQL ordering exactly.
func priorityRank(p int) int {
	if p == 0 {
		return 999999999
	}
	return p
}

// claimSimpleColumn claims up to capN (and the global cap) UNCLAIMED cards
// currently sitting in column, spawning lane's worker for each. Unlike the
// coder pool, review and documentation each feed exactly one lane from
// exactly one column, so no cross-column fairness is needed —
// store.ClaimColumn's own (priority, created_at, identifier) ordering
// already picks the single best candidate each time.
//
// R5: a downstream column claim never changes board_status (the card stays
// in column while the claimed lane runs), so there is nothing to mirror to
// Linear for the claim itself — only a later Transition (once the lane's
// result comes back) enqueues a mirror.
func (d *Dispatcher) claimSimpleColumn(ctx context.Context, now int64, column, lane string, capN int) error {
	for {
		global, perLane := d.inflightLaneCounts()
		if global >= d.cfg.Caps.Global || perLane[lane] >= capN {
			return nil
		}

		claim, err := d.store.ClaimColumn(ctx, column, lane, d.newRunID(), now, d.ttl())
		if errors.Is(err, store.ErrNoReady) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claiming column %s: %w", column, err)
		}

		// Reviewer/scribe claims carry no rework feedback — that is a
		// Coder-lane-only concern (see claimCoderPool).
		if err := d.spawnClaim(ctx, *claim, ""); err != nil {
			return fmt.Errorf("spawning claim for issue %s: %w", claim.Issue.ID, err)
		}
	}
}

// spawnClaim starts the worker process for a freshly won claim. claim.Run.
// Lane already carries the lane the claim dispatched (the bare lane
// ClaimReady/ClaimColumn recorded — "coder"/"reviewer"/"scribe"), so this
// needs no per-lane branching: spawnAttempt/WorkerSpec build the right
// `--lane` flag for whichever lane the claim was for (R5). reviewFeedback is
// the rework feedback for a Coder re-run out of the rework column (empty for
// every other claim); see spawnAttempt. A Spawn failure
// (e.g. workspace setup or exec failure, as opposed to a worker process
// failure) is treated the same as an in-run failure: the issue is blocked
// immediately, since there is no process to wait on.
func (d *Dispatcher) spawnClaim(ctx context.Context, claim store.Claim, reviewFeedback string) error {
	return d.spawnAttempt(ctx, claim.Issue, claim.Run.RunID, claim.Run.Lane, "", 1, reviewFeedback)
}
