package spawn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/xlyk/clipse/internal/contract"
)

// LocalSpawner spawns workers as local subprocesses via exec.CommandContext.
// It is the only Spawner implementation in v1 (see the design doc's Spawner
// seam note); a remote/SSH host-pool Spawner is reserved for v2.
type LocalSpawner struct {
	command  []string
	boardDir string
}

// NewLocalSpawner returns a LocalSpawner that execs command (an argv PREFIX,
// e.g. ["uv", "--project", "/abs/path/agent", "run", "clipse-worker"] for
// the real worker, or a single-element []string{binaryPath} for testworker)
// for every worker, appending workerArgs(spec) after it and redirecting each
// worker's stderr to <boardDir>/logs/<issue>.log. command must be non-empty;
// config.Load's validation is what normally guarantees that in production
// (see config.Worker.Command).
func NewLocalSpawner(command []string, boardDir string) *LocalSpawner {
	return &LocalSpawner{command: command, boardDir: boardDir}
}

// workerArgs returns the ordered CLI flags every worker invocation carries:
// the five fields every worker invocation carries, followed by
// --checkpoint-db, --max-tokens, --model, --docs-model, --model-params,
// --docs-model-params, and --base-branch ONLY when spec carries them
// (CheckpointDB non-empty / MaxTokens > 0 / Model non-empty / DocsModel
// non-empty / ModelParams non-empty / DocsModelParams non-empty / BaseBranch
// non-empty — see WorkerSpec's doc comment).
// Kept as a pure helper (tested directly in argv_test.go) so this
// conditional-append logic doesn't need a real subprocess to exercise, and
// so a worker that has none of these configured (e.g. testworker, driven by
// hand-built WorkerSpecs in kernel tests) never sees a flag it doesn't
// understand.
func workerArgs(spec WorkerSpec) []string {
	args := []string{
		"--issue=" + spec.Issue,
		"--lane=" + spec.Lane,
		"--run=" + spec.RunID,
		"--thread=" + spec.ThreadID,
		"--workspace=" + spec.Workspace,
	}
	if spec.CheckpointDB != "" {
		args = append(args, "--checkpoint-db="+spec.CheckpointDB)
	}
	if spec.MaxTokens > 0 {
		args = append(args, fmt.Sprintf("--max-tokens=%d", spec.MaxTokens))
	}
	if spec.Model != "" {
		args = append(args, "--model="+spec.Model)
	}
	if spec.DocsModel != "" {
		args = append(args, "--docs-model="+spec.DocsModel)
	}
	if spec.ModelParams != "" {
		args = append(args, "--model-params="+spec.ModelParams)
	}
	if spec.DocsModelParams != "" {
		args = append(args, "--docs-model-params="+spec.DocsModelParams)
	}
	if spec.BaseBranch != "" {
		args = append(args, "--base-branch="+spec.BaseBranch)
	}
	return args
}

// Spawn starts s.command (its first element as the program, the rest as a
// fixed prefix) with workerArgs(spec) appended, running it as the leader of
// its own process group (so Kill and the max_runtime deadline can reap the
// whole group, not just the leader). ctx's deadline governs the worker's
// max_runtime: the caller sets it, and once it fires the process group is
// killed and Wait returns a timeout Result.
func (s *LocalSpawner) Spawn(ctx context.Context, spec WorkerSpec) (RunHandle, error) {
	if len(s.command) == 0 {
		return nil, fmt.Errorf("spawning worker: no command configured")
	}

	logPath, err := s.stderrLogPath(spec.Issue)
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening worker stderr log %s: %w", logPath, err)
	}

	// Copy s.command[1:] into a fresh slice before appending workerArgs:
	// Spawn must be safe for concurrent calls sharing the same s.command,
	// and appending onto a slice that reslices s.command directly could
	// otherwise race on (or corrupt) its backing array.
	name := s.command[0]
	args := append(append([]string{}, s.command[1:]...), workerArgs(spec)...)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = spec.Env
	// Run the worker in its own issue worktree, not the dispatcher's cwd. DAC
	// (create_cli_agent) resolves the injected memory/AGENTS.md guides from
	// settings.project_root, which is bootstrapped from the worker process's
	// working directory (Path.cwd()) -- NOT from the cwd= argument
	// dac.build_coder_agent passes (that only sets the filesystem/shell backend
	// root and prompt text). Leaving Dir unset made every coder round carry the
	// dispatcher repo's own AGENTS.md instead of the target repo's guides
	// (Reflex retro). Empty Workspace leaves Dir at the caller's cwd (exec.Cmd
	// semantics), matching the pre-fix behavior for specs that carry no
	// worktree.
	cmd.Dir = spec.Workspace
	cmd.Stderr = logFile

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	// Run the worker as its own process-group leader so Kill (and the
	// context-deadline kill exec.CommandContext performs on the leader
	// alone) can be extended to signal the whole group, reaping any
	// children the worker spawns.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("starting worker %s: %w", name, err)
	}

	startedAt, psErr := processStartTime(cmd.Process.Pid)
	if psErr != nil {
		// Best-effort: leave ProcStartedAt at 0 ("unverifiable") rather than
		// failing the spawn outright.
		startedAt = 0
	}

	h := &localHandle{
		cmd:       cmd,
		logFile:   logFile,
		pid:       cmd.Process.Pid,
		startedAt: startedAt,
		ctx:       ctx,
	}
	return h, nil
}

// stderrLogPath ensures <boardDir>/logs exists and returns the per-issue log
// path within it.
func (s *LocalSpawner) stderrLogPath(issue string) (string, error) {
	logsDir := filepath.Join(s.boardDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return "", fmt.Errorf("creating logs dir %s: %w", logsDir, err)
	}
	return filepath.Join(logsDir, issue+".log"), nil
}

// localHandle is the LocalSpawner's RunHandle implementation.
type localHandle struct {
	cmd       *exec.Cmd
	logFile   *os.File
	pid       int
	startedAt int64
	ctx       context.Context

	mu       sync.Mutex
	killed   bool
	waitOnce sync.Once
	waitRes  Result
	waitErr  error
}

func (h *localHandle) PID() int { return h.pid }

func (h *localHandle) ProcStartedAt() int64 { return h.startedAt }

// Kill signals the worker's entire process group with SIGTERM, then
// SIGKILL, rather than just the leader pid: Setpgid:true at Start made the
// leader the group leader, so -pgid addresses the whole group, catching any
// children the worker spawned.
func (h *localHandle) Kill() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.killed {
		return nil
	}
	h.killed = true

	pgid := h.pid
	// Best-effort graceful termination first; ignore errors here since the
	// process may already be gone or may ignore SIGTERM, either of which
	// the follow-up SIGKILL handles.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// Process group is already gone: not an error.
			return nil
		}
		return fmt.Errorf("killing worker process group %d: %w", pgid, err)
	}
	return nil
}

// Wait blocks until the process exits or ctx's deadline fires (in which
// case it kills the process group and reports a timeout), then parses
// stdout into a Result. It is safe to call more than once; the underlying
// wait/parse work runs exactly once.
func (h *localHandle) Wait() (Result, error) {
	h.waitOnce.Do(func() {
		h.waitRes, h.waitErr = h.wait()
	})
	return h.waitRes, h.waitErr
}

func (h *localHandle) wait() (Result, error) {
	defer func() { _ = h.logFile.Close() }()

	runErr := h.cmd.Wait()

	// exec.CommandContext kills the leader pid (not the group) itself once
	// ctx's deadline passes, so cmd.Wait() above can return promptly even
	// though other children in the group are still alive. Kill the whole
	// group ourselves whenever the deadline has fired, whether or not
	// cmd.Wait() already returned — this both handles the leader (Kill is
	// idempotent) and reaps stray children.
	if h.ctx.Err() != nil {
		_ = h.Kill()
		return Result{
			ExitCode: h.exitCode(runErr),
			Err:      fmt.Errorf("worker killed after context deadline: %w", h.ctx.Err()),
		}, nil
	}

	exitCode := h.exitCode(runErr)
	if runErr != nil {
		return Result{
			ExitCode: exitCode,
			Err:      fmt.Errorf("%w: exit code %d: %w", ErrWorkerExit, exitCode, runErr),
		}, nil
	}

	var worker contract.WorkerResult
	stdout := h.stdoutBytes()
	if err := json.Unmarshal(stdout, &worker); err != nil {
		return Result{
			ExitCode: exitCode,
			Err:      fmt.Errorf("%w: %w", ErrMalformedResult, err),
		}, nil
	}

	return Result{Worker: worker, ExitCode: exitCode}, nil
}

// exitCode extracts the process exit code from cmd.Wait()'s error (nil
// meaning exit 0), falling back to -1 for failure modes that have no exit
// code (e.g. the process was killed by a signal before exiting normally).
func (h *localHandle) exitCode(runErr error) int {
	if runErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func (h *localHandle) stdoutBytes() []byte {
	if buf, ok := h.cmd.Stdout.(*bytes.Buffer); ok {
		return buf.Bytes()
	}
	return nil
}
