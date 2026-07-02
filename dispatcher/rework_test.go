package dispatcher_test

import (
	"context"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
)

// TestTick_ReworkLoop_TerminatesAtReworkCap asserts amendment C1: a
// permanently-disagreeing Reviewer (mocked) drives the issue through
// exactly cfg.ReworkCap review<->rework cycles, and the cap-exceeding cycle
// parks the issue in Blocked (with a comment naming the cap and the last
// review) instead of ever re-dispatching the Coder lane a third time —
// never an infinite Coder<->Reviewer loop.
func TestTick_ReworkLoop_TerminatesAtReworkCap(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "review", 1, 100)

	prURL := "https://github.com/x/y/pull/1"
	spawner := newFakeSpawner()
	spawner.ResultsQueue["issue-1"] = []spawn.Result{
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeChangesRequested, Summary: "review 1: needs work", PrUrl: &prURL}},
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "coder 1: addressed"}},
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeChangesRequested, Summary: "review 2: still needs work", PrUrl: &prURL}},
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "coder 2: addressed again"}},
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeChangesRequested, Summary: "review 3: still not good enough", PrUrl: &prURL}},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.ReworkCap = 2
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	for i := 0; i < 20; i++ {
		if err := d.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: unexpected error: %v", i, err)
		}
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnBlocked) {
		t.Fatalf("BoardStatus = %q, want blocked (rework cap exceeded)", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}
	if issue.ReworkCount != cfg.ReworkCap {
		t.Errorf("ReworkCount = %d, want exactly %d (never incremented past the cap)", issue.ReworkCount, cfg.ReworkCap)
	}

	// Exactly cfg.ReworkCap coder re-runs happened (review1,coder1,review2,
	// coder2,review3): the cap-exceeding 3rd changes_requested blocks
	// instead of ever spawning a 3rd coder re-run.
	specs := spawner.Specs()
	if len(specs) != 5 {
		t.Fatalf("SpawnCount = %d, want exactly 5 (no coder re-run spawned after the cap trips)", len(specs))
	}

	// Lanes strictly alternate reviewer/coder/reviewer/coder/reviewer: each
	// changes_requested claims and dispatches the Coder lane from rework
	// (never a human, and never off the issue's own — always "coder" —
	// lane_label), and each needs_review claims the Reviewer lane back
	// from review.
	wantLanes := []string{
		string(contract.LaneReviewer), string(contract.LaneCoder),
		string(contract.LaneReviewer), string(contract.LaneCoder),
		string(contract.LaneReviewer),
	}
	var coderSpawns int
	for i, sp := range specs {
		if sp.Lane == string(contract.LaneCoder) {
			coderSpawns++
		}
		if i < len(wantLanes) && sp.Lane != wantLanes[i] {
			t.Errorf("spawn %d Lane = %q, want %q", i, sp.Lane, wantLanes[i])
		}
	}
	if coderSpawns != cfg.ReworkCap {
		t.Errorf("coder re-run spawns = %d, want exactly ReworkCap (%d)", coderSpawns, cfg.ReworkCap)
	}

	// A further tick must not reclaim/respawn anything for this issue (no
	// auto-retry past a block, same H invariant every other block path
	// honors).
	spawnsAfterBlock := spawner.SpawnCount()
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("extra tick: unexpected error: %v", err)
	}
	if spawner.SpawnCount() != spawnsAfterBlock {
		t.Errorf("SpawnCount grew to %d after block, want no further claim (%d)", spawner.SpawnCount(), spawnsAfterBlock)
	}

	// The block comment names the cap and the last review's PR + summary.
	var comment string
	for _, c := range lc.CommentCalls {
		comment = c.Body
	}
	if !strings.Contains(comment, "rework cap") {
		t.Errorf("block comment = %q, want it to mention the rework cap", comment)
	}
	if !strings.Contains(comment, prURL) {
		t.Errorf("block comment = %q, want it to link the PR %q", comment, prURL)
	}
	if !strings.Contains(comment, "review 3") {
		t.Errorf("block comment = %q, want it to reference the last review", comment)
	}
}
