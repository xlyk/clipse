// Package gitops implements the Git-operator lane as deterministic Go
// (design doc decision J's amendment / decision O): merging a reviewed PR
// is pure CLI/API work with an exact, checkable success criterion, so it
// runs as kernel code the dispatcher calls inline for a claimed "merging"
// card, never as a spawned DAC worker. The "git_operator" lane label
// remains board semantics only.
//
// Run is the package's single entry point. It checks required CI checks
// and branch protection for spec's PR, merges only when both are
// satisfied, resolves a merely-stale (out-of-date) base automatically, tags
// the release when configured, and cleans up the worktree/branch on a
// successful merge. It never touches Linear, SQLite, or board.Next
// directly -- it returns a small Result the dispatcher maps onto the board
// state machine, the same way it already maps a spawned worker's
// contract.WorkerResult.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Outcome enumerates what one Run pass over a merging card's PR decided.
// Unlike a spawned worker's contract.WorkerResult (whose outcome enum has
// no "still waiting, nothing is wrong" state), gitops needs OutcomeCIPending
// as a distinct case from OutcomeNotMergeable: required checks still
// running (or a base-update still catching up) is not a failure, and the
// dispatcher must not transition the board for it at all (R3) -- it leaves
// the merging-column claim in place and lets the short claim TTL expire so
// the next poll re-checks.
type Outcome string

const (
	// OutcomeMerged means the PR merged cleanly. The dispatcher maps this
	// to a "done" outcome from the merging column (board.Next transitions
	// merging -> documentation). Run has already removed the worktree and
	// local branch, and created Spec.Tag if one was set, by the time this
	// is returned.
	OutcomeMerged Outcome = "merged"

	// OutcomeNotMergeable means required checks are failing or absent, or
	// branch protection is unsatisfied (or the merge attempt itself
	// failed). The dispatcher maps this to a "blocked" outcome from the
	// merging column (board.Next transitions merging -> blocked);
	// Result.Reason is the blocking comment.
	OutcomeNotMergeable Outcome = "not_mergeable"

	// OutcomeStaleBaseConflict means the PR was only blocked by an
	// out-of-date base; Run attempted `gh pr update-branch` and the
	// re-check still shows a real conflict. The dispatcher maps this to
	// the {changes_requested, merging} -> {rework, request_changes}
	// transition (R1); Result.ConflictingFiles names what to fix.
	OutcomeStaleBaseConflict Outcome = "stale_base_conflict"

	// OutcomeCIPending means required checks (or a base-update Run just
	// triggered) are still in progress, not failed. The dispatcher MUST
	// NOT call Transition/board.Next for this outcome (R3) -- there is
	// nothing to transition; the merging claim's short TTL expiring
	// naturally re-checks next poll. This is the whole reason gitops
	// returns a typed Result instead of a bool: OutcomeCIPending must never
	// be confused with OutcomeNotMergeable.
	OutcomeCIPending Outcome = "ci_pending"
)

// prStateMerged is gh/GitHub's own `state` enum value for a PR that has
// already been merged (the other members are "OPEN" and "CLOSED").
const prStateMerged = "MERGED"

// Spec describes one merging card's PR for Run to act on. The dispatcher
// resolves every field from its own issue/run state before calling Run;
// this package has no notion of Linear issues or SQLite runs.
type Spec struct {
	// Branch is the PR's head branch -- the same branch name Spawner
	// creates the issue's worktree/branch from (issues.branch_name).
	Branch string

	// BaseBranch is the PR's base branch (e.g. "main").
	BaseBranch string

	// Workspace is the issue's worktree directory: the working directory
	// for every git/gh invocation Run makes.
	Workspace string

	// PrimaryClonePath is the dispatcher's single managed repo clone,
	// forwarded to spawn.RemoveWorktree for the worktree-admin operations
	// on a successful merge (git worktree add/remove share this repo's
	// .git across every issue's worktree).
	PrimaryClonePath string

	// MergeMethod selects the `gh pr merge` strategy: "squash", "merge",
	// or "rebase". Empty defaults to "squash".
	MergeMethod string

	// Tag, when non-empty, is created (locally, via `git tag`) against the
	// branch tip once the merge succeeds. Empty skips tagging entirely --
	// it is genuinely optional, per the design doc's Git-operator
	// responsibilities.
	Tag string

	// RequireChecks controls the checksAbsent verdict: true (the safe
	// default -- CI that registers late must be WAITED for, not blocked on)
	// maps absent checks to OutcomeCIPending; false (a repo with no CI at
	// all) lets the merge proceed on branch protection alone. The dispatcher
	// sets it from cfg.Repo.RequireChecks (yaml repo.require_checks, default
	// true).
	RequireChecks bool
}

// validate reports a descriptive error for a Spec missing a field Run
// cannot proceed without. MergeMethod and Tag are intentionally excluded:
// both have well-defined empty-value defaults (squash, no tag).
func (s Spec) validate() error {
	switch {
	case s.Branch == "":
		return errors.New("gitops: Spec.Branch is required")
	case s.BaseBranch == "":
		return errors.New("gitops: Spec.BaseBranch is required")
	case s.Workspace == "":
		return errors.New("gitops: Spec.Workspace is required")
	case s.PrimaryClonePath == "":
		return errors.New("gitops: Spec.PrimaryClonePath is required")
	default:
		return nil
	}
}

// Result is Run's typed outcome -- the "small result type the dispatcher
// maps" onto a board transition, analogous to (but distinct from) a
// spawned worker's contract.WorkerResult. json tags are included so a
// caller that wants to log/persist one alongside a run row can marshal it
// directly, though Run's caller consumes it in-process.
type Result struct {
	Outcome Outcome `json:"outcome"`

	// Reason is a short, human-readable explanation, set for
	// OutcomeNotMergeable and OutcomeStaleBaseConflict (the dispatcher uses
	// it as the Blocked/Rework comment body). Empty for OutcomeMerged and
	// OutcomeCIPending.
	Reason string `json:"reason,omitempty"`

	// ConflictingFiles is set only for OutcomeStaleBaseConflict, naming the
	// files a local conflict probe found unmerged after the update-branch
	// attempt, so the dispatcher's rework comment can name them.
	ConflictingFiles []string `json:"conflicting_files,omitempty"`

	// PRURL/PRNumber echo back gh's own view of the PR, when Run got far
	// enough to resolve one (every outcome except an early infrastructure
	// error).
	PRURL    string `json:"pr_url,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`

	// Retriable reports whether an OutcomeNotMergeable could plausibly
	// resolve on its own (true: e.g. a refused merge from a transient GitHub
	// state, or unsatisfied protection fixable out of band) or is
	// deterministic until a human or coder acts (false: failing required
	// checks). The dispatcher maps false to a non-retrying block kind --
	// re-running an identical merge gate against identical inputs burned 5
	// retries per incident in the Reflex build.
	Retriable bool `json:"retriable,omitempty"`
}

// notMergeable builds an OutcomeNotMergeable Result carrying view's PR
// identity, so every not-mergeable path (failing checks, unsatisfied
// protection, a failed merge attempt) reports the same shape. retriable says
// whether a later poll could plausibly succeed without human/coder action.
func notMergeable(view prView, reason string, retriable bool) Result {
	return Result{Outcome: OutcomeNotMergeable, Reason: reason, PRURL: view.URL, PRNumber: view.Number, Retriable: retriable}
}

// Run executes one deterministic pass of the Git-operator lane against
// spec's PR. runCommand is the CommandRunner used for every `gh`
// invocation; a nil runCommand defaults to DefaultCommandRunner (a real
// subprocess call) -- local git operations (the conflict probe, tagging,
// worktree cleanup) always run for real, matching internal/spawn's own
// convention, since they never leave the local clone.
//
// The returned error is reserved for infrastructure failures (a
// CommandRunner error, unparseable gh output, a worktree-cleanup I/O
// failure) that the dispatcher should log and leave the claim in place for
// -- as opposed to a well-formed Result, which always represents a
// meaningful decision about the PR itself.
func Run(ctx context.Context, spec Spec, runCommand CommandRunner) (Result, error) {
	if runCommand == nil {
		runCommand = DefaultCommandRunner
	}
	if err := spec.validate(); err != nil {
		return Result{}, err
	}

	view, err := fetchPRView(ctx, spec, runCommand)
	if err != nil {
		return Result{}, fmt.Errorf("gitops: fetching PR state for branch %s: %w", spec.Branch, err)
	}
	if view.State == prStateMerged {
		// Idempotent short-circuit (crash-recovery safety): a dispatcher
		// restart's RecoverOrphans can requeue an orphaned "merging" claim
		// whose PR already merged in an EARLIER pass -- e.g. the process
		// died between `gh pr merge` succeeding and the Transition that
		// would have closed the run out. Re-running the checks/protection/
		// merge gate against an already-merged PR would misreport it as
		// OutcomeNotMergeable (or fail outright); recognizing the PR's own
		// terminal state up front instead reports the same OutcomeMerged a
		// fresh merge would, without re-attempting `gh pr merge` or redoing
		// tag/cleanup (whose completion this pass has no way to verify) --
		// mirroring graphs/coder.py's open_PR gh-pr-view-first reuse.
		return Result{Outcome: OutcomeMerged, PRURL: view.URL, PRNumber: view.Number}, nil
	}

	checks, err := fetchChecks(ctx, spec, runCommand)
	if err != nil {
		return Result{}, fmt.Errorf("gitops: fetching CI checks for branch %s: %w", spec.Branch, err)
	}
	switch state, failing := classifyChecks(checks); state {
	case checksAbsent:
		if spec.RequireChecks {
			// No checks *yet*: on this repo CI may just not have registered
			// (observed ~40 min late during the Reflex build). Same handling
			// as pending -- wait, don't block. Blocking here strands a PR
			// that just needed CI to catch up.
			return Result{Outcome: OutcomeCIPending, PRURL: view.URL, PRNumber: view.Number}, nil
		}
		// No CI on this repo by declaration: fall through to protection/merge.
	case checksFailing:
		// Deterministic: an identical re-check against identical inputs just
		// re-fails. Park for a human/coder, don't auto-retry.
		return notMergeable(view, fmt.Sprintf("required checks failing: %s", joinNames(failing)), false), nil
	case checksPending:
		return Result{Outcome: OutcomeCIPending, PRURL: view.URL, PRNumber: view.Number}, nil
	}
	// checksGreen falls through.

	protectionOK, protectionReason, err := checkBranchProtection(ctx, spec, runCommand)
	if err != nil {
		return Result{}, fmt.Errorf("gitops: checking branch protection for %s: %w", spec.BaseBranch, err)
	}
	if !protectionOK {
		// Protection can be fixed out of band (a rule toggled, a required
		// review added) and is cheap to re-check, so this is retriable.
		return notMergeable(view, fmt.Sprintf("branch protection unsatisfied for %s: %s", spec.BaseBranch, protectionReason), true), nil
	}

	if mergeabilityUnknown(view) {
		// GitHub's mergeability computation is mid-recompute (a concurrent
		// merge just advanced the base); a `gh pr merge` now is guaranteed
		// refused. Treat it like a pending check (R3): re-check next poll,
		// rather than firing the doomed merge and leaning on mergeNotReady's
		// string-matching to rescue it.
		return Result{Outcome: OutcomeCIPending, PRURL: view.URL, PRNumber: view.Number}, nil
	}

	if needsBaseUpdate(view) || hasConflict(view) {
		result, proceed, err := resolveStaleBase(ctx, spec, view, runCommand)
		if err != nil {
			return Result{}, err
		}
		if !proceed {
			return result, nil
		}
	}

	// The Coder lane opens draft PRs (project convention); gh refuses to
	// merge a draft, so convert it to ready-for-review first. Gated on
	// IsDraft so a non-draft PR is never needlessly readied.
	if view.IsDraft {
		if err := readyPR(ctx, spec, runCommand); err != nil {
			return Result{}, fmt.Errorf("gitops: marking draft PR ready for branch %s: %w", spec.Branch, err)
		}
	}

	mergeRes, err := mergePR(ctx, spec, runCommand)
	if err != nil {
		return Result{}, fmt.Errorf("gitops: merging PR for branch %s: %w", spec.Branch, err)
	}
	if mergeRes.ExitCode != 0 {
		reason := firstNonEmpty(mergeRes.Stderr, mergeRes.Stdout)
		if mergeNotReady(reason) {
			// GitHub refused the merge because the PR isn't ready YET — required
			// checks still pending/re-running, or a strict "up to date" policy
			// after a concurrent merge advanced the base — not a genuine failure.
			// Treat it exactly like a pending check (R3): leave the merging
			// claim in place, let its short TTL expire, and retry next poll, by
			// which point the checks finish (or a later pass's resolveStaleBase
			// catches the base up). A hard NotMergeable block here would strand a
			// PR that just needed a couple more minutes.
			return Result{Outcome: OutcomeCIPending, PRURL: view.URL, PRNumber: view.Number}, nil
		}
		// A merge refusal mergeNotReady didn't recognize: the state may still
		// shift under us, so let a later poll retry rather than park outright.
		return notMergeable(view, fmt.Sprintf("gh pr merge failed: %s", reason), true), nil
	}

	if spec.Tag != "" {
		if err := tagRelease(ctx, spec); err != nil {
			return Result{}, fmt.Errorf("gitops: tagging %s on branch %s: %w", spec.Tag, spec.Branch, err)
		}
	}

	if err := removeWorktree(ctx, spec); err != nil {
		return Result{}, fmt.Errorf("gitops: cleaning up worktree for branch %s: %w", spec.Branch, err)
	}

	return Result{Outcome: OutcomeMerged, PRURL: view.URL, PRNumber: view.Number}, nil
}

// resolveStaleBase attempts to bring an out-of-date or conflicting branch
// up to date with its base via `gh pr update-branch`, then re-checks.
//
// It returns proceed=true only when the re-check shows the PR is no longer
// behind or conflicting -- Run's caller then falls through to the normal
// merge attempt in the same pass, matching the "stale-base-updates-and-
// merges" scenario. Any other outcome (a real conflict, or the update
// still catching up) is terminal for this pass: proceed=false and result is
// what Run should return as-is.
func resolveStaleBase(ctx context.Context, spec Spec, view prView, runner CommandRunner) (result Result, proceed bool, err error) {
	if _, updateErr := updateBranch(ctx, spec, runner); updateErr != nil {
		if !hasConflict(view) {
			return Result{}, false, fmt.Errorf("gitops: updating branch %s onto %s: %w", spec.Branch, spec.BaseBranch, updateErr)
		}
		// GitHub refuses update-branch outright when the PR conflicts with
		// its base -- that refusal IS the conflict verdict, not an
		// infrastructure failure. Returning an error here re-claims the card
		// forever (the Reflex build's 193-attempt loop); routing to rework
		// hands the coder a conflict-resolution turn instead.
		files, probeErr := probeConflictFiles(ctx, spec)
		if probeErr != nil {
			return Result{}, false, fmt.Errorf("gitops: probing conflicting files for branch %s against %s: %w", spec.Branch, spec.BaseBranch, probeErr)
		}
		return Result{
			Outcome:          OutcomeStaleBaseConflict,
			Reason:           fmt.Sprintf("gh pr update-branch refused for %s (conflict with base %s)", spec.Branch, spec.BaseBranch),
			ConflictingFiles: files,
			PRURL:            view.URL,
			PRNumber:         view.Number,
		}, false, nil
	}

	recheck, err := fetchPRView(ctx, spec, runner)
	if err != nil {
		return Result{}, false, fmt.Errorf("gitops: re-checking PR state for branch %s: %w", spec.Branch, err)
	}

	switch {
	case !needsBaseUpdate(recheck) && !hasConflict(recheck):
		// Resolved cleanly -- caller proceeds straight to the merge.
		return Result{}, true, nil
	case hasConflict(recheck):
		files, probeErr := probeConflictFiles(ctx, spec)
		if probeErr != nil {
			return Result{}, false, fmt.Errorf("gitops: probing conflicting files for branch %s against %s: %w", spec.Branch, spec.BaseBranch, probeErr)
		}
		reason := fmt.Sprintf("branch %s still conflicts with base %s after gh pr update-branch", spec.Branch, spec.BaseBranch)
		return Result{
			Outcome:          OutcomeStaleBaseConflict,
			Reason:           reason,
			ConflictingFiles: files,
			PRURL:            recheck.URL,
			PRNumber:         recheck.Number,
		}, false, nil
	default:
		// Still BEHIND (or an unresolved transitional state): GitHub's
		// update-branch job is presumably still catching up. Treat this
		// exactly like a pending check -- try again next poll -- rather
		// than inventing a third "waiting" flavor of Result.
		return Result{Outcome: OutcomeCIPending, PRURL: recheck.URL, PRNumber: recheck.Number}, false, nil
	}
}

// mergeNotReady reports whether a failed `gh pr merge` reflects a transient
// not-ready state — required checks still pending/re-running, or a strict
// "require branches to be up to date" policy after a concurrent merge advanced
// the base — rather than a genuine, terminal failure. GitHub phrases these as
// "not mergeable: the base branch policy prohibits the merge", references the
// required status checks, or says the PR is "not in a mergeable state", and
// hints at --auto/--admin. Retrying next poll lets the checks finish (or a
// later resolveStaleBase pass catch the base up). Matched case-insensitively
// on stable fragments of gh's own message.
func mergeNotReady(output string) bool {
	o := strings.ToLower(output)
	for _, marker := range []string{
		"base branch policy",
		"required status check",
		"not in a mergeable state",
		"requirements have been met",
		"--auto",
	} {
		if strings.Contains(o, marker) {
			return true
		}
	}
	return false
}

// joinNames renders a []string as a comma-separated list for a Result
// Reason string, without pulling in strings.Join at every call site.
func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// firstNonEmpty returns the first non-empty argument, or "" if all are
// empty -- used to pick a Reason from whichever of stderr/stdout a failed
// gh invocation actually wrote to.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
