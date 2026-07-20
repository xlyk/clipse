package spawn_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/spawn"
)

// buildTestworker compiles testworker/main.go into a temp binary once per
// test and returns its path. Building inside the test (rather than shelling
// out to a pre-built binary) keeps the test self-contained and in sync with
// the current testworker source.
func buildTestworker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "testworker")
	if runtimeIsWindows() {
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

func runtimeIsWindows() bool {
	return os.PathSeparator == '\\'
}

// findRepoRoot walks up from the current working directory to find the
// module root (identified by go.mod), so `go build ./testworker` resolves
// regardless of where `go test` is invoked from.
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

func newSpec(t *testing.T, boardDir, scenario string) spawn.WorkerSpec {
	t.Helper()
	return spawn.WorkerSpec{
		Issue:     "CLP-1",
		Lane:      "coder",
		RunID:     "run-1",
		ThreadID:  "thread-1",
		Workspace: t.TempDir(),
		Env:       append(os.Environ(), "TESTWORKER_SCENARIO="+scenario),
	}
}

// TestLocalSpawner_Success asserts that a clean-exit, valid-JSON worker run
// produces a Result with the parsed WorkerResult, ExitCode 0, and no error.
func TestLocalSpawner_Success(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner([]string{bin}, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := newSpec(t, boardDir, "needs_review")

	handle, err := s.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}
	if handle.PID() <= 0 {
		t.Errorf("PID() = %d, want > 0", handle.PID())
	}
	if handle.ProcStartedAt() <= 0 {
		t.Errorf("ProcStartedAt() = %d, want > 0", handle.ProcStartedAt())
	}

	res, err := handle.Wait()
	if err != nil {
		t.Fatalf("Wait: unexpected top-level error: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("Result.Err = %v, want nil", res.Err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Worker.Outcome != contract.WorkerResultOutcomeNeedsReview {
		t.Errorf("Outcome = %q, want needs_review", res.Worker.Outcome)
	}
	if res.Worker.IssueId != "CLP-1" {
		t.Errorf("IssueId = %q, want CLP-1", res.Worker.IssueId)
	}
	if res.Worker.RunId != "run-1" {
		t.Errorf("RunId = %q, want run-1", res.Worker.RunId)
	}

	assertStderrLog(t, boardDir, "CLP-1")
}

// TestLocalSpawner_RunsInWorkspaceDir asserts the Spawner runs the worker
// process with the issue worktree as its working directory, not the
// dispatcher's own cwd. This matters because DAC (create_cli_agent) resolves
// the injected memory/AGENTS.md guides from settings.project_root, which is
// bootstrapped from the worker process's Path.cwd() -- not from the cwd=
// argument dac.build_coder_agent passes (that only sets the filesystem/shell
// backend root and prompt text). Before cmd.Dir was set, every coder round
// carried the dispatcher repo's AGENTS.md instead of the target repo's guides
// (Reflex retro). The "pwd" testworker scenario reports its own os.Getwd().
func TestLocalSpawner_RunsInWorkspaceDir(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner([]string{bin}, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspace := t.TempDir()
	// os.Getwd() in the worker reports the symlink-resolved path (on macOS
	// t.TempDir() lives under /var -> /private/var), so resolve the expected
	// path the same way before comparing.
	wantDir, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", workspace, err)
	}

	spec := spawn.WorkerSpec{
		Issue:     "CLP-1",
		Lane:      "coder",
		RunID:     "run-1",
		ThreadID:  "thread-1",
		Workspace: workspace,
		Env:       append(os.Environ(), "TESTWORKER_SCENARIO=pwd"),
	}

	handle, err := s.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}

	res, err := handle.Wait()
	if err != nil {
		t.Fatalf("Wait: unexpected top-level error: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("Result.Err = %v, want nil", res.Err)
	}
	if res.Worker.Summary != wantDir {
		t.Errorf("worker working directory = %q, want the issue worktree %q", res.Worker.Summary, wantDir)
	}
}

func TestLocalSpawner_DaytonaControllerRunsInHostProjectDir(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner([]string{bin}, boardDir)
	projectDir := t.TempDir()
	const managedGuidance = "managed target repository guidance"
	if err := os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), []byte(managedGuidance), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := spawn.WorkerSpec{
		Issue: "CLP-1", Lane: "coder", RunID: "run-1", ThreadID: "thread-1",
		Workspace: "/home/daytona/workspace/clipse", ProjectDir: projectDir, Backend: "daytona",
		Env: append(os.Environ(), "TESTWORKER_SCENARIO=agents"),
	}

	handle, err := s.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := handle.Wait()
	if err != nil || res.Err != nil {
		t.Fatalf("Wait = (%+v, %v)", res, err)
	}
	if res.Worker.Summary != managedGuidance {
		t.Fatalf("worker guidance = %q, want managed repo AGENTS.md", res.Worker.Summary)
	}
}

// TestLocalSpawner_MultiElementCommand asserts a configured command PREFIX
// longer than one element (e.g. the ["uv", "--project", ..., "run",
// "clipse-worker"] shape config.Worker.Command documents) execs correctly:
// the Spawner runs command[0] with command[1:] threaded ahead of the
// per-spec flags, not just command[0] alone. "env" re-execs its arguments
// unchanged, so env+bin here behaves exactly like bin alone would, proving
// the prefix is passed through rather than dropped or misordered.
func TestLocalSpawner_MultiElementCommand(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner([]string{"env", bin}, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := newSpec(t, boardDir, "done")

	handle, err := s.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}

	res, err := handle.Wait()
	if err != nil {
		t.Fatalf("Wait: unexpected top-level error: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("Result.Err = %v, want nil", res.Err)
	}
	if res.Worker.Outcome != contract.WorkerResultOutcomeDone {
		t.Errorf("Outcome = %q, want done", res.Worker.Outcome)
	}
	if res.Worker.IssueId != "CLP-1" {
		t.Errorf("IssueId = %q, want CLP-1", res.Worker.IssueId)
	}
}

// TestLocalSpawner_EmptyCommandErrors asserts Spawn fails fast with a clear
// error rather than panicking when no command is configured (defense in
// depth: config.Load's validation is what normally prevents this in
// production — see config.Worker.Command).
func TestLocalSpawner_EmptyCommandErrors(t *testing.T) {
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner(nil, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := newSpec(t, boardDir, "done")

	if _, err := s.Spawn(ctx, spec); err == nil {
		t.Fatal("Spawn: expected error for empty command, got nil")
	}
}

// TestLocalSpawner_CrashNonzeroExit asserts a nonzero-exit worker (no valid
// JSON on stdout) surfaces as a Result carrying an error, without Wait
// itself returning a top-level error (the dispatcher maps this to Blocked).
func TestLocalSpawner_CrashNonzeroExit(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner([]string{bin}, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := newSpec(t, boardDir, "crash")

	handle, err := s.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}

	res, err := handle.Wait()
	if err != nil {
		t.Fatalf("Wait: unexpected top-level error: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want nonzero")
	}
	if res.Err == nil {
		t.Fatalf("Result.Err = nil, want an error for nonzero exit")
	}
	if !errors.Is(res.Err, spawn.ErrWorkerExit) {
		t.Errorf("Result.Err = %v, want it to wrap ErrWorkerExit", res.Err)
	}
}

// TestLocalSpawner_Timeout asserts a hanging worker is killed once the
// context deadline passes, and Wait returns a timeout Result within bounded
// time rather than hanging the test.
func TestLocalSpawner_Timeout(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner([]string{bin}, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	spec := newSpec(t, boardDir, "hang")

	handle, err := s.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}

	done := make(chan struct{})
	var res spawn.Result
	var waitErr error
	go func() {
		res, waitErr = handle.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("Wait did not return within bounded time after context deadline")
	}

	if waitErr != nil {
		t.Fatalf("Wait: unexpected top-level error: %v", waitErr)
	}
	if res.Err == nil {
		t.Fatalf("Result.Err = nil, want a timeout error")
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Errorf("Result.Err = %v, want it to wrap context.DeadlineExceeded", res.Err)
	}
}

// TestLocalSpawner_TimeoutKillsPipeHoldingProcessGroup reproduces the
// SPA-1012 shutdown hang: CommandContext kills the launcher, but cmd.Wait
// cannot return while a surviving child still owns the stdout/stderr pipes.
// The deadline path must kill the complete process group before waiting on
// those pipes.
func TestLocalSpawner_TimeoutKillsPipeHoldingProcessGroup(t *testing.T) {
	boardDir := t.TempDir()
	command := []string{
		"/bin/sh", "-c",
		`trap '' TERM; (trap '' TERM; while :; do sleep 1; done) & wait`,
	}
	s := spawn.NewLocalSpawner(command, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	handle, err := s.Spawn(ctx, newSpec(t, boardDir, "unused"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	done := make(chan struct{})
	var res spawn.Result
	var waitErr error
	go func() {
		res, waitErr = handle.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait remained blocked by a child that inherited worker pipes")
	}
	if waitErr != nil {
		t.Fatalf("Wait: %v", waitErr)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Result.Err = %v, want context deadline exceeded", res.Err)
	}
	if err := syscall.Kill(-handle.PID(), 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("worker process group %d still exists after Wait: %v", handle.PID(), err)
	}
}

// TestLocalSpawner_Malformed asserts a clean-exit worker that writes
// non-JSON (or schema-invalid JSON) to stdout surfaces as a Result carrying
// a parse error.
func TestLocalSpawner_Malformed(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner([]string{bin}, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := newSpec(t, boardDir, "malformed")

	handle, err := s.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}

	res, err := handle.Wait()
	if err != nil {
		t.Fatalf("Wait: unexpected top-level error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (worker exits clean but writes bad JSON)", res.ExitCode)
	}
	if res.Err == nil {
		t.Fatalf("Result.Err = nil, want a malformed-result error")
	}
	if !errors.Is(res.Err, spawn.ErrMalformedResult) {
		t.Errorf("Result.Err = %v, want it to wrap ErrMalformedResult", res.Err)
	}
}

// assertStderrLog asserts the LocalSpawner redirected the worker's stderr to
// <boardDir>/logs/<issue>.log.
func assertStderrLog(t *testing.T, boardDir, issue string) {
	t.Helper()
	logPath := filepath.Join(boardDir, "logs", issue+".log")
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("stderr log %s: %v", logPath, err)
	}
}
