package dispatcher_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
)

// TestTick_RespectsCapsAcrossManyReadyIssues asserts that with many ready
// coder issues and small caps, in-flight spawns never exceed either
// cfg.Caps.Global or the coder per-lane cap, observed by counting concurrent
// spawns the fake Spawner has seen with no matching Wait yet.
func TestTick_RespectsCapsAcrossManyReadyIssues(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("issue-%d", i)
		seedReadyIssue(t, s, id, "coder", 1, int64(100+i))
	}

	spawner := newFakeSpawner()
	// Every spawn hangs (never resolves) so the dispatcher's in-flight
	// bookkeeping is the only thing keeping later ticks from over-claiming.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("issue-%d", i)
		spawner.Results[id] = spawn.Result{Err: context.DeadlineExceeded}
	}

	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.MaxRuntimeS = 3600 // hung workers below never resolve within this test's lifetime
	cfg.Caps = config.Caps{
		Global:  3,
		PerLane: config.PerLaneCaps{Coder: 2, Reviewer: 2, GitOperator: 2},
	}
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	// Multiple ticks: since every spawned worker hangs (fakeHandle.Wait
	// blocks until ctx.Done(), which never fires because MaxRuntimeS is
	// large), no run ever resolves, so in-flight count should plateau at
	// the coder per-lane cap (2) and never exceed the global cap (3) either.
	for i := 0; i < 5; i++ {
		if err := d.Tick(context.Background()); err != nil {
			t.Fatalf("Tick %d: unexpected error: %v", i, err)
		}
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	var running int
	for _, is := range snap.Issues {
		if is.BoardStatus == string(contract.ColumnRunning) {
			running++
		}
	}
	if running != 2 {
		t.Errorf("running issues = %d, want exactly 2 (coder per-lane cap)", running)
	}
	if spawner.SpawnCount() != 2 {
		t.Errorf("SpawnCount() = %d, want 2 (never exceeded caps across ticks)", spawner.SpawnCount())
	}
}

// TestTick_GlobalCapLimitsAcrossLanes asserts the global cap is enforced
// across lanes even when each lane's own per-lane cap would allow more.
func TestTick_GlobalCapLimitsAcrossLanes(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-coder-1", "coder", 1, 100)
	seedReadyIssue(t, s, "issue-coder-2", "coder", 1, 101)
	seedReadyIssue(t, s, "issue-reviewer-1", "reviewer", 1, 102)
	seedReadyIssue(t, s, "issue-reviewer-2", "reviewer", 1, 103)

	spawner := newFakeSpawner()
	for _, id := range []string{"issue-coder-1", "issue-coder-2", "issue-reviewer-1", "issue-reviewer-2"} {
		spawner.Results[id] = spawn.Result{Err: context.DeadlineExceeded}
	}

	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.Caps = config.Caps{
		Global:  2,
		PerLane: config.PerLaneCaps{Coder: 4, Reviewer: 4, GitOperator: 4},
	}
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	if spawner.SpawnCount() != 2 {
		t.Fatalf("SpawnCount() = %d, want 2 (global cap)", spawner.SpawnCount())
	}
}
