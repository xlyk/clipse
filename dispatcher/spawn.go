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
// reviewFeedback, when non-empty, is the most recent review/rework feedback
// for a Coder re-run claimed out of the rework column; it is injected into
// the worker's environment as CLIPSE_REVIEW_FEEDBACK (see that constant) so
// the Coder lane can address it. Every other spawn (a fresh coder claim from
// ready, reviewer, a continuation) passes "" and injects nothing.
//
// On a Spawn failure (workspace or exec-level, not a worker-process
// failure), the issue is transitioned straight to blocked: there is no
// process to Wait on, so this can't flow through the normal
// applyResult/runResult path.
func (d *Dispatcher) spawnAttempt(ctx context.Context, issue store.Issue, runID, lane, threadID string, turn int, reviewFeedback string) error {
	// Every spawned lane (coder/reviewer) operates on the issue's own branch.
	workspace, err := d.ws.Ensure(issue)
	if err != nil {
		// A workspace/spawn failure is transient by nature, so it is eligible
		// for bounded auto-retry (auto-unblock layer 1); parkOrRetry falls back
		// to blockOnSpawnFailure once the budget is spent (or RecoverCap is 0).
		cause := fmt.Errorf("preparing workspace: %w", err)
		return d.parkOrRetry(ctx, issue, runID, lane, cause.Error(), contract.BlockKindTransient, d.now(), retryPayload{}, func() error {
			return d.blockOnSpawnFailure(ctx, issue.ID, runID, lane, cause)
		})
	}

	// d.envFor is always set (New defaults it to defaultEnvFor;
	// WithEnvFor overrides it) — never nil, so this never falls back to a
	// nil WorkerSpec.Env, which exec.Cmd.Env would treat as "inherit the
	// dispatcher's full environment".
	env := d.envFor(issue)

	// A rework re-run carries the review feedback that routed the card back to
	// the Coder lane. Injected here (not via envFor) so it rides alongside
	// CLIPSE_ISSUE_TEXT without ever touching the host-env allow-list, and so
	// only the rework path — the only caller that passes it non-empty — pays
	// for the store read that produced it.
	if reviewFeedback != "" {
		env = append(env, clipseReviewFeedbackEnvVar+"="+reviewFeedback)
	}

	model, docsModel := d.modelsFor(lane)
	spec := spawn.WorkerSpec{
		Issue:        issue.Identifier,
		Lane:         lane,
		RunID:        runID,
		ThreadID:     threadID,
		Workspace:    workspace,
		Env:          env,
		CheckpointDB: d.checkpointDBPath(issue),
		MaxTokens:    d.cfg.MaxTokensPerRun,
		Model:        model,
		DocsModel:    docsModel,
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
		cause := fmt.Errorf("spawning worker: %w", err)
		return d.parkOrRetry(ctx, issue, runID, lane, cause.Error(), contract.BlockKindTransient, d.now(), retryPayload{}, func() error {
			return d.blockOnSpawnFailure(ctx, issue.ID, runID, lane, cause)
		})
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
// modelsFor resolves the "provider:model" spec(s) a spawned lane's worker
// should get from cfg.Models, keyed purely by lane — the same resolution
// applies whether spawnAttempt was invoked from a fresh claim (spawnClaim) or
// a turn-cap continuation (applyContinue), since a run's lane never changes
// across its own turns. Only the Coder lane runs the docs sub-step
// (graphs/coder.py's run_docs node), so every other lane's docsModel is
// always "". A lane with no configured model (e.g. git_operator, which never
// spawns a DAC worker at all) resolves to ("", "").
func (d *Dispatcher) modelsFor(lane string) (model, docsModel string) {
	switch lane {
	case string(contract.LaneCoder):
		return d.cfg.Models.Coder, d.cfg.Models.CoderDocs
	case string(contract.LaneReviewer):
		return d.cfg.Models.Reviewer, ""
	default:
		return "", ""
	}
}

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
