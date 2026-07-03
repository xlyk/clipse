package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xlyk/clipse/internal/store"
)

// seedReadyIssue inserts a single 'ready' issue in laneLabel, ready to be
// claimed.
func seedReadyIssue(t *testing.T, s *store.Store, id, laneLabel string, priority int, createdAt int64) {
	t.Helper()
	ctx := context.Background()
	issue := store.Issue{
		ID:          id,
		Identifier:  id,
		LaneLabel:   laneLabel,
		BoardStatus: "ready",
		Deps:        `[]`,
		Priority:    priority,
		BranchName:  id + "-branch",
		UpdatedAt:   createdAt,
		LastSeen:    createdAt,
		CreatedAt:   createdAt,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue(%s): unexpected error: %v", id, err)
	}
}

func TestClaimReady_ExactlyOneWinnerUnderConcurrency(t *testing.T) {
	s := openTestStore(t)
	const lane = "agent:coder"
	seedReadyIssue(t, s, "issue-1", lane, 1, 100)

	const n = 50
	var (
		wg      sync.WaitGroup
		wins    int64
		noReady int64
		other   int64
	)
	winners := make([]*store.Claim, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := fmt.Sprintf("run-%d", i)
			claim, err := s.ClaimReady(context.Background(), lane, runID, 1000, 60)
			switch {
			case err == nil:
				atomic.AddInt64(&wins, 1)
				winners[i] = claim
			case errors.Is(err, store.ErrNoReady):
				atomic.AddInt64(&noReady, 1)
			default:
				atomic.AddInt64(&other, 1)
				t.Errorf("goroutine %d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if other != 0 {
		t.Fatalf("unexpected-error count = %d, want 0", other)
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}
	if noReady != n-1 {
		t.Fatalf("noReady = %d, want %d", noReady, n-1)
	}

	var winnerRunID string
	for _, w := range winners {
		if w != nil {
			winnerRunID = w.Run.RunID
			if w.Issue.ID != "issue-1" {
				t.Errorf("winning claim issue = %q, want %q", w.Issue.ID, "issue-1")
			}
			if w.Issue.BoardStatus != "running" {
				t.Errorf("winning claim board_status = %q, want %q", w.Issue.BoardStatus, "running")
			}
			if !w.Issue.ClaimLock.Valid || w.Issue.ClaimLock.String != w.Run.RunID {
				t.Errorf("winning claim ClaimLock = %+v, want valid %q", w.Issue.ClaimLock, w.Run.RunID)
			}
		}
	}

	// Exactly one runs row must exist, and the issue must be running with
	// claim_lock set to the winner's run_id.
	ctx := context.Background()
	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if len(snap.Issues) != 1 {
		t.Fatalf("len(snap.Issues) = %d, want 1", len(snap.Issues))
	}
	got := snap.Issues[0]
	if got.BoardStatus != "running" {
		t.Errorf("issue BoardStatus = %q, want %q", got.BoardStatus, "running")
	}
	if !got.ClaimLock.Valid || got.ClaimLock.String != winnerRunID {
		t.Errorf("issue ClaimLock = %+v, want valid %q", got.ClaimLock, winnerRunID)
	}
	if got.LatestRun == nil {
		t.Fatalf("issue LatestRun = nil, want the winning run")
	}
	if got.LatestRun.RunID != winnerRunID {
		t.Errorf("LatestRun.RunID = %q, want %q", got.LatestRun.RunID, winnerRunID)
	}

	var runCount int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runCount); err != nil {
		t.Fatalf("counting runs: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("runs row count = %d, want 1", runCount)
	}
}

// TestClaimReady_IncludesTitleAndDescription asserts the Issue ClaimReady
// returns (Claim.Issue) carries title/description: this is the exact value
// the dispatcher's spawnAttempt feeds into CLIPSE_ISSUE_TEXT, so a claim
// that dropped these fields would silently hand the worker an empty prompt.
func TestClaimReady_IncludesTitleAndDescription(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const lane = "agent:coder"

	issue := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		Title:       "Add the thing",
		Description: "Implement the thing that does the stuff.",
		LaneLabel:   lane,
		BoardStatus: "ready",
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "clp-1-branch",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	claim, err := s.ClaimReady(ctx, lane, "run-1", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}
	if claim.Issue.Title != "Add the thing" {
		t.Errorf("claim.Issue.Title = %q, want %q", claim.Issue.Title, "Add the thing")
	}
	if claim.Issue.Description != "Implement the thing that does the stuff." {
		t.Errorf("claim.Issue.Description = %q, want %q", claim.Issue.Description, "Implement the thing that does the stuff.")
	}
}

func TestClaimReady_NoReadyIssues(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.ClaimReady(ctx, "agent:coder", "run-1", 1000, 60)
	if !errors.Is(err, store.ErrNoReady) {
		t.Fatalf("ClaimReady on empty store: err = %v, want ErrNoReady", err)
	}
}

func TestClaimReady_OrdersByPriorityThenCreatedThenIdentifier(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const lane = "agent:coder"

	// priority: Linear semantics are 0=none,1=urgent,2=high,3=medium,4=low.
	// Most urgent actionable first; 0=none is lowest (claimed last).
	seedReadyIssue(t, s, "issue-none", lane, 0, 50)
	seedReadyIssue(t, s, "issue-low", lane, 4, 50)
	seedReadyIssue(t, s, "issue-urgent-later", lane, 1, 200)
	seedReadyIssue(t, s, "issue-urgent-earlier", lane, 1, 100)

	claim, err := s.ClaimReady(ctx, lane, "run-1", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}
	if claim.Issue.ID != "issue-urgent-earlier" {
		t.Errorf("claimed issue = %q, want %q (most urgent, earliest created)", claim.Issue.ID, "issue-urgent-earlier")
	}
}

func TestClaimReady_IgnoresOtherLanes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedReadyIssue(t, s, "issue-1", "agent:reviewer", 1, 100)

	_, err := s.ClaimReady(ctx, "agent:coder", "run-1", 1000, 60)
	if !errors.Is(err, store.ErrNoReady) {
		t.Fatalf("ClaimReady wrong lane: err = %v, want ErrNoReady", err)
	}
}

func TestClaimReady_AppendsClaimedEvent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const lane = "agent:coder"
	seedReadyIssue(t, s, "issue-1", lane, 1, 100)

	if _, err := s.ClaimReady(ctx, lane, "run-1", 1000, 60); err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}

	events, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: unexpected error: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "claimed" && e.IssueID.Valid && e.IssueID.String == "issue-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'claimed' event found for issue-1; events = %+v", events)
	}
}

func TestHeartbeat_ExtendsClaimExpiresAndHeartbeatAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const lane = "agent:coder"
	seedReadyIssue(t, s, "issue-1", lane, 1, 100)

	claim, err := s.ClaimReady(ctx, lane, "run-1", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}

	if err := s.Heartbeat(ctx, claim.Run.RunID, 1100, 60); err != nil {
		t.Fatalf("Heartbeat: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	got := snap.Issues[0]
	if !got.ClaimExpires.Valid || got.ClaimExpires.Int64 != 1160 {
		t.Errorf("ClaimExpires = %+v, want valid 1160", got.ClaimExpires)
	}
	if got.LatestRun == nil {
		t.Fatalf("LatestRun = nil")
	}
	if got.LatestRun.HeartbeatAt != 1100 {
		t.Errorf("LatestRun.HeartbeatAt = %d, want 1100", got.LatestRun.HeartbeatAt)
	}
}

func TestHeartbeat_ErrorsWhenNoSuchActiveClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Heartbeat(ctx, "no-such-run", 1000, 60); err == nil {
		t.Fatalf("Heartbeat on nonexistent run: err = nil, want error")
	}
}

func TestReleaseStaleClaims_RequeuesExpiredLeavesFreshUntouched(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const lane = "agent:coder"

	seedReadyIssue(t, s, "issue-stale", lane, 1, 100)
	seedReadyIssue(t, s, "issue-fresh", lane, 1, 200)

	staleClaim, err := s.ClaimReady(ctx, lane, "run-stale", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimReady(stale): unexpected error: %v", err)
	}
	freshClaim, err := s.ClaimReady(ctx, lane, "run-fresh", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimReady(fresh): unexpected error: %v", err)
	}

	// Manually push the stale claim's expiry into the past (simulating a
	// heartbeat that never arrived) while leaving the fresh one as-is
	// (expires in the future relative to the `now` we release at).
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE issues SET claim_expires = ? WHERE id = ?`, 900, "issue-stale"); err != nil {
		t.Fatalf("forcing stale expiry: %v", err)
	}

	n, err := s.ReleaseStaleClaims(ctx, 1000)
	if err != nil {
		t.Fatalf("ReleaseStaleClaims: unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReleaseStaleClaims released = %d, want 1", n)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	byID := map[string]store.IssueSnapshot{}
	for _, is := range snap.Issues {
		byID[is.ID] = is
	}

	stale := byID["issue-stale"]
	if stale.BoardStatus != "ready" {
		t.Errorf("stale issue BoardStatus = %q, want %q", stale.BoardStatus, "ready")
	}
	if stale.ClaimLock.Valid {
		t.Errorf("stale issue ClaimLock.Valid = true, want false")
	}
	if stale.ClaimExpires.Valid {
		t.Errorf("stale issue ClaimExpires.Valid = true, want false")
	}

	fresh := byID["issue-fresh"]
	if fresh.BoardStatus != "running" {
		t.Errorf("fresh issue BoardStatus = %q, want preserved %q", fresh.BoardStatus, "running")
	}
	if !fresh.ClaimLock.Valid || fresh.ClaimLock.String != freshClaim.Run.RunID {
		t.Errorf("fresh issue ClaimLock = %+v, want preserved %q", fresh.ClaimLock, freshClaim.Run.RunID)
	}

	var staleRunStatus string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT status FROM runs WHERE run_id = ?`, staleClaim.Run.RunID).Scan(&staleRunStatus); err != nil {
		t.Fatalf("reading stale run status: %v", err)
	}
	if staleRunStatus != "stale" {
		t.Errorf("stale run status = %q, want %q", staleRunStatus, "stale")
	}

	var freshRunStatus string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT status FROM runs WHERE run_id = ?`, freshClaim.Run.RunID).Scan(&freshRunStatus); err != nil {
		t.Fatalf("reading fresh run status: %v", err)
	}
	if freshRunStatus != "running" {
		t.Errorf("fresh run status = %q, want preserved %q", freshRunStatus, "running")
	}

	events, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: unexpected error: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "stale_release" && e.IssueID.Valid && e.IssueID.String == "issue-stale" {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'stale_release' event found for issue-stale; events = %+v", events)
	}
}

// TestClaimReady_AttemptIncrementsAcrossReleaseAndReclaim asserts that a
// re-claim of the same issue (after a release back to 'ready', e.g. a stale
// claim release or an explicit requeue) gets attempt = previous max + 1,
// rather than resetting to 1. This is what lets the dispatcher's max_attempts
// cap (A1) count real dispatch attempts across restarts/retries.
func TestClaimReady_AttemptIncrementsAcrossReleaseAndReclaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const lane = "agent:coder"
	seedReadyIssue(t, s, "issue-1", lane, 1, 100)

	first, err := s.ClaimReady(ctx, lane, "run-1", 1000, 60)
	if err != nil {
		t.Fatalf("first ClaimReady: unexpected error: %v", err)
	}
	if first.Run.Attempt != 1 {
		t.Fatalf("first claim Attempt = %d, want 1", first.Run.Attempt)
	}

	// Force the claim to expire, then release it back to 'ready' (as
	// ReleaseStaleClaims would after a lost heartbeat).
	if _, err := s.DB().ExecContext(ctx, `UPDATE issues SET claim_expires = ? WHERE id = ?`, 900, "issue-1"); err != nil {
		t.Fatalf("forcing stale expiry: %v", err)
	}
	if n, err := s.ReleaseStaleClaims(ctx, 1000); err != nil || n != 1 {
		t.Fatalf("ReleaseStaleClaims: n=%d err=%v, want n=1 err=nil", n, err)
	}

	second, err := s.ClaimReady(ctx, lane, "run-2", 2000, 60)
	if err != nil {
		t.Fatalf("second ClaimReady: unexpected error: %v", err)
	}
	if second.Run.Attempt != 2 {
		t.Errorf("second claim Attempt = %d, want 2", second.Run.Attempt)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].LatestRun == nil || snap.Issues[0].LatestRun.Attempt != 2 {
		t.Errorf("LatestRun.Attempt = %+v, want 2", snap.Issues[0].LatestRun)
	}
}

// seedColumnIssue inserts a single issue already sitting in column (e.g.
// "review"), unclaimed, ready to be claimed by ClaimColumn -- the downstream
// analogue of seedReadyIssue. LaneLabel is always the bare "coder": per the
// kernel invariant, an issue's own lane_label never changes as it moves
// through downstream columns -- ClaimColumn dispatches whichever lane the
// COLUMN implies, not the issue's label.
func seedColumnIssue(t *testing.T, s *store.Store, id, column string, priority int, createdAt int64) {
	t.Helper()
	ctx := context.Background()
	issue := store.Issue{
		ID:          id,
		Identifier:  id,
		LaneLabel:   "coder",
		BoardStatus: column,
		Deps:        `[]`,
		Priority:    priority,
		BranchName:  id + "-branch",
		UpdatedAt:   createdAt,
		LastSeen:    createdAt,
		CreatedAt:   createdAt,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue(%s): unexpected error: %v", id, err)
	}
}

// TestClaimColumn_ExactlyOneWinnerUnderConcurrency_BoardStatusUnchanged
// mirrors TestClaimReady_ExactlyOneWinnerUnderConcurrency for the downstream
// per-column claim path: exactly one concurrent caller wins the CAS, and
// critically -- unlike ClaimReady -- the winning issue's board_status stays
// exactly "review" throughout: ClaimColumn only ever sets claim_lock/
// claim_expires, never board_status.
func TestClaimColumn_ExactlyOneWinnerUnderConcurrency_BoardStatusUnchanged(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "review", 1, 100)

	const n = 50
	var (
		wg      sync.WaitGroup
		wins    int64
		noReady int64
		other   int64
	)
	winners := make([]*store.Claim, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := fmt.Sprintf("run-%d", i)
			claim, err := s.ClaimColumn(context.Background(), "review", "reviewer", runID, 1000, 60)
			switch {
			case err == nil:
				atomic.AddInt64(&wins, 1)
				winners[i] = claim
			case errors.Is(err, store.ErrNoReady):
				atomic.AddInt64(&noReady, 1)
			default:
				atomic.AddInt64(&other, 1)
				t.Errorf("goroutine %d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if other != 0 {
		t.Fatalf("unexpected-error count = %d, want 0", other)
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}
	if noReady != n-1 {
		t.Fatalf("noReady = %d, want %d", noReady, n-1)
	}

	var winnerRunID string
	for _, w := range winners {
		if w == nil {
			continue
		}
		winnerRunID = w.Run.RunID
		if w.Issue.ID != "issue-1" {
			t.Errorf("winning claim issue = %q, want %q", w.Issue.ID, "issue-1")
		}
		if w.Issue.BoardStatus != "review" {
			t.Errorf("winning claim board_status = %q, want unchanged %q", w.Issue.BoardStatus, "review")
		}
		if w.Issue.LaneLabel != "coder" {
			t.Errorf("winning claim issue LaneLabel = %q, want unchanged %q", w.Issue.LaneLabel, "coder")
		}
		if w.Run.Lane != "reviewer" {
			t.Errorf("winning claim run Lane = %q, want dispatched lane %q", w.Run.Lane, "reviewer")
		}
		if !w.Issue.ClaimLock.Valid || w.Issue.ClaimLock.String != w.Run.RunID {
			t.Errorf("winning claim ClaimLock = %+v, want valid %q", w.Issue.ClaimLock, w.Run.RunID)
		}
	}

	ctx := context.Background()
	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if len(snap.Issues) != 1 {
		t.Fatalf("len(snap.Issues) = %d, want 1", len(snap.Issues))
	}
	got := snap.Issues[0]
	if got.BoardStatus != "review" {
		t.Errorf("issue BoardStatus = %q, want unchanged %q", got.BoardStatus, "review")
	}
	if !got.ClaimLock.Valid || got.ClaimLock.String != winnerRunID {
		t.Errorf("issue ClaimLock = %+v, want valid %q", got.ClaimLock, winnerRunID)
	}

	var runCount int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runCount); err != nil {
		t.Fatalf("counting runs: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("runs row count = %d, want 1", runCount)
	}
}

func TestClaimColumn_IgnoresOtherColumns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedColumnIssue(t, s, "issue-1", "ready", 1, 100)

	_, err := s.ClaimColumn(ctx, "review", "reviewer", "run-1", 1000, 60)
	if !errors.Is(err, store.ErrNoReady) {
		t.Fatalf("ClaimColumn wrong column: err = %v, want ErrNoReady", err)
	}
}

func TestClaimColumn_IgnoresAlreadyClaimedCard(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	if _, err := s.ClaimColumn(ctx, "merging", "git_operator", "run-1", 1000, 60); err != nil {
		t.Fatalf("first ClaimColumn: unexpected error: %v", err)
	}

	_, err := s.ClaimColumn(ctx, "merging", "git_operator", "run-2", 1000, 60)
	if !errors.Is(err, store.ErrNoReady) {
		t.Fatalf("second ClaimColumn on already-claimed card: err = %v, want ErrNoReady", err)
	}
}

func TestClaimColumn_AppendsClaimedEvent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedColumnIssue(t, s, "issue-1", "review", 1, 100)

	if _, err := s.ClaimColumn(ctx, "review", "reviewer", "run-1", 1000, 60); err != nil {
		t.Fatalf("ClaimColumn: unexpected error: %v", err)
	}

	events, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: unexpected error: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "claimed" && e.IssueID.Valid && e.IssueID.String == "issue-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'claimed' event found for issue-1; events = %+v", events)
	}
}

// TestClaimColumn_OrdersByPriorityThenCreatedThenIdentifier mirrors
// TestClaimReady_OrdersByPriorityThenCreatedThenIdentifier: ClaimColumn must
// select candidates the same deterministic way ClaimReady does.
func TestClaimColumn_OrdersByPriorityThenCreatedThenIdentifier(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const column = "review"

	seedColumnIssue(t, s, "issue-none", column, 0, 50)
	seedColumnIssue(t, s, "issue-low", column, 4, 50)
	seedColumnIssue(t, s, "issue-urgent-later", column, 1, 200)
	seedColumnIssue(t, s, "issue-urgent-earlier", column, 1, 100)

	claim, err := s.ClaimColumn(ctx, column, "reviewer", "run-1", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimColumn: unexpected error: %v", err)
	}
	if claim.Issue.ID != "issue-urgent-earlier" {
		t.Errorf("claimed issue = %q, want %q (most urgent, earliest created)", claim.Issue.ID, "issue-urgent-earlier")
	}
}

// TestClaimColumn_AttemptIsIssueGlobalAcrossLanes asserts ClaimColumn
// computes the next attempt as prior-max+1 scoped to the ISSUE, not the
// lane (R5): a coder's original run and a later reviewer claim on the same
// issue share one attempt sequence, so cfg.MaxAttempts counts real dispatch
// attempts across every lane.
func TestClaimColumn_AttemptIsIssueGlobalAcrossLanes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	firstClaim, err := s.ClaimReady(ctx, "coder", "run-coder-1", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}
	if firstClaim.Run.Attempt != 1 {
		t.Fatalf("coder claim Attempt = %d, want 1", firstClaim.Run.Attempt)
	}

	// Simulate what Transition would do on a needs_review outcome (open the
	// Review card, clearing the coder's claim) without pulling in the whole
	// board.Next machinery for this store-level test.
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE issues SET board_status = 'review', claim_lock = NULL, claim_expires = NULL WHERE id = ?`, "issue-1"); err != nil {
		t.Fatalf("forcing review column: %v", err)
	}

	reviewClaim, err := s.ClaimColumn(ctx, "review", "reviewer", "run-reviewer-1", 2000, 60)
	if err != nil {
		t.Fatalf("ClaimColumn: unexpected error: %v", err)
	}
	if reviewClaim.Run.Attempt != 2 {
		t.Errorf("reviewer claim Attempt = %d, want 2 (issue-global)", reviewClaim.Run.Attempt)
	}
	if reviewClaim.Run.Lane != "reviewer" {
		t.Errorf("reviewer claim Lane = %q, want %q", reviewClaim.Run.Lane, "reviewer")
	}
}

// TestReleaseTargetColumn is a table-driven test of the pure rule shared by
// ReleaseStaleClaims (this package) and the dispatcher's orphan recovery
// (dispatcher.requeueOrphan, which must call this same function so the two
// release paths cannot drift): a card claimed while running (the Coder
// lane's sole working column) releases to ready; a card claimed in any
// downstream lane-entry column keeps that column.
func TestReleaseTargetColumn(t *testing.T) {
	tests := []struct {
		current string
		want    string
	}{
		{current: "running", want: "ready"},
		{current: "review", want: "review"},
		{current: "rework", want: "rework"},
		{current: "merging", want: "merging"},
		{current: "ready", want: "ready"},
	}
	for _, tc := range tests {
		t.Run(tc.current, func(t *testing.T) {
			if got := store.ReleaseTargetColumn(tc.current); got != tc.want {
				t.Errorf("ReleaseTargetColumn(%q) = %q, want %q", tc.current, got, tc.want)
			}
		})
	}
}

// TestReleaseStaleClaims_DownstreamClaimStaysInItsColumn asserts a stale
// claim on a card sitting in a downstream lane-entry column (claimed via
// ClaimColumn, e.g. review) releases the claim but leaves board_status
// exactly as it was -- only a stale claim on 'running' (the Coder lane's
// sole working column) returns to 'ready'; regression coverage for that case
// lives in TestReleaseStaleClaims_RequeuesExpiredLeavesFreshUntouched.
func TestReleaseStaleClaims_DownstreamClaimStaysInItsColumn(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedColumnIssue(t, s, "issue-1", "review", 1, 100)

	claim, err := s.ClaimColumn(ctx, "review", "reviewer", "run-1", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimColumn: unexpected error: %v", err)
	}

	if _, err := s.DB().ExecContext(ctx, `UPDATE issues SET claim_expires = ? WHERE id = ?`, 900, "issue-1"); err != nil {
		t.Fatalf("forcing stale expiry: %v", err)
	}

	n, err := s.ReleaseStaleClaims(ctx, 1000)
	if err != nil {
		t.Fatalf("ReleaseStaleClaims: unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReleaseStaleClaims released = %d, want 1", n)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "review" {
		t.Errorf("BoardStatus = %q, want unchanged %q", got.BoardStatus, "review")
	}
	if got.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want false")
	}
	if got.ClaimExpires.Valid {
		t.Errorf("ClaimExpires.Valid = true, want false")
	}

	var runStatus string
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id = ?`, claim.Run.RunID).Scan(&runStatus); err != nil {
		t.Fatalf("reading run status: %v", err)
	}
	if runStatus != "stale" {
		t.Errorf("run status = %q, want stale", runStatus)
	}

	events, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: unexpected error: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "stale_release" && e.IssueID.Valid && e.IssueID.String == "issue-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'stale_release' event found for issue-1; events = %+v", events)
	}
}

// TestReleaseStaleClaims_MixedRunningAndDownstreamInSameCall asserts a
// single ReleaseStaleClaims call resolves each stale card independently: a
// stale 'running' claim returns to ready, while a stale 'merging' claim
// released in the same call keeps its column.
func TestReleaseStaleClaims_MixedRunningAndDownstreamInSameCall(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	seedReadyIssue(t, s, "issue-running", "coder", 1, 100)
	if _, err := s.ClaimReady(ctx, "coder", "run-running", 1000, 60); err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}

	seedColumnIssue(t, s, "issue-merging", "merging", 1, 100)
	if _, err := s.ClaimColumn(ctx, "merging", "git_operator", "run-merging", 1000, 60); err != nil {
		t.Fatalf("ClaimColumn: unexpected error: %v", err)
	}

	if _, err := s.DB().ExecContext(ctx,
		`UPDATE issues SET claim_expires = 900 WHERE id IN ('issue-running', 'issue-merging')`); err != nil {
		t.Fatalf("forcing stale expiry: %v", err)
	}

	n, err := s.ReleaseStaleClaims(ctx, 1000)
	if err != nil {
		t.Fatalf("ReleaseStaleClaims: unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("ReleaseStaleClaims released = %d, want 2", n)
	}

	runningIssue, err := s.GetIssue(ctx, "issue-running")
	if err != nil {
		t.Fatalf("GetIssue(issue-running): unexpected error: %v", err)
	}
	if runningIssue.BoardStatus != "ready" {
		t.Errorf("issue-running BoardStatus = %q, want %q", runningIssue.BoardStatus, "ready")
	}
	if runningIssue.ClaimLock.Valid {
		t.Errorf("issue-running ClaimLock.Valid = true, want false")
	}

	mergingIssue, err := s.GetIssue(ctx, "issue-merging")
	if err != nil {
		t.Fatalf("GetIssue(issue-merging): unexpected error: %v", err)
	}
	if mergingIssue.BoardStatus != "merging" {
		t.Errorf("issue-merging BoardStatus = %q, want unchanged %q", mergingIssue.BoardStatus, "merging")
	}
	if mergingIssue.ClaimLock.Valid {
		t.Errorf("issue-merging ClaimLock.Valid = true, want false")
	}
}

// TestPeekReadyCandidate_ReturnsWithoutClaiming asserts PeekReadyCandidate
// reports the same candidate ClaimReady would pick, but leaves it fully
// unclaimed (no claim_lock, no runs row) -- the dispatcher's coder-pool
// fairness rule (R4) depends on being able to compare the top ready
// candidate against the top rework candidate without committing to either.
func TestPeekReadyCandidate_ReturnsWithoutClaiming(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const lane = "coder"

	seedReadyIssue(t, s, "issue-low", lane, 4, 50)
	seedReadyIssue(t, s, "issue-urgent", lane, 1, 100)

	issue, err := s.PeekReadyCandidate(ctx, lane, 1000)
	if err != nil {
		t.Fatalf("PeekReadyCandidate: unexpected error: %v", err)
	}
	if issue.ID != "issue-urgent" {
		t.Errorf("PeekReadyCandidate = %q, want %q (most urgent)", issue.ID, "issue-urgent")
	}

	// Peeking must not have claimed anything: both issues are still
	// unclaimed and 'ready', and no runs row exists.
	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	for _, is := range snap.Issues {
		if is.BoardStatus != "ready" {
			t.Errorf("issue %s BoardStatus = %q, want unchanged ready (peek must not claim)", is.ID, is.BoardStatus)
		}
		if is.ClaimLock.Valid {
			t.Errorf("issue %s ClaimLock.Valid = true, want false (peek must not claim)", is.ID)
		}
	}
	var runCount int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runCount); err != nil {
		t.Fatalf("counting runs: %v", err)
	}
	if runCount != 0 {
		t.Fatalf("runs row count = %d, want 0 (peek must not insert a run)", runCount)
	}

	// A real claim afterward still succeeds and picks the same candidate.
	claim, err := s.ClaimReady(ctx, lane, "run-1", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimReady after peek: unexpected error: %v", err)
	}
	if claim.Issue.ID != "issue-urgent" {
		t.Errorf("ClaimReady after peek claimed %q, want %q", claim.Issue.ID, "issue-urgent")
	}
}

func TestPeekReadyCandidate_NoneReturnsErrNoReady(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.PeekReadyCandidate(ctx, "coder", 1000)
	if !errors.Is(err, store.ErrNoReady) {
		t.Fatalf("PeekReadyCandidate on empty store: err = %v, want ErrNoReady", err)
	}
}

func TestPeekReadyCandidate_IgnoresOtherLanes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedReadyIssue(t, s, "issue-1", "reviewer", 1, 100)

	_, err := s.PeekReadyCandidate(ctx, "coder", 1000)
	if !errors.Is(err, store.ErrNoReady) {
		t.Fatalf("PeekReadyCandidate wrong lane: err = %v, want ErrNoReady", err)
	}
}

// TestPeekColumnCandidate_ReturnsWithoutClaiming mirrors
// TestPeekReadyCandidate_ReturnsWithoutClaiming for the downstream
// per-column peek.
func TestPeekColumnCandidate_ReturnsWithoutClaiming(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const column = "rework"

	seedColumnIssue(t, s, "issue-low", column, 4, 50)
	seedColumnIssue(t, s, "issue-urgent", column, 1, 100)

	issue, err := s.PeekColumnCandidate(ctx, column, 1000)
	if err != nil {
		t.Fatalf("PeekColumnCandidate: unexpected error: %v", err)
	}
	if issue.ID != "issue-urgent" {
		t.Errorf("PeekColumnCandidate = %q, want %q (most urgent)", issue.ID, "issue-urgent")
	}

	got, err := s.GetIssue(ctx, "issue-urgent")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want false (peek must not claim)")
	}
	if got.BoardStatus != column {
		t.Errorf("BoardStatus = %q, want unchanged %q", got.BoardStatus, column)
	}

	claim, err := s.ClaimColumn(ctx, column, "coder", "run-1", 1000, 60)
	if err != nil {
		t.Fatalf("ClaimColumn after peek: unexpected error: %v", err)
	}
	if claim.Issue.ID != "issue-urgent" {
		t.Errorf("ClaimColumn after peek claimed %q, want %q", claim.Issue.ID, "issue-urgent")
	}
}

func TestPeekColumnCandidate_NoneReturnsErrNoReady(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedColumnIssue(t, s, "issue-1", "ready", 1, 100)

	_, err := s.PeekColumnCandidate(ctx, "rework", 1000)
	if !errors.Is(err, store.ErrNoReady) {
		t.Fatalf("PeekColumnCandidate wrong column: err = %v, want ErrNoReady", err)
	}
}

// seedBackedOffIssue seeds an unclaimed issue in column carrying a future
// blocked_until (auto-unblock layer 1's backoff window) so the claim-skip
// tests can drive the boundary. laneLabel is always "coder" (the issue's own
// label never changes; the column decides the lane — see seedColumnIssue).
func seedBackedOffIssue(t *testing.T, s *store.Store, id, column string, blockedUntil int64) {
	t.Helper()
	issue := store.Issue{
		ID:           id,
		Identifier:   id,
		LaneLabel:    "coder",
		BoardStatus:  column,
		BlockedUntil: blockedUntil,
		Deps:         `[]`,
		Priority:     1,
		BranchName:   id + "-branch",
		UpdatedAt:    100,
		LastSeen:     100,
		CreatedAt:    100,
	}
	if err := s.UpsertIssue(context.Background(), issue); err != nil {
		t.Fatalf("seed UpsertIssue(%s): unexpected error: %v", id, err)
	}
}

// TestClaimReady_SkipsBackoffWindow asserts a 'ready' issue whose blocked_until
// lies in the future is invisible to both PeekReadyCandidate and ClaimReady
// until now reaches that deadline (inclusive), then becomes claimable — the
// backoff half of auto-unblock layer 1's anti-hot-loop guarantee.
func TestClaimReady_SkipsBackoffWindow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const blockedUntil = 200
	seedBackedOffIssue(t, s, "issue-1", "ready", blockedUntil)

	// Before the deadline: neither peek nor claim sees it.
	if _, err := s.PeekReadyCandidate(ctx, "coder", blockedUntil-1); !errors.Is(err, store.ErrNoReady) {
		t.Errorf("PeekReadyCandidate at now<deadline: err = %v, want ErrNoReady", err)
	}
	if _, err := s.ClaimReady(ctx, "coder", "run-early", blockedUntil-1, 60); !errors.Is(err, store.ErrNoReady) {
		t.Errorf("ClaimReady at now<deadline: err = %v, want ErrNoReady", err)
	}

	// At the deadline (inclusive) it reappears to a peek...
	peeked, err := s.PeekReadyCandidate(ctx, "coder", blockedUntil)
	if err != nil {
		t.Fatalf("PeekReadyCandidate at now==deadline: unexpected error: %v", err)
	}
	if peeked.ID != "issue-1" {
		t.Errorf("PeekReadyCandidate = %q, want issue-1", peeked.ID)
	}

	// ...and is claimable once now is past it.
	claim, err := s.ClaimReady(ctx, "coder", "run-1", blockedUntil+5, 60)
	if err != nil {
		t.Fatalf("ClaimReady at now>deadline: unexpected error: %v", err)
	}
	if claim.Issue.ID != "issue-1" {
		t.Errorf("ClaimReady claimed %q, want issue-1", claim.Issue.ID)
	}
}

// TestClaimColumn_SkipsBackoffWindow mirrors TestClaimReady_SkipsBackoffWindow
// for the downstream column claim path (e.g. a reviewer/coder-rework re-run
// re-queued after a transient failure): the shared selectClaimCandidate gate
// applies identically.
func TestClaimColumn_SkipsBackoffWindow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const (
		column       = "rework"
		blockedUntil = 200
	)
	seedBackedOffIssue(t, s, "issue-1", column, blockedUntil)

	if _, err := s.PeekColumnCandidate(ctx, column, blockedUntil-1); !errors.Is(err, store.ErrNoReady) {
		t.Errorf("PeekColumnCandidate at now<deadline: err = %v, want ErrNoReady", err)
	}
	if _, err := s.ClaimColumn(ctx, column, "coder", "run-early", blockedUntil-1, 60); !errors.Is(err, store.ErrNoReady) {
		t.Errorf("ClaimColumn at now<deadline: err = %v, want ErrNoReady", err)
	}

	peeked, err := s.PeekColumnCandidate(ctx, column, blockedUntil)
	if err != nil {
		t.Fatalf("PeekColumnCandidate at now==deadline: unexpected error: %v", err)
	}
	if peeked.ID != "issue-1" {
		t.Errorf("PeekColumnCandidate = %q, want issue-1", peeked.ID)
	}

	claim, err := s.ClaimColumn(ctx, column, "coder", "run-1", blockedUntil+5, 60)
	if err != nil {
		t.Fatalf("ClaimColumn at now>deadline: unexpected error: %v", err)
	}
	if claim.Issue.ID != "issue-1" {
		t.Errorf("ClaimColumn claimed %q, want issue-1", claim.Issue.ID)
	}
}

func TestReleaseStaleClaims_NoneStaleReturnsZero(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const lane = "agent:coder"
	seedReadyIssue(t, s, "issue-1", lane, 1, 100)

	if _, err := s.ClaimReady(ctx, lane, "run-1", 1000, 60); err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}

	n, err := s.ReleaseStaleClaims(ctx, 1000)
	if err != nil {
		t.Fatalf("ReleaseStaleClaims: unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("ReleaseStaleClaims released = %d, want 0", n)
	}
}
