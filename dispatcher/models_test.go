package dispatcher_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
)

// testModels returns a fixed, distinguishable-per-lane config.Models used
// across this file's cases, so a wrong lane's value leaking onto a spec is
// unmistakable in a test failure message.
func testModels() config.Models {
	return config.Models{
		Coder:     "openai_codex:gpt-5.5",
		CoderDocs: "anthropic:claude-sonnet-4-6",
		Reviewer:  "anthropic:claude-opus-4-6",
	}
}

// TestSpawnAttempt_ResolvesCoderModelAndDocsModel asserts a fresh Coder claim
// (spawnClaim -> spawnAttempt -> modelsFor) carries cfg.Models.Coder /
// .CoderDocs onto the WorkerSpec — closing the gap a prior reviewer flagged:
// the dispatcher's WorkerSpec literal never populated Model/DocsModel at all.
func TestSpawnAttempt_ResolvesCoderModelAndDocsModel(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.Models = testModels()

	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1", len(specs))
	}
	if specs[0].Model != cfg.Models.Coder {
		t.Errorf("Model = %q, want %q", specs[0].Model, cfg.Models.Coder)
	}
	if specs[0].DocsModel != cfg.Models.CoderDocs {
		t.Errorf("DocsModel = %q, want %q", specs[0].DocsModel, cfg.Models.CoderDocs)
	}
}

// TestSpawnAttempt_ReviewerClaimHasNoDocsModel asserts a Reviewer-column
// claim resolves cfg.Models.Reviewer as Model but leaves DocsModel empty —
// the docs sub-step is a coder-graph-only concern (AGENTS.md: "Documentation
// is a coder-graph step, not a lane"), so a Reviewer worker has no use for it.
func TestSpawnAttempt_ReviewerClaimHasNoDocsModel(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "review", 1, 100)

	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.Models = testModels()

	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1", len(specs))
	}
	if specs[0].Lane != string(contract.LaneReviewer) {
		t.Fatalf("Lane = %q, want %q (bad seed/claim)", specs[0].Lane, contract.LaneReviewer)
	}
	if specs[0].Model != cfg.Models.Reviewer {
		t.Errorf("Model = %q, want %q", specs[0].Model, cfg.Models.Reviewer)
	}
	if specs[0].DocsModel != "" {
		t.Errorf("DocsModel = %q, want empty (Reviewer lane never runs docs)", specs[0].DocsModel)
	}
}

// TestApplyContinue_PreservesModelResolution asserts a turn-cap "continue"
// re-spawn — which goes through applyContinue -> spawnAttempt, not
// spawnClaim -> spawnAttempt — resolves the same lane's model exactly like
// the initial claim's spawn did, since modelsFor depends only on the lane
// (constant across a run's turns), not on which caller invoked spawnAttempt.
func TestApplyContinue_PreservesModelResolution(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{
			Outcome:  contract.WorkerResultOutcomeContinue,
			ThreadId: "thread-continue",
			Summary:  "made progress, more to do",
		},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.TurnCap = 2
	cfg.Models = testModels()

	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	// Tick 1 claims + spawns turn 1; tick 2 drains turn 1's continue result
	// and (via applyContinue) re-spawns turn 2. Extra ticks give the Wait
	// goroutine room to deliver without the test racing it (mirrors
	// TestTick_ContinueRespawnsUntilTurnCapThenBlocks in outcomes_test.go).
	for i := 0; i < cfg.TurnCap+3; i++ {
		if err := d.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: unexpected error: %v", i, err)
		}
	}

	specs := spawner.Specs()
	if len(specs) < 2 {
		t.Fatalf("SpawnCount = %d, want at least 2 (initial claim + one continuation)", len(specs))
	}
	respawn := specs[1]
	if respawn.Model != cfg.Models.Coder {
		t.Errorf("continuation Model = %q, want %q", respawn.Model, cfg.Models.Coder)
	}
	if respawn.DocsModel != cfg.Models.CoderDocs {
		t.Errorf("continuation DocsModel = %q, want %q", respawn.DocsModel, cfg.Models.CoderDocs)
	}
}
