package dispatcher_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/linear"
)

// TestSpawnAttempt_WiresCheckpointDBAndMaxTokens proves the production gap
// this task closes: constructed exactly the way cli/dispatch.go's
// runDispatch wires the real Dispatcher, a claimed issue's spawned worker
// gets a --checkpoint-db path derived from cfg.CheckpointsDir (one file per
// issue, named by the issue's Linear identifier — design doc:
// "<board>/checkpoints/<issue_id>.db") and cfg.MaxTokensPerRun forwarded as
// WorkerSpec.MaxTokens, so LocalSpawner appends --checkpoint-db and
// --max-tokens per the Phase 2 plan's checkpointer-path and token-ceiling
// work items.
func TestSpawnAttempt_WiresCheckpointDBAndMaxTokens(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.CheckpointsDir = "/tmp/clipse-checkpoints"
	cfg.MaxTokensPerRun = 150000

	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1", len(specs))
	}

	wantCheckpointDB := filepath.Join(cfg.CheckpointsDir, "issue-1.db")
	if specs[0].CheckpointDB != wantCheckpointDB {
		t.Errorf("CheckpointDB = %q, want %q", specs[0].CheckpointDB, wantCheckpointDB)
	}
	if specs[0].MaxTokens != cfg.MaxTokensPerRun {
		t.Errorf("MaxTokens = %d, want %d", specs[0].MaxTokens, cfg.MaxTokensPerRun)
	}
}

// TestSpawnAttempt_CheckpointDBEmptyWhenCheckpointsDirUnset asserts that a
// Config with no CheckpointsDir configured (the zero value — e.g. a
// hand-built test Config that never went through config.Load, as most
// dispatcher tests use) produces an empty WorkerSpec.CheckpointDB rather
// than a nonsensical path rooted at "", so LocalSpawner omits
// --checkpoint-db entirely (see internal/spawn.workerArgs). Real production
// configs always have a non-empty CheckpointsDir (config.Load defaults it),
// so this only matters for tests/tools that build a config.Config by hand.
func TestSpawnAttempt_CheckpointDBEmptyWhenCheckpointsDirUnset(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig() // CheckpointsDir/MaxTokensPerRun left at zero value.

	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1", len(specs))
	}
	if specs[0].CheckpointDB != "" {
		t.Errorf("CheckpointDB = %q, want empty (CheckpointsDir unset)", specs[0].CheckpointDB)
	}
	if specs[0].MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, want 0 (MaxTokensPerRun unset)", specs[0].MaxTokens)
	}
}
