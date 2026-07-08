package dispatcher

import (
	"context"
	"encoding/json"
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
		// Either way the store's claim on runID is being cleared right now, so
		// any stale d.inflight[runID] left over from a "continue" respawn
		// (this is the SAME runID as the turn that just finished -- see
		// applyContinue) must go with it: otherwise the next tick's Heartbeat
		// finds no active claim for a run this dispatcher still thinks is
		// inflight, errors, and aborts reconcile before promote/claim/outbox
		// ever run again. A no-op for a fresh claim's first spawn
		// (spawnClaim), which never has a pre-existing entry for runID.
		delete(d.inflight, runID)
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

	// The coder reads its blockers' and its own Linear comments at claim time
	// (the read side of the handoff loop). Coder-lane only -- reviewers get the
	// PR diff, not the thread. Best-effort: dependencyNotes returns "" on any
	// fetch failure, so a slow Linear never fails the spawn.
	if lane == string(contract.LaneCoder) {
		if notes := d.dependencyNotes(ctx, issue); notes != "" {
			env = append(env, clipseDependencyNotesEnvVar+"="+notes)
		}
	}

	model, docsModel := d.modelsFor(lane)
	modelParams, docsModelParams := d.modelParamsFor(lane)
	shellAllowList, docsShellAllowList := d.shellFor(lane)
	spec := spawn.WorkerSpec{
		Issue:              issue.Identifier,
		Lane:               lane,
		RunID:              runID,
		ThreadID:           threadID,
		Workspace:          workspace,
		Env:                env,
		CheckpointDB:       d.checkpointDBPath(issue),
		MaxTokens:          d.cfg.MaxTokensPerRun,
		Model:              model,
		DocsModel:          docsModel,
		ModelParams:        modelParams,
		DocsModelParams:    docsModelParams,
		ShellAllowList:     shellAllowList,
		DocsShellAllowList: docsShellAllowList,
		BaseBranch:         d.cfg.Repo.BaseBranch,
		TranscriptPath:     d.transcriptPath(issue),
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
		// See the matching delete() in the Ensure error branch above: this
		// spawn attempt may be a "continue" respawn reusing runID, and its
		// failure clears the store claim, so the stale inflight record must
		// not survive it either.
		delete(d.inflight, runID)
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
func (d *Dispatcher) checkpointDBPath(issue store.Issue) string {
	if d.cfg.CheckpointsDir == "" {
		return ""
	}
	return filepath.Join(d.cfg.CheckpointsDir, issue.Identifier+".db")
}

// transcriptPath returns the per-issue agent transcript JSONL path the
// worker should append every DAC turn/tool event to, derived from
// cfg.BoardDir and the issue's Linear identifier -- one file per issue,
// living next to the per-issue stderr log LocalSpawner already writes to
// <board_dir>/logs/<issue>.log (see internal/spawn/local.go's
// stderrLogPath), so every turn/lane/rework this issue ever runs
// accumulates into the SAME file (AGENTS.md's transcript bullet). Returns
// "" when BoardDir is unset -- mirrors checkpointDBPath's own "no directory
// to root a path under" fallback for hand-built Configs that bypass
// config.Load (most dispatcher tests); LocalSpawner only appends
// --transcript when this is non-empty (see internal/spawn.workerArgs). Real
// production configs always have a non-empty BoardDir (config.Load defaults
// it), so the transcript is always-on there.
func (d *Dispatcher) transcriptPath(issue store.Issue) string {
	if d.cfg.BoardDir == "" {
		return ""
	}
	return filepath.Join(d.cfg.BoardDir, "logs", issue.Identifier+".transcript.jsonl")
}

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

// modelParamsFor resolves the opaque per-lane model-construction kwargs a
// spawned lane's worker should get from cfg.ModelParams (config.ModelParams's
// doc comment: DAC create_model extra_kwargs like a codex reasoning effort or
// an anthropic extended-thinking budget), JSON-encoding each map for
// WorkerSpec.ModelParams/.DocsModelParams. It mirrors modelsFor's lane
// mapping exactly — coder gets both Coder and CoderDocs, reviewer gets only
// Reviewer, everything else gets neither — since the docs sub-step only ever
// runs inside the Coder graph (AGENTS.md: "Documentation is a coder-graph
// step, not a lane"). A nil or empty map encodes as "" rather than "{}" or
// "null", so internal/spawn.LocalSpawner's workerArgs omits
// --model-params/--docs-model-params entirely for an unconfigured lane
// instead of handing the worker an empty JSON object to parse.
func (d *Dispatcher) modelParamsFor(lane string) (params, docsParams string) {
	enc := func(m map[string]any) string {
		if len(m) == 0 {
			return ""
		}
		b, err := json.Marshal(m)
		if err != nil {
			// A decoded YAML map failing to round-trip through
			// encoding/json would be a stdlib-defying surprise, but params
			// are a best-effort passthrough (config.ModelParams's doc
			// comment): never fail the spawn over it, just log and fall
			// back to "no overrides" for this lane.
			d.logger.Warn("marshal model_params failed", "lane", lane, "error", err)
			return ""
		}
		return string(b)
	}
	switch lane {
	case string(contract.LaneCoder):
		return enc(d.cfg.ModelParams.Coder), enc(d.cfg.ModelParams.CoderDocs)
	case string(contract.LaneReviewer):
		return enc(d.cfg.ModelParams.Reviewer), ""
	default:
		return "", ""
	}
}

// shellFor resolves the per-lane shell allow-list policy (config.ShellPolicy)
// from cfg.Shell into the JSON-encoded form WorkerSpec.ShellAllowList/
// .DocsShellAllowList expect: "" for an All policy OR an unconfigured lane
// (the worker's own default is unrestricted, so the flag is simply omitted —
// see internal/spawn.workerArgs), or a compact JSON array of command names
// for a restrictive list. Keyed off len(Commands) rather than the All flag
// (mirroring modelParamsFor's enc, which keys off len(m) rather than a
// presence bool): config.Load's defaultShellPolicy guarantees a real
// deployment's cfg.Shell is always fully resolved to one of {All:true,
// Commands:nil} or {All:false, Commands:non-empty} (see
// validateShellPolicy), but a hand-built config.Config that bypasses Load
// (as most dispatcher tests do) leaves an unset lane at ShellPolicy's Go zero
// value {All:false, Commands:nil} — len-keying treats that the same as All,
// instead of marshaling a nil slice to the literal string "null" and handing
// the worker a flag value it can't use. It mirrors modelParamsFor's lane
// mapping exactly (coder gets both Coder and CoderDocs, reviewer gets only
// Reviewer, everything else gets neither), for the same reason: the docs
// sub-step only ever runs inside the Coder graph.
//
// Unlike modelParamsFor's marshal-failure fallback (silently "no overrides",
// logged at Warn — a lane that was already going to inherit the provider
// default), a marshal failure here falls back to "" which the worker reads
// as All/unrestricted: the OPPOSITE of an operator's configured restriction.
// That is logged at Error, not Warn, so a restrictive shell policy silently
// turning permissive is loud, not quiet.
func (d *Dispatcher) shellFor(lane string) (shell, docsShell string) {
	enc := func(commands []string) string {
		if len(commands) == 0 {
			return ""
		}
		b, err := json.Marshal(commands)
		if err != nil {
			d.logger.Error("marshal shell_allow_list failed, falling back to unrestricted", "lane", lane, "error", err)
			return ""
		}
		return string(b)
	}
	switch lane {
	case string(contract.LaneCoder):
		return enc(d.cfg.Shell.Coder.Commands), enc(d.cfg.Shell.CoderDocs.Commands)
	case string(contract.LaneReviewer):
		return enc(d.cfg.Shell.Reviewer.Commands), ""
	default:
		return "", ""
	}
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
