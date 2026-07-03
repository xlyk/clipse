package dispatcher_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
)

// TestTick_HappyPath_NeedsReviewMovesToReview drives one ready coder issue
// through claim -> spawn -> needs_review, asserting the full observable
// trail: the running mirror is enqueued and sent before review, the run
// closes, board_status ends at review, and the claim is cleared.
//
// Caps.PerLane.Reviewer is zeroed so nothing claims the freshly-opened
// review card within this same tick (Phase 3's cross-lane claiming is
// same-tick responsive, exactly like promote -> claim already is) —
// isolating that lets this test assert purely on the Coder lane's own
// needs_review -> review transition, which a same-tick reviewer claim would
// otherwise race with (this fake Spawner scripts one result per issue
// identifier, shared by whichever lane spawns it next).
func TestTick_HappyPath_NeedsReviewMovesToReview(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{
			Outcome: contract.WorkerResultOutcomeNeedsReview,
			Summary: "opened PR",
			Tokens:  contract.WorkerResultTokens{In: 10, Out: 20},
		},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.Caps.PerLane.Reviewer = 0
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	// Tick 1: poll (no candidates from Linear here; issue seeded directly),
	// claim + spawn issue-1. The fake worker resolves immediately.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: unexpected error: %v", err)
	}
	// Tick 2: reconcile drains the result and applies the transition, then
	// drains the outbox.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if len(snap.Issues) != 1 {
		t.Fatalf("len(Issues) = %d, want 1", len(snap.Issues))
	}
	issue := snap.Issues[0]
	if issue.BoardStatus != string(contract.ColumnReview) {
		t.Errorf("BoardStatus = %q, want %q", issue.BoardStatus, contract.ColumnReview)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}
	if issue.LatestRun == nil {
		t.Fatalf("LatestRun = nil, want the closed run")
	}
	if issue.LatestRun.Status != string(contract.WorkerResultOutcomeNeedsReview) {
		t.Errorf("LatestRun.Status = %q, want %q", issue.LatestRun.Status, contract.WorkerResultOutcomeNeedsReview)
	}
	if issue.LatestRun.TokensIn != 10 || issue.LatestRun.TokensOut != 20 {
		t.Errorf("LatestRun tokens = (%d,%d), want (10,20)", issue.LatestRun.TokensIn, issue.LatestRun.TokensOut)
	}

	var setStateTargets []string
	for _, c := range lc.SetStateCalls {
		setStateTargets = append(setStateTargets, c.TargetColumn)
	}
	if len(setStateTargets) != 2 || setStateTargets[0] != "running" || setStateTargets[1] != string(contract.ColumnReview) {
		t.Errorf("SetState targets = %v, want [running review]", setStateTargets)
	}
}

// TestTick_DefensiveBlockOnIllegalOutcome asserts that an outcome which is
// illegal from the 'running' column lands the issue in 'blocked' defensively
// rather than panicking or leaving it stuck. A coder never legally emits
// 'done' or 'changes_requested' from running (see board.Next's table), so
// this exercises applyResult's board.Next-error branch — the only way those
// outcomes are reachable in Phase 1's coder-only loop.
func TestTick_DefensiveBlockOnIllegalOutcome(t *testing.T) {
	cases := []struct {
		name    string
		outcome contract.WorkerResultOutcome
	}{
		{"done_from_running", contract.WorkerResultOutcomeDone},
		{"changes_requested_from_running", contract.WorkerResultOutcomeChangesRequested},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

			spawner := newFakeSpawner()
			spawner.Results["issue-1"] = spawn.Result{
				Worker: contract.WorkerResult{Outcome: tc.outcome, Summary: "illegal from running"},
			}
			ws := newStubWorkspacer(t.TempDir())
			lc := &linear.MockClient{}
			d := newTestDispatcher(t, testConfig(), s, lc, spawner, ws, fixedClock(1000))

			if err := d.Tick(context.Background()); err != nil {
				t.Fatalf("tick 1: unexpected error: %v", err)
			}
			if err := d.Tick(context.Background()); err != nil {
				t.Fatalf("tick 2: unexpected error: %v", err)
			}

			snap, err := s.ReadSnapshot(context.Background())
			if err != nil {
				t.Fatalf("ReadSnapshot: unexpected error: %v", err)
			}
			if snap.Issues[0].BoardStatus != string(contract.ColumnBlocked) {
				t.Errorf("BoardStatus = %q, want blocked (defensive)", snap.Issues[0].BoardStatus)
			}
			if snap.Issues[0].ClaimLock.Valid {
				t.Errorf("ClaimLock.Valid = true, want cleared on block")
			}
			// A defensive block still posts a reason comment.
			if len(lc.CommentCalls) == 0 {
				t.Errorf("no Comment call recorded, want a block-reason comment")
			}
		})
	}
}
