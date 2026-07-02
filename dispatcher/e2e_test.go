package dispatcher_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// buildTestworker compiles testworker/main.go into a temp binary, so the
// end-to-end test drives the REAL spawn.LocalSpawner against a real (but
// LLM-free, network-free) worker subprocess.
func buildTestworker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "testworker")
	if os.PathSeparator == '\\' {
		bin += ".exe"
	}
	repoRoot := findRepoRoot(t)
	cmd := exec.Command("go", "build", "-o", bin, "./testworker")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building testworker: %v\n%s", err, out)
	}
	return bin
}

// findRepoRoot walks up from the working directory to the module root
// (go.mod), so `go build ./testworker` resolves regardless of where `go
// test` runs.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root (go.mod) above %s", dir)
		}
		dir = parent
	}
}

// TestTick_EndToEnd_RealSpawnerNeedsReview drives a ready coder issue through
// a full tick cycle with the REAL LocalSpawner executing the built testworker
// binary (scenario needs_review, selected via WorkerSpec.Env), with zero LLM
// and zero real network. It proves the whole path — claim, spawn a real
// subprocess, parse its typed JSON result, map through board.Next, transition
// to review, and mirror to the mock Linear — end to end.
func TestTick_EndToEnd_RealSpawnerNeedsReview(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()

	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := spawn.NewLocalSpawner([]string{bin}, boardDir)
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.MaxRuntimeS = 30 // generous per-worker deadline for the real subprocess

	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithEnvFor(func(store.Issue) []string {
			return append(os.Environ(), "TESTWORKER_SCENARIO=needs_review")
		}),
	)

	// Tick 1 claims + spawns the real subprocess. Wait for it to exit and
	// its result to be applied (re-ticking until the issue leaves running),
	// then assert the terminal board state.
	tickUntilStatus(t, context.Background(), d, s, "issue-1", string(contract.ColumnReview))

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	issue := snap.Issues[0]
	if issue.BoardStatus != string(contract.ColumnReview) {
		t.Fatalf("BoardStatus = %q, want review", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}
	if issue.LatestRun == nil || issue.LatestRun.Status != string(contract.WorkerResultOutcomeNeedsReview) {
		t.Errorf("LatestRun = %+v, want status needs_review", issue.LatestRun)
	}

	// Linear saw running then review, in that order.
	var targets []string
	for _, c := range lc.SetStateCalls {
		targets = append(targets, c.TargetColumn)
	}
	if len(targets) < 2 || targets[0] != "running" || targets[len(targets)-1] != string(contract.ColumnReview) {
		t.Errorf("SetState targets = %v, want running ... review", targets)
	}

	// The worker's stderr log was created by the real LocalSpawner.
	logPath := filepath.Join(boardDir, "logs", "CLP-1.log")
	if _, err := os.Stat(logPath); err != nil {
		// The seeded issue's Identifier is "issue-1" until a poll overwrites
		// it; with no Linear candidates here, the spawned identifier is
		// whatever the seeded issue carries. Accept either.
		altLogPath := filepath.Join(boardDir, "logs", "issue-1.log")
		if _, altErr := os.Stat(altLogPath); altErr != nil {
			t.Errorf("worker stderr log not found at %s or %s", logPath, altLogPath)
		}
	}
}

// tickUntilStatus re-ticks until issueID reaches want or a deadline passes.
// The real subprocess takes non-deterministic wall-clock time to exit, so a
// single follow-up tick may drain nothing; this polls with a short pause.
func tickUntilStatus(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, s *store.Store, issueID, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("Tick while waiting for %q: unexpected error: %v", want, err)
		}
		issue, err := s.GetIssue(ctx, issueID)
		if err != nil {
			t.Fatalf("GetIssue while waiting for %q: unexpected error: %v", want, err)
		}
		if issue.BoardStatus == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("issue %s never reached %q within deadline", issueID, want)
}
