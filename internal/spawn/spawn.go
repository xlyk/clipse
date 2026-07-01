// Package spawn defines the Spawner interface and its local subprocess
// implementation. The dispatcher is the only caller: it builds a WorkerSpec
// per claimed issue, calls Spawn, and later calls Wait on the returned
// RunHandle to get the worker's typed result (see internal/contract) and
// map it through internal/board.
//
// Spawner is a seam, not just an implementation detail: v1 ships only
// LocalSpawner (in-process exec.Command), but the design doc reserves a
// remote/SSH host-pool Spawner for v2 behind this same interface (see
// docs/design/2026-07-01-clipse-design.md, "Remote/multi-host workers").
package spawn

import (
	"context"
	"errors"

	"github.com/xlyk/clipse/internal/contract"
)

// Sentinel errors Wait can wrap in Result.Err, so callers (the dispatcher)
// can distinguish worker-failure modes with errors.Is rather than string
// matching. All three represent *expected* worker-failure outcomes the
// dispatcher maps to Blocked — Wait itself returns a nil top-level error for
// each of these; a non-nil top-level error from Wait means something went
// wrong in the Spawner/RunHandle machinery itself (e.g. failing to read the
// process's exit status), not in the worker.
var (
	// ErrWorkerExit means the worker process exited with a nonzero status.
	ErrWorkerExit = errors.New("spawn: worker exited nonzero")

	// ErrMalformedResult means the worker exited cleanly but its stdout was
	// not schema-valid WorkerResult JSON (absent, partial, or invalid).
	ErrMalformedResult = errors.New("spawn: worker result malformed")
)

// WorkerSpec describes one worker invocation: the issue/lane/run/thread
// identifiers the worker binary receives as flags, the worktree it runs in,
// and its environment. Env is the full, explicit environment the process
// gets (exec.Cmd.Env semantics) — the dispatcher (or a future lane-scoped
// allow-list, see Phase 2's env-scrubbing note) decides what's in it.
type WorkerSpec struct {
	Issue     string
	Lane      string
	RunID     string
	ThreadID  string
	Workspace string
	Env       []string
}

// Spawner starts a worker process for spec and returns a handle to observe
// and control it. Implementations must make Spawn safe to call
// concurrently for distinct specs.
type Spawner interface {
	Spawn(ctx context.Context, spec WorkerSpec) (RunHandle, error)
}

// RunHandle observes and controls one spawned worker process.
type RunHandle interface {
	// PID is the OS process id of the worker's process-group leader.
	PID() int

	// ProcStartedAt is the worker process's start time as a unix timestamp
	// (second granularity), used later to distinguish a live PID from an
	// unrelated process the OS has since reused the PID for (A1's
	// PID-reuse guard). 0 means unverifiable — see processStartTime.
	ProcStartedAt() int64

	// Kill terminates the worker's entire process group (leader plus any
	// children it spawned), not just the leader pid.
	Kill() error

	// Wait blocks until the worker process exits (or ctx's deadline fires
	// and Kill is invoked internally), then returns the parsed Result.
	//
	// Wait's own (top-level) error return is reserved for failures in the
	// Spawner/RunHandle machinery itself (e.g. os/exec plumbing errors
	// unrelated to the worker's own behavior). Every *expected*
	// worker-failure mode — nonzero exit, malformed/absent stdout JSON, or
	// a context-deadline kill — is reported through Result.Err (wrapping
	// ErrWorkerExit, ErrMalformedResult, or context.DeadlineExceeded)
	// while Wait itself returns a nil top-level error, so the dispatcher
	// can uniformly map those cases to Blocked.
	Wait() (Result, error)
}

// Result is what a worker turn produced: its parsed typed result (zero
// value if parsing failed), the process's exit code, and an error
// describing any failure mode (nil on clean success).
type Result struct {
	Worker   contract.WorkerResult
	ExitCode int
	Err      error
}
