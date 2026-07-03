package dispatcher_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/store"
)

// envValue returns the value bound to key in a KEY=VALUE environment slice
// (as carried on spawn.WorkerSpec.Env), and whether it was present at all.
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// openTestStore opens a Store backed by a fresh SQLite file in a temp dir,
// mirroring internal/store's own test helper (unexported there, so it can't
// be reused directly from this package).
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: unexpected error: %v", err)
		}
	})
	return s
}

// testConfig returns a Config with small, deterministic caps suitable for
// most dispatcher tests. Individual tests override fields (e.g. Caps,
// TurnCap) as needed.
func testConfig() config.Config {
	return config.Config{
		Repo:            config.Repo{Remote: "origin", Path: "/repo", BaseBranch: "main"},
		PollIntervalS:   30,
		TurnCap:         3,
		MaxRuntimeS:     3600,
		LaneLabelPrefix: "agent:",
		MaxAttempts:     3,
		ReworkCap:       3,
		Caps: config.Caps{
			Global: 8,
			PerLane: config.PerLaneCaps{
				Coder:       4,
				Reviewer:    2,
				GitOperator: 1,
			},
		},
	}
}

// newTestDispatcher wires a Dispatcher from the given store/linear/spawner
// with a fixed clock and deterministic sequential run ids, so assertions
// about exact run ids and timestamps are stable across test runs.
func newTestDispatcher(t *testing.T, cfg config.Config, st *store.Store, lc linear.Client, spawner *fakeSpawner, ws dispatcher.Workspacer, clock func() int64) *dispatcher.Dispatcher {
	t.Helper()
	return dispatcher.New(cfg, st, lc, spawner, ws, dispatcher.WithClock(clock), dispatcher.WithRunIDGenerator(sequentialRunIDs()))
}

// fixedClock returns a clock func that always reports t.
func fixedClock(t int64) func() int64 {
	return func() int64 { return t }
}

// tickUntilBlocked re-ticks the dispatcher until issueID reaches the blocked
// column or a deadline passes, then fails the test if it never blocked. It
// exists for the timeout failure case, whose worker result is only posted
// after the spawn-context deadline fires asynchronously — so a single
// follow-up tick may drain nothing. A brief pause between ticks lets the
// Wait-goroutine deliver its result without busy-spinning.
func tickUntilBlocked(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, s *store.Store, issueID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("Tick while waiting for block: unexpected error: %v", err)
		}
		issue, err := s.GetIssue(ctx, issueID)
		if err != nil {
			t.Fatalf("GetIssue while waiting for block: unexpected error: %v", err)
		}
		if issue.BoardStatus == string(contract.ColumnBlocked) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("issue %s never reached blocked within deadline", issueID)
}

// seedReadyIssue inserts a single 'ready' issue in the bare lane (e.g.
// "coder"), branch named "<id>-branch" (spawnAttempt's Workspacer.Ensure and
// the end-to-end tests need a non-empty BranchName), ready to be claimed.
func seedReadyIssue(t *testing.T, s *store.Store, id, lane string, priority int, createdAt int64) {
	t.Helper()
	ctx := context.Background()
	issue := store.Issue{
		ID:          id,
		Identifier:  id,
		LaneLabel:   lane,
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

// seedColumnIssue inserts a single issue already sitting in column (e.g.
// "review"), unclaimed, ready to be claimed by ClaimColumn — the downstream
// analogue of seedReadyIssue (Phase 3 cross-lane claiming). LaneLabel is
// always the bare "coder": per the kernel invariant, an issue's own
// lane_label never changes as it moves through downstream columns —
// ClaimColumn dispatches whichever lane the COLUMN implies, not the issue's
// label.
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
