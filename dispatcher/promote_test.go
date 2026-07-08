package dispatcher_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/store"
)

// TestTick_PollCancelledDependencyUnblocksPromotion is an end-to-end
// regression guard for the finding-3 fix: a Linear-cancelled blocker, once
// adopted into SQLite as board_status="cancelled" (Task 3's producer fix in
// internal/linear), counts as terminal for board.Promote — so a dependent
// sitting in Todo waiting on it is no longer stuck forever. This assertion
// already holds against dispatcher/promote.go's pre-existing (previously
// dead) logic; it's here to lock in the full pipeline, not because this
// task changed any dispatcher code.
func TestTick_PollCancelledDependencyUnblocksPromotion(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// issue-2 (the blocker) sits at "review", unclaimed -- as if a human
	// cancelled the ticket mid-review instead of letting it finish.
	seedColumnIssue(t, s, "issue-2", "review", 1, 100)
	// issue-1 (the dependent) sits in Todo, waiting on issue-2.
	issue1 := store.Issue{
		ID: "issue-1", Identifier: "CLP-1", LaneLabel: "coder", BoardStatus: "todo",
		Deps: `["issue-2"]`, Priority: 1, BranchName: "issue-1-branch",
		UpdatedAt: 100, LastSeen: 100, CreatedAt: 100,
	}
	if err := s.UpsertIssue(ctx, issue1); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	// Linear now reports issue-2 as cancelled.
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-2", Identifier: "CLP-2", Status: "cancelled", Lane: "coder", Priority: 1, BranchName: "issue-2-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got2, err := s.GetIssue(ctx, "issue-2")
	if err != nil {
		t.Fatalf("GetIssue(issue-2): unexpected error: %v", err)
	}
	if got2.BoardStatus != "cancelled" {
		t.Fatalf("issue-2 BoardStatus = %q, want cancelled (adopted from Linear)", got2.BoardStatus)
	}

	got1, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue(issue-1): unexpected error: %v", err)
	}
	if got1.BoardStatus != "ready" {
		t.Errorf("issue-1 BoardStatus = %q, want ready (promoted once its cancelled dependency counted as terminal)", got1.BoardStatus)
	}
}
