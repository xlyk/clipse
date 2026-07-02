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
