package dispatcher

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// spawnAttempt starts (or restarts, for a "continue" outcome) the worker
// process for runID against issue, records the process identity, tracks the
// run as inflight, and starts the single Wait-goroutine that reports the
// result back on d.results. turn is the turn number this attempt represents
// (used to seed/refresh the inflight record's turn counter).
//
// On a Spawn failure (workspace or exec-level, not a worker-process
// failure), the issue is transitioned straight to blocked: there is no
// process to Wait on, so this can't flow through the normal
// applyResult/runResult path.
func (d *Dispatcher) spawnAttempt(ctx context.Context, issue store.Issue, runID, lane, threadID string, turn int) error {
	// The Scribe lane needs its own docs worktree cut from origin/<base>, not
	// the issue's already-merged Coder branch (which fails non-fast-forward on
	// push once gitops has advanced its remote tip). Every other spawned lane
	// (coder/reviewer) operates on the issue's own branch via Ensure.
	ensure := d.ws.Ensure
	if lane == string(contract.LaneScribe) {
		ensure = d.ws.EnsureDocs
	}
	workspace, err := ensure(issue)
	if err != nil {
		return d.blockOnSpawnFailure(ctx, issue.ID, runID, lane, fmt.Errorf("preparing workspace: %w", err))
	}

	// d.envFor is always set (New defaults it to defaultEnvFor;
	// WithEnvFor overrides it) — never nil, so this never falls back to a
	// nil WorkerSpec.Env, which exec.Cmd.Env would treat as "inherit the
	// dispatcher's full environment".
	env := d.envFor(issue)

	spec := spawn.WorkerSpec{
		Issue:        issue.Identifier,
		Lane:         lane,
		RunID:        runID,
		ThreadID:     threadID,
		Workspace:    workspace,
		Env:          env,
		CheckpointDB: d.checkpointDBPath(issue),
		MaxTokens:    d.cfg.MaxTokensPerRun,
	}

	// Root the worker's timeout at a context that keeps ctx's values but
	// drops its cancellation (context.WithoutCancel), so a graceful
	// dispatcher shutdown (Run's ctx being cancelled) does not tear down a
	// live worker mid-commit/push. Only MaxRuntimeS — never shutdown — kills
	// a spawned worker; a worker still running when the process exits
	// becomes an orphan the next dispatcher's RecoverOrphans reaps.
	spawnCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(d.cfg.MaxRuntimeS)*time.Second)
	handle, err := d.spawner.Spawn(spawnCtx, spec)
	if err != nil {
		cancel()
		return d.blockOnSpawnFailure(ctx, issue.ID, runID, lane, fmt.Errorf("spawning worker: %w", err))
	}

	if err := d.store.SetRunProcess(ctx, runID, handle.PID(), handle.ProcStartedAt()); err != nil {
		// The process is already running; a failure to record its identity
		// is not itself fatal to the run, but must not be swallowed either.
		d.logger.Error("recording run process failed", "run_id", runID, "issue_id", issue.ID, "error", err)
	}

	d.inflight[runID] = inflightRun{
		handle:    handle,
		issueID:   issue.ID,
		lane:      lane,
		workspace: workspace,
		cancel:    cancel,
		turn:      turn,
	}

	go func() {
		res, _ := handle.Wait()
		d.results <- runResult{runID: runID, issueID: issue.ID, res: res}
	}()

	return nil
}

// checkpointDBPath returns the per-issue LangGraph checkpointer database
// path the worker should use, derived from cfg.CheckpointsDir and the
// issue's Linear identifier — the same identifier passed as --issue,
// matching the design doc's "<board>/checkpoints/<issue_id>.db" convention.
// It returns "" when CheckpointsDir is unset: hand-built Configs that
// bypass config.Load (as most dispatcher tests do) have no directory to
// root a path under, and LocalSpawner only appends --checkpoint-db when
// this is non-empty (see internal/spawn.workerArgs). Real production
// configs always have a non-empty CheckpointsDir, since config.Load
// defaults it.
func (d *Dispatcher) checkpointDBPath(issue store.Issue) string {
	if d.cfg.CheckpointsDir == "" {
		return ""
	}
	return filepath.Join(d.cfg.CheckpointsDir, issue.Identifier+".db")
}

// blockOnSpawnFailure transitions issue straight to blocked when the
// Spawner/Workspacer machinery itself fails (never got a process to Wait
// on), rather than going through applyResult.
func (d *Dispatcher) blockOnSpawnFailure(ctx context.Context, issueID, runID, lane string, cause error) error {
	now := d.now()
	req := store.TransitionReq{
		IssueID:         issueID,
		NewStatus:       "blocked",
		ClearClaim:      true,
		CloseRunID:      runID,
		RunStatus:       "blocked",
		RunError:        cause.Error(),
		EnqueueSetState: true,
		Comment:         blockedComment("", cause.Error()),
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issueID),
			RunID:   nullString(runID),
			Kind:    "blocked",
			Detail:  cause.Error(),
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("blocking issue %s after spawn failure: %w", issueID, err)
	}
	d.logger.Warn("worker spawn failed, issue blocked", "issue_id", issueID, "run_id", runID, "lane", lane, "error", cause)
	return nil
}
