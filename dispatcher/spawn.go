package dispatcher

import (
	"context"
	"fmt"
	"time"

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
	workspace, err := d.ws.Ensure(issue)
	if err != nil {
		return d.blockOnSpawnFailure(ctx, issue.ID, runID, lane, fmt.Errorf("preparing workspace: %w", err))
	}

	var env []string
	if d.envFor != nil {
		env = d.envFor(issue)
	}

	spec := spawn.WorkerSpec{
		Issue:     issue.Identifier,
		Lane:      lane,
		RunID:     runID,
		ThreadID:  threadID,
		Workspace: workspace,
		Env:       env,
	}

	spawnCtx, cancel := context.WithTimeout(ctx, time.Duration(d.cfg.MaxRuntimeS)*time.Second)
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
		Comment:         fmt.Sprintf("blocked: %s", cause.Error()),
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
