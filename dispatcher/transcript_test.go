package dispatcher_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/linear"
)

// TestSpawnAttempt_WiresTranscriptPath asserts a claimed issue's spawned
// worker gets a --transcript path derived from cfg.BoardDir (one file per
// issue, named by the issue's Linear identifier, living in <board_dir>/logs
// next to the per-issue stderr log -- AGENTS.md's transcript bullet), the
// same direct cfg-to-spec forwarding as CheckpointDB/MaxTokens/BaseBranch
// (see checkpoint_test.go).
func TestSpawnAttempt_WiresTranscriptPath(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.BoardDir = "/tmp/clipse-board"

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

	wantTranscript := filepath.Join(cfg.BoardDir, "logs", "issue-1.transcript.jsonl")
	if specs[0].TranscriptPath != wantTranscript {
		t.Errorf("TranscriptPath = %q, want %q", specs[0].TranscriptPath, wantTranscript)
	}
}

// TestSpawnAttempt_TranscriptPathEmptyWhenBoardDirUnset asserts a Config
// with no BoardDir configured (the zero value -- e.g. a hand-built test
// Config that never went through config.Load, as most dispatcher tests
// use) produces an empty WorkerSpec.TranscriptPath rather than a
// nonsensical path rooted at "", so LocalSpawner omits --transcript
// entirely (see internal/spawn.workerArgs) and the worker runs with
// transcripts disabled. Real production configs always have a non-empty
// BoardDir (config.Load defaults it).
func TestSpawnAttempt_TranscriptPathEmptyWhenBoardDirUnset(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig() // BoardDir left at zero value.

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
	if specs[0].TranscriptPath != "" {
		t.Errorf("TranscriptPath = %q, want empty (BoardDir unset)", specs[0].TranscriptPath)
	}
}
