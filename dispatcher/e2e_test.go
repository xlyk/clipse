package dispatcher_test

import (
	"context"
	"encoding/json"
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

func TestRun_EndToEnd_DrainRestartResumeUsesReplacementConfig(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	releaseFile := filepath.Join(t.TempDir(), "release")
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 10)
	seedReadyIssue(t, s, "issue-2", "coder", 1, 20)
	cfg := testConfig()
	cfg.MaxRuntimeS = 30
	cfg.Caps.Global = 1
	cfg.Caps.PerLane.Coder = 1
	cfg.Caps.PerLane.Reviewer = 0

	oldSpawner := spawn.NewLocalSpawner([]string{bin}, boardDir)
	d1 := dispatcher.New(cfg, s, &linear.MockClient{}, oldSpawner, newStubWorkspacer(t.TempDir()),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(10*time.Millisecond),
		dispatcher.WithInstanceID("old-instance"),
		dispatcher.WithEnvFor(func(store.Issue) []string {
			return append(os.Environ(), "TESTWORKER_SCENARIO=wait_file", "TESTWORKER_RELEASE_FILE="+releaseFile)
		}),
	)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	done1 := make(chan error, 1)
	go func() { done1 <- d1.Run(ctx1) }()
	waitFor(t, 5*time.Second, func() bool {
		control, err := s.ReadDispatcherControl(context.Background())
		counts, countErr := s.DispatcherRuntimeCounts(context.Background())
		return err == nil && countErr == nil && control.ActiveInstanceID == "old-instance" && counts.ActiveRuns == 1
	}, "real worker claim")
	if err := s.RequestDrain(context.Background(), "drain-e2e", time.Now().Unix(), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		control, err := s.ReadDispatcherControl(context.Background())
		return err == nil && control.ObservedMode == store.ObservedDraining
	}, "real dispatcher drain acknowledgment")
	queued, err := s.GetIssue(context.Background(), "issue-2")
	if err != nil {
		t.Fatal(err)
	}
	if queued.ClaimLock.Valid {
		t.Fatal("second issue was claimed after the drain barrier")
	}
	if err := os.WriteFile(releaseFile, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done1:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old dispatcher did not exit after real worker completion")
	}

	newSpawner := spawn.NewLocalSpawner([]string{bin}, boardDir)
	d2 := dispatcher.New(cfg, s, &linear.MockClient{}, newSpawner, newStubWorkspacer(t.TempDir()),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(10*time.Millisecond),
		dispatcher.WithInstanceID("new-instance"),
		dispatcher.WithEnvFor(func(store.Issue) []string {
			return append(os.Environ(), "TESTWORKER_SCENARIO=report_env", "TESTWORKER_CONFIG_MARKER=replacement-v2")
		}),
	)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- d2.Run(ctx2) }()
	waitFor(t, 5*time.Second, func() bool {
		control, err := s.ReadDispatcherControl(context.Background())
		return err == nil && control.ActiveInstanceID == "new-instance" && control.DesiredMode == store.SchedulingPaused
	}, "replacement registration while paused")
	time.Sleep(50 * time.Millisecond)
	queued, err = s.GetIssue(context.Background(), "issue-2")
	if err != nil {
		t.Fatal(err)
	}
	if queued.ClaimLock.Valid {
		t.Fatal("replacement claimed work before explicit resume")
	}
	if err := s.RequestResume(context.Background(), "resume-e2e", time.Now().Unix(), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		issue, err := s.GetIssue(context.Background(), "issue-2")
		return err == nil && issue.BoardStatus == string(contract.ColumnReview) && !issue.ClaimLock.Valid
	}, "first post-resume worker result")
	cancel2()
	select {
	case err := <-done2:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement dispatcher did not stop")
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var replacementResult contract.WorkerResult
	for _, issue := range snap.Issues {
		if issue.ID != "issue-2" || issue.LatestRun == nil || !issue.LatestRun.ResultJSON.Valid {
			continue
		}
		if err := json.Unmarshal([]byte(issue.LatestRun.ResultJSON.String), &replacementResult); err != nil {
			t.Fatal(err)
		}
	}
	if replacementResult.Summary != "replacement-v2" {
		t.Fatalf("first post-resume worker summary = %q, want replacement config marker", replacementResult.Summary)
	}
	events, err := s.ListEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "orphan_requeue" || event.Kind == "orphan_blocked" {
			t.Fatalf("planned restart emitted orphan event: %+v", event)
		}
	}
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
//
// Caps.PerLane.Reviewer is zeroed for the same reason happy_test.go's
// TestTick_HappyPath_NeedsReviewMovesToReview zeroes it: without it, a
// same-tick reviewer claim on the freshly-opened review card would spawn
// testworker again under WithEnvFor's fixed TESTWORKER_SCENARIO=needs_review
// override — an outcome illegal from "review" — defensively blocking the
// issue instead of leaving it at review for this test to observe.
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
	cfg.Caps.PerLane.Reviewer = 0

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
