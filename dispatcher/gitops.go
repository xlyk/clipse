package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/gitops"
	"github.com/xlyk/clipse/internal/store"
)

// GitOpsRunner executes one deterministic pass of the Git-operator lane for
// spec (see Dispatcher.gitOps). Production uses defaultGitOpsRunner; tests
// substitute a fake that returns scripted gitops.Result values.
type GitOpsRunner func(ctx context.Context, spec gitops.Spec) (gitops.Result, error)

// defaultGitOpsRunner is New's default GitOpsRunner: a real gitops.Run call
// against the default (real subprocess) CommandRunner.
func defaultGitOpsRunner(ctx context.Context, spec gitops.Spec) (gitops.Result, error) {
	return gitops.Run(ctx, spec, nil)
}

// GitOpsPreChecker performs the READ-ONLY PR pre-check runGitopsClaim runs
// from the primary clone BEFORE ensuring a worktree (see
// Dispatcher.gitOpsPreCheck). resolved reports whether the pre-check already
// decided the pass (a merged or missing PR); when false the caller ensures the
// worktree and runs the full gitops pass. Production uses
// defaultGitOpsPreChecker; tests substitute a fake via WithGitOpsPreChecker so
// the merging path never touches a real gh subprocess.
type GitOpsPreChecker func(ctx context.Context, spec gitops.Spec) (result gitops.Result, resolved bool, err error)

// defaultGitOpsPreChecker is New's default GitOpsPreChecker: a real,
// read-only gitops.PreCheckPRState call against the default CommandRunner.
func defaultGitOpsPreChecker(ctx context.Context, spec gitops.Spec) (gitops.Result, bool, error) {
	return gitops.PreCheckPRState(ctx, spec, nil)
}

// mergingTTL is the claim TTL (seconds) used for a "merging" ClaimColumn
// claim: deliberately cfg.PollIntervalS, NOT d.ttl() (cfg.MaxRuntimeS) —
// R3. gitops runs synchronously and returns within one tick, so this claim
// never needs a worker-runtime-length lease; a SHORT ttl instead makes the
// claim's own natural expiry double as the CI re-check cadence for
// OutcomeCIPending (see applyGitopsResult): the claim goes stale by roughly
// the next poll, ReleaseStaleClaims frees it (leaving board_status
// "merging" unchanged — store.ReleaseTargetColumn), and claimAndRunGitops
// re-claims and re-checks it then.
func (d *Dispatcher) mergingTTL() int64 {
	return int64(d.cfg.PollIntervalS)
}

// claimAndRunGitops claims up to Caps.PerLane.GitOperator (and the global
// cap) UNCLAIMED cards sitting in "merging", running internal/gitops INLINE
// for each — no subprocess, no d.inflight entry, since gitops finishes
// synchronously within this call. Both caps are therefore enforced with a
// local counter/adjustment rather than d.inflightLaneCounts alone, which
// would never see a gitops-claimed run at all (it only reflects spawned
// subprocess workers).
func (d *Dispatcher) claimAndRunGitops(ctx context.Context, now int64) error {
	const gitOpLane = string(contract.LaneGitOperator)
	gitOpCap := d.cfg.Caps.PerLane.GitOperator
	claimed := 0

	for claimed < gitOpCap {
		global, _ := d.inflightLaneCounts()
		if global+claimed >= d.cfg.Caps.Global {
			return nil
		}

		claim, err := d.store.ClaimColumn(ctx, string(contract.ColumnMerging), gitOpLane, d.newRunID(), now, d.mergingTTL())
		if errors.Is(err, store.ErrNoReady) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claiming merging column: %w", err)
		}
		claimed++

		if err := d.runGitopsClaim(ctx, *claim); err != nil {
			return fmt.Errorf("running gitops for issue %s: %w", claim.Issue.ID, err)
		}
	}
	return nil
}

// runGitopsClaim first PRE-CHECKS claim's PR from the primary clone with a
// READ-ONLY d.gitOpsPreCheck (a single `gh pr view`, no merge/cleanup/probe)
// and short-circuits on a terminal verdict — a merged PR (board -> done) or a
// missing one (parked needs_input) — so a deleted branch/PR never resurrects a
// hand-deleted worktree just to fail on it forever (the Reflex retro's zombie
// runs). Only when the pre-check does NOT resolve the pass does it ensure the
// issue's worktree (the same one the Coder lane committed/pushed from) and run
// the full, side-effecting d.gitOps against it — which owns the real merge,
// cleanup, tag, and conflict-file probe, all of which need the PR-branch
// worktree, never the primary clone. The result maps onto a board transition
// (applyGitopsResult). An infrastructure failure — the read-only pre-check's
// gh call, preparing the workspace, or d.gitOps itself (gitops.Run's own error
// return is reserved for exactly this: gh/git plumbing failures, never a
// meaningful PR decision) — is logged and the claim is left in place: it
// expires per the short mergingTTL and the next poll's claimAndRunGitops
// retries it, exactly like an OutcomeCIPending result.
func (d *Dispatcher) runGitopsClaim(ctx context.Context, claim store.Claim) error {
	issue := claim.Issue

	// Read-only pre-check from the primary clone (no worktree): `gh pr view`
	// resolves the repo from its working directory, so the primary clone
	// answers it without ensuring the issue's worktree first. It short-circuits
	// only the two states a worktree can't help with — a merged or missing PR;
	// everything else falls through to the full worktree pass below.
	preSpec := gitops.Spec{
		Branch:           issue.BranchName,
		BaseBranch:       d.cfg.Repo.BaseBranch,
		Workspace:        d.cfg.Repo.Path,
		PrimaryClonePath: d.cfg.Repo.Path,
	}
	result, resolved, err := d.gitOpsPreCheck(ctx, preSpec)
	if err != nil {
		d.logger.Error("gitops pr pre-check failed, leaving merging claim in place", "issue_id", issue.ID, "run_id", claim.Run.RunID, "error", err)
		return nil
	}
	if resolved {
		return d.applyGitopsResult(ctx, issue, claim.Run.RunID, claim.Run.Lane, result)
	}

	workspace, err := d.ws.Ensure(issue)
	if err != nil {
		d.logger.Error("gitops workspace prep failed, leaving merging claim in place", "issue_id", issue.ID, "run_id", claim.Run.RunID, "error", err)
		return nil
	}

	spec := gitops.Spec{
		Branch:           issue.BranchName,
		BaseBranch:       d.cfg.Repo.BaseBranch,
		Workspace:        workspace,
		PrimaryClonePath: d.cfg.Repo.Path,
		RequireChecks:    d.cfg.Repo.RequireChecks,
		// Derive the squash subject from the issue, not the coder-narration
		// PR title (the sink half of the fix; the coder graph fixes the
		// source).
		IssueID:    issue.ID,
		IssueTitle: issue.Title,
	}

	result, err = d.gitOps(ctx, spec)
	if err != nil {
		d.logger.Error("gitops run failed, leaving merging claim in place", "issue_id", issue.ID, "run_id", claim.Run.RunID, "error", err)
		return nil
	}

	return d.applyGitopsResult(ctx, issue, claim.Run.RunID, claim.Run.Lane, result)
}

// applyGitopsResult maps one gitops.Result onto the same board-transition
// path a spawned worker's result uses (applyTerminalWorkerOutcome), by
// building the equivalent contract.WorkerResult:
//
//   - OutcomeMerged            -> outcome "done"               (merging -> done)
//   - OutcomeStaleBaseConflict -> outcome "changes_requested"   (merging -> rework, R1)
//   - OutcomeNotMergeable      -> outcome "blocked"              (merging -> blocked)
//   - OutcomeCIPending         -> NO transition at all (R3): the merging
//     claim is left exactly as ClaimColumn set it, and its short TTL
//     (mergingTTL) naturally expires by the next poll, at which point
//     ReleaseStaleClaims frees it (board_status unchanged) and
//     claimAndRunGitops re-claims and re-checks it.
func (d *Dispatcher) applyGitopsResult(ctx context.Context, issue store.Issue, runID, lane string, result gitops.Result) error {
	base := gitopsResultBase{issueID: issue.ID, runID: runID, lane: lane}
	switch result.Outcome {
	case gitops.OutcomeCIPending:
		return nil
	case gitops.OutcomeMerged:
		summary := fmt.Sprintf("merged PR #%d", result.PRNumber)
		return d.applyTerminalWorkerOutcome(ctx, issue, runID, lane, gitopsWorkerResult(base, result, contract.WorkerResultOutcomeDone, summary, nil))
	case gitops.OutcomeStaleBaseConflict:
		summary := staleBaseConflictSummary(result)
		return d.applyTerminalWorkerOutcome(ctx, issue, runID, lane, gitopsWorkerResult(base, result, contract.WorkerResultOutcomeChangesRequested, summary, nil))
	case gitops.OutcomeNotMergeable:
		// gitops declares retriability per not-mergeable reason (see
		// gitops.Result.Retriable): a deterministic verdict (failing required
		// checks) parks as needs_input rather than burning identical retries
		// (the Reflex build's 5-retries-per-ticket); a plausibly-transient one
		// (unsatisfied protection, an unrecognized merge refusal) stays
		// transient and eligible for bounded auto-retry.
		bk := contract.BlockKindNeedsInput
		if result.Retriable {
			bk = contract.BlockKindTransient
		}
		return d.applyTerminalWorkerOutcome(ctx, issue, runID, lane, gitopsWorkerResult(base, result, contract.WorkerResultOutcomeBlocked, result.Reason, &bk))
	default:
		return fmt.Errorf("gitops returned unknown outcome %q for issue %s", result.Outcome, issue.ID)
	}
}

// staleBaseConflictSummary renders gitops' Reason plus its ConflictingFiles
// list (dropped otherwise — contract.WorkerResult has no such field) into
// one summary string: internal/gitops never posts anything to the PR or
// Linear itself for this route (unlike the Reviewer lane, which posts its
// own inline PR comments before emitting changes_requested), so this
// summary is what applyTerminalWorkerOutcome turns into the Linear comment
// naming the conflicting files (design doc: "route to Rework with a comment
// naming the conflicting files").
func staleBaseConflictSummary(result gitops.Result) string {
	summary := result.Reason
	if len(result.ConflictingFiles) > 0 {
		summary += fmt.Sprintf(" (conflicting files: %s)", strings.Join(result.ConflictingFiles, ", "))
	}
	return summary
}

// gitopsResultBase carries the identifiers a spawned worker's own stdout
// JSON would fill in itself (run_id/issue_id/lane), which gitops.Result has
// no notion of — gitops never sees Linear issues or dispatcher runs (see
// internal/gitops's package doc). Threading them through keeps a
// gitops-driven runs.result_json row exactly as complete as a real
// worker's, rather than leaving these required-but-informational fields
// blank.
type gitopsResultBase struct {
	issueID string
	runID   string
	lane    string
}

// gitopsWorkerResult builds the contract.WorkerResult applyTerminalWorkerOutcome
// consumes, from a gitops.Result -- the shape a spawned worker's stdout JSON
// would otherwise carry, so the Git-operator lane's deterministic executor
// (decision J amendment / O) can flow through the exact same board-transition
// path as every other lane's typed result.
func gitopsWorkerResult(base gitopsResultBase, result gitops.Result, outcome contract.WorkerResultOutcome, summary string, blockKind *contract.BlockKind) contract.WorkerResult {
	wr := contract.WorkerResult{
		RunId:     base.runID,
		IssueId:   base.issueID,
		Lane:      contract.Lane(base.lane),
		Outcome:   outcome,
		Summary:   summary,
		Artifacts: []string{},
		Tokens:    contract.WorkerResultTokens{},
		TurnCount: 1,
		BlockKind: blockKind,
	}
	if result.PRURL != "" {
		url := result.PRURL
		wr.PrUrl = &url
	}
	return wr
}
