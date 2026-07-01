package dispatcher_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
)

// zeroCapConfig returns a Config identical to testConfig but with every cap
// zeroed, so a Tick's selectAndClaim phase never claims anything — letting
// poll-focused tests observe pollAndUpsert's effect in isolation from the
// same-tick claim it would otherwise trigger.
func zeroCapConfig() config.Config {
	cfg := testConfig()
	cfg.Caps = config.Caps{}
	return cfg
}

// TestTick_PollCachesCandidatesFromLinear asserts pollAndUpsert caches every
// candidate Linear returns, mapping Lane->lane_label and Status->board_status
// on the initial insert. issue-2 has an unresolved dependency so promote
// leaves it in todo, isolating pollAndUpsert's own mapping from promotion's
// separate effect.
func TestTick_PollCachesCandidatesFromLinear(t *testing.T) {
	s := openTestStore(t)
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "clp-1", UpdatedAt: 100},
			{ID: "issue-2", Identifier: "CLP-2", Status: "todo", Lane: "", Deps: []string{"issue-1"}, Priority: 0, BranchName: "clp-2", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	byID := map[string]string{}
	byLane := map[string]string{}
	for _, is := range snap.Issues {
		byID[is.ID] = is.BoardStatus
		byLane[is.ID] = is.LaneLabel
	}
	if byID["issue-1"] != "ready" {
		t.Errorf("issue-1 board_status = %q, want ready", byID["issue-1"])
	}
	if byLane["issue-1"] != "coder" {
		t.Errorf("issue-1 lane_label = %q, want coder", byLane["issue-1"])
	}
	// issue-2 has no lane, so it's cached but inert for dispatch; its
	// dependency (issue-1) isn't terminal, so promote leaves it in todo.
	if byID["issue-2"] != "todo" {
		t.Errorf("issue-2 board_status = %q, want todo", byID["issue-2"])
	}
	if byLane["issue-2"] != "" {
		t.Errorf("issue-2 lane_label = %q, want empty", byLane["issue-2"])
	}
}

// TestTick_RepollPreservesRunningBoardStatus asserts that once an issue is
// claimed and running, a later poll returning its old Linear status (e.g.
// "ready", since Linear hasn't been mirrored to "running" from Linear's own
// perspective at poll time) does not reset board_status away from running.
// UpsertIssue's own conflict semantics guarantee this; this test exercises
// it through a full Tick pass.
func TestTick_RepollPreservesRunningBoardStatus(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "clp-1-branch", UpdatedAt: 100},
		},
	}
	spawner := newFakeSpawner()
	// Every re-spawn of issue-1 (Linear identifier CLP-1) reports "continue"
	// with a distant turn cap, so the run stays inflight/running across both
	// ticks rather than resolving to a terminal transition mid-test.
	spawner.Results["CLP-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeContinue, ThreadId: "thread-1"},
	}
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.TurnCap = 1000
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	// First tick: claims and spawns issue-1 (moves it to running).
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].BoardStatus != "running" {
		t.Fatalf("after first tick, board_status = %q, want running", snap.Issues[0].BoardStatus)
	}

	// Second tick re-polls the same stale "ready" status from Linear.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: unexpected error: %v", err)
	}

	snap2, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap2.Issues[0].BoardStatus != "running" {
		t.Errorf("after second tick (repoll), board_status = %q, want preserved running", snap2.Issues[0].BoardStatus)
	}
}
