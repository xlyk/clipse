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
// gets (exec.Cmd.Env semantics) — the caller decides what's in it. The
// dispatcher's default builds Env from an explicit allow-list
// (config.Config.EnvAllowlist, filtered via AllowlistedEnv) rather than its
// own os.Environ(), so a worker never inherits secrets like LINEAR_API_KEY
// that aren't explicitly allow-listed (design doc threat model, B3).
type WorkerSpec struct {
	Issue     string
	Lane      string
	RunID     string
	ThreadID  string
	Workspace string
	Env       []string

	// CheckpointDB is the absolute path to this issue's LangGraph
	// checkpointer SQLite database (design doc: one file per issue, outside
	// any git worktree — see dispatcher.checkpointDBPath). The kernel owns
	// this path, not the worker (Phase 2 plan item: "the kernel owns paths,
	// not worker-side convention"). Optional: LocalSpawner appends
	// --checkpoint-db=<value> only when this is non-empty, so callers that
	// have no checkpoints directory configured (e.g. hand-built test
	// Configs that bypass config.Load) simply omit the flag.
	CheckpointDB string

	// MaxTokens is the per-run token ceiling (config.Config.MaxTokensPerRun)
	// the worker enforces against its own DAC-callback usage tracking.
	// Optional: LocalSpawner appends --max-tokens=<value> only when this is
	// > 0.
	MaxTokens int

	// Model is "provider:model" for this lane's DAC agent (empty = worker
	// default). Optional: LocalSpawner appends --model=<value> only when
	// this is non-empty.
	Model string

	// DocsModel is "provider:model" for the coder graph's docs sub-step
	// (empty = worker default). Optional: LocalSpawner appends
	// --docs-model=<value> only when this is non-empty.
	DocsModel string

	// ModelParams is a JSON object of extra model-construction kwargs for
	// this lane's DAC agent (empty = none). Optional: LocalSpawner appends
	// --model-params=<value> only when this is non-empty.
	ModelParams string

	// DocsModelParams is a JSON object of extra model-construction kwargs
	// for the coder graph's docs sub-step (empty = none). Optional:
	// LocalSpawner appends --docs-model-params=<value> only when this is
	// non-empty.
	DocsModelParams string

	// ShellAllowList is a JSON array of allowed command names for the
	// lane's DAC shell ("" means unrestricted — the worker defaults to the
	// `all` policy and the flag is omitted; see config.ShellPolicy).
	// Optional: LocalSpawner appends --shell-allow-list=<value> only when
	// this is non-empty.
	ShellAllowList string

	// DocsShellAllowList is the docs sub-step's policy, coder lane only
	// (empty = unrestricted / omitted, same as ShellAllowList). Optional:
	// LocalSpawner appends --docs-shell-allow-list=<value> only when this is
	// non-empty.
	DocsShellAllowList string

	// BaseBranch is the repo base branch (config.Repo.BaseBranch) the coder
	// syncs its worktree to each turn, e.g. `git merge origin/<base>` (empty
	// = omitted). Set for every lane; harmless for Reviewer/git-operator
	// workers, which never sync a worktree. Optional: LocalSpawner appends
	// --base-branch=<value> only when this is non-empty.
	BaseBranch string
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
