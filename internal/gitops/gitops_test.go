package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupScenario builds a real primary repo + issue worktree (branch
// "clp-1-feature" off "main"), PATH-shims the fake gh script in front of
// the real one, and points it at $FAKE_GH_SCENARIO=scenario. It returns the
// Spec Run should be called with and the path to that invocation's gh
// call-log file.
//
// For "stale_base_conflict" only, it also makes the issue branch and main
// genuinely conflict on README.md: unlike the other scenarios (which never
// exercise real git beyond worktree admin), Run's stale-base-conflict path
// runs a real local merge test (probeConflictFiles) whose result must
// agree with what the faked `gh pr view` reports.
func setupScenario(t *testing.T, scenario string) (spec Spec, callLog string) {
	t.Helper()

	primary := newPrimaryRepo(t, "main")
	branch := "clp-1-feature"
	worktree := newIssueWorktree(t, primary, branch, "main")

	if scenario == "stale_base_conflict" || scenario == "update_branch_refused" {
		writeFileT(t, worktree, "README.md", "issue branch changed this line\n")
		runGitT(t, worktree, "commit", "-am", "issue branch edits README")
		writeFileT(t, primary, "README.md", "main branch changed this line differently\n")
		runGitT(t, primary, "commit", "-am", "main edits README differently")
	}

	fakeDir := t.TempDir()
	writeFakeGh(t, fakeDir)
	prependPath(t, fakeDir)

	callLogFile := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_GH_SCENARIO", scenario)
	t.Setenv("FAKE_GH_STATE_DIR", t.TempDir())
	t.Setenv("FAKE_GH_CALLLOG", callLogFile)

	spec = Spec{
		Branch:           branch,
		BaseBranch:       "main",
		Workspace:        worktree,
		PrimaryClonePath: primary,
	}
	return spec, callLogFile
}

// readCallLog returns the recorded gh invocations (one per line, argv
// space-joined -- see fakegh_test.go) made during a Run call set up via
// setupScenario.
func readCallLog(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading call log %s: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// countCallsWithPrefix counts how many recorded calls start with prefix
// (e.g. "pr view", "pr update-branch").
func countCallsWithPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// assertWorktreeIntact fails the test unless worktree still exists and
// branch is still a local branch of primary -- the state a non-merged
// outcome must leave behind so Rework/a human requeue can reuse it.
func assertWorktreeIntact(t *testing.T, primary, worktree, branch string) {
	t.Helper()
	if _, err := os.Stat(worktree); err != nil {
		t.Errorf("worktree %s should still exist, stat error: %v", worktree, err)
	}
	if !branchExistsT(t, primary, branch) {
		t.Errorf("branch %s should still exist in %s", branch, primary)
	}
}

// assertWorktreeRemoved fails the test unless worktree and branch are both
// gone -- the state Run must leave behind on a successful merge.
func assertWorktreeRemoved(t *testing.T, primary, worktree, branch string) {
	t.Helper()
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("worktree %s should be removed, stat error: %v", worktree, err)
	}
	if branchExistsT(t, primary, branch) {
		t.Errorf("branch %s should be removed from %s", branch, primary)
	}
}

// TestRun_Mergeable_Merges is the happy path: checks green, protection
// satisfied, PR clean -> Run merges, and cleans up the worktree/branch.
func TestRun_Mergeable_Merges(t *testing.T) {
	spec, callLog := setupScenario(t, "mergeable")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("Run() Outcome = %q, want %q (Reason: %q)", res.Outcome, OutcomeMerged, res.Reason)
	}
	if res.PRURL == "" || res.PRNumber == 0 {
		t.Errorf("Run() Result missing PR identity: %+v", res)
	}

	assertWorktreeRemoved(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)

	calls := readCallLog(t, callLog)
	if got := countCallsWithPrefix(calls, "pr merge "+spec.Branch); got != 1 {
		t.Errorf("gh pr merge called %d times, want 1 (calls: %v)", got, calls)
	}
	if got := countCallsWithPrefix(calls, "pr merge "+spec.Branch+" --squash"); got != 1 {
		t.Errorf("default merge method should be --squash (calls: %v)", calls)
	}
}

// TestRun_MergeMethod_HonorsConfiguredFlag asserts Spec.MergeMethod picks
// the corresponding `gh pr merge` flag end to end.
func TestRun_MergeMethod_HonorsConfiguredFlag(t *testing.T) {
	spec, callLog := setupScenario(t, "mergeable")
	spec.MergeMethod = "rebase"

	if _, err := Run(ctxWithTimeout(t), spec, nil); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}

	calls := readCallLog(t, callLog)
	if got := countCallsWithPrefix(calls, "pr merge "+spec.Branch+" --rebase"); got != 1 {
		t.Errorf("expected a --rebase merge call, calls: %v", calls)
	}
}

// TestRun_Tag_CreatesTagOnMerge asserts a configured Spec.Tag is created
// once the merge succeeds.
func TestRun_Tag_CreatesTagOnMerge(t *testing.T) {
	spec, _ := setupScenario(t, "mergeable")
	spec.Tag = "v1.2.3"

	// Cleanup removes the worktree after a successful merge, so the tag
	// must exist in it *before* Run tears it down; assert by re-deriving
	// the branch tip's sha from the primary clone's reflog is unnecessary
	// here -- easier to just check the tag landed in the primary clone's
	// object store (a worktree and its primary clone share one repo, so a
	// tag created in the worktree is visible from the primary clone too,
	// even after the worktree directory itself is removed).
	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("Run() Outcome = %q, want %q", res.Outcome, OutcomeMerged)
	}

	tags := runGitT(t, spec.PrimaryClonePath, "tag", "--list", "v1.2.3")
	if tags != "v1.2.3" {
		t.Errorf("git tag --list v1.2.3 = %q, want it to exist", tags)
	}
}

// TestRun_FailingChecks_NotMergeable asserts a failing required check
// blocks the merge, leaves the worktree intact, and names the failing
// check in Reason.
func TestRun_FailingChecks_NotMergeable(t *testing.T) {
	spec, _ := setupScenario(t, "failing_checks")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeNotMergeable {
		t.Fatalf("Run() Outcome = %q, want %q", res.Outcome, OutcomeNotMergeable)
	}
	if !strings.Contains(res.Reason, "build") {
		t.Errorf("Run() Reason = %q, want it to name the failing check", res.Reason)
	}
	assertWorktreeIntact(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)
}

// TestRun_MergeNotReady_CIPending asserts that a `gh pr merge` refusal because
// the PR isn't ready YET — GitHub's "base branch policy prohibits the merge …
// after all the requirements have been met" (required checks still pending, or
// a strict up-to-date policy after a concurrent merge advanced the base) — is
// treated as a retryable pending state, NOT a permanent NotMergeable block. A
// hard block here would strand a PR that just needed a couple more minutes for
// CI; instead the merging claim's short TTL expires and the next poll retries.
func TestRun_MergeNotReady_CIPending(t *testing.T) {
	spec, _ := setupScenario(t, "merge_not_ready")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeCIPending {
		t.Fatalf("Run() Outcome = %q, want %q (transient not-ready, retry next poll)", res.Outcome, OutcomeCIPending)
	}
	// Worktree must stay intact — nothing merged, so no cleanup.
	assertWorktreeIntact(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)
}

// TestRunAbsentChecksPendsWhenRequired asserts that with RequireChecks (the
// safe default), no required checks reported yet is treated as "wait for CI
// to register", NOT a block: on a repo whose CI registers late this is the
// only correct move (observed ~40 min late during the Reflex build), so it
// maps to OutcomeCIPending and leaves the worktree intact.
func TestRunAbsentChecksPendsWhenRequired(t *testing.T) {
	spec, _ := setupScenario(t, "absent_checks")
	spec.RequireChecks = true

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeCIPending {
		t.Fatalf("Run() Outcome = %q, want %q (absent checks wait for registration)", res.Outcome, OutcomeCIPending)
	}
	assertWorktreeIntact(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)
}

// TestRunAbsentChecksProceedsWhenNotRequired asserts that a repo declaring it
// has no CI at all (RequireChecks=false) lets an absent-checks PR merge on
// branch protection alone, rather than blocking or waiting forever for checks
// that will never register.
func TestRunAbsentChecksProceedsWhenNotRequired(t *testing.T) {
	spec, _ := setupScenario(t, "absent_checks")
	spec.RequireChecks = false

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("Run() Outcome = %q, want %q (checks not required)", res.Outcome, OutcomeMerged)
	}
	assertWorktreeRemoved(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)
}

// TestRun_ProtectionUnsatisfied_NotMergeable asserts green checks alone are
// not enough -- an unprotected base branch also blocks the merge.
func TestRun_ProtectionUnsatisfied_NotMergeable(t *testing.T) {
	spec, _ := setupScenario(t, "protection_unsatisfied")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeNotMergeable {
		t.Fatalf("Run() Outcome = %q, want %q", res.Outcome, OutcomeNotMergeable)
	}
	if !strings.Contains(res.Reason, "protection") {
		t.Errorf("Run() Reason = %q, want it to mention branch protection", res.Reason)
	}
	assertWorktreeIntact(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)
}

// TestRun_CIPending_ProducesNoTransitionableOutcome asserts still-running
// (not failed) checks produce OutcomeCIPending, distinct from
// OutcomeNotMergeable -- R3: the dispatcher must skip Transition/board.Next
// entirely for this outcome and let the short merging-claim TTL expire so
// the next poll re-checks, rather than parking the issue in Blocked.
func TestRun_CIPending_ProducesNoTransitionableOutcome(t *testing.T) {
	spec, _ := setupScenario(t, "ci_pending")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeCIPending {
		t.Fatalf("Run() Outcome = %q, want %q", res.Outcome, OutcomeCIPending)
	}
	if res.Outcome == OutcomeNotMergeable {
		t.Fatal("OutcomeCIPending must never equal/be confused with OutcomeNotMergeable")
	}
	assertWorktreeIntact(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)
}

// TestRun_StaleBaseUpdatesAndMerges asserts a PR that's merely behind its
// base is updated via `gh pr update-branch` and, once the re-check reports
// clean, merged in the same Run call.
func TestRun_StaleBaseUpdatesAndMerges(t *testing.T) {
	spec, callLog := setupScenario(t, "stale_base_merges")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("Run() Outcome = %q, want %q (Reason: %q)", res.Outcome, OutcomeMerged, res.Reason)
	}
	assertWorktreeRemoved(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)

	calls := readCallLog(t, callLog)
	if got := countCallsWithPrefix(calls, "pr update-branch "+spec.Branch); got != 1 {
		t.Errorf("expected exactly one update-branch call, calls: %v", calls)
	}
	if got := countCallsWithPrefix(calls, "pr view "+spec.Branch); got != 2 {
		t.Errorf("expected two pr view calls (before and after the update), got %d: %v", got, calls)
	}
}

// TestRun_StaleBaseConflict_RoutesToRework asserts a PR that's behind AND
// genuinely conflicts (even after gh pr update-branch) reports
// OutcomeStaleBaseConflict naming the real conflicting file, and leaves
// the worktree clean and intact for a Rework re-run.
func TestRun_StaleBaseConflict_RoutesToRework(t *testing.T) {
	spec, callLog := setupScenario(t, "stale_base_conflict")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeStaleBaseConflict {
		t.Fatalf("Run() Outcome = %q, want %q (Reason: %q)", res.Outcome, OutcomeStaleBaseConflict, res.Reason)
	}
	if len(res.ConflictingFiles) != 1 || res.ConflictingFiles[0] != "README.md" {
		t.Errorf("Run() ConflictingFiles = %v, want [README.md]", res.ConflictingFiles)
	}
	if res.Reason == "" {
		t.Error("Run() Reason is empty, want an explanation naming the stale base")
	}

	assertWorktreeIntact(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)
	assertNoMergeInProgress(t, spec.Workspace)
	if status := runGitT(t, spec.Workspace, "status", "--porcelain"); status != "" {
		t.Errorf("worktree not clean after Run: %q", status)
	}

	calls := readCallLog(t, callLog)
	if got := countCallsWithPrefix(calls, "pr merge"); got != 0 {
		t.Errorf("gh pr merge should never be called on a conflict, calls: %v", calls)
	}
}

// TestRunUpdateBranchRefusedOnConflictIsRework asserts that when the
// triggering view already shows a conflict and `gh pr update-branch` is
// refused outright (GitHub won't update a branch that conflicts with its
// base), that refusal IS the conflict verdict -- OutcomeStaleBaseConflict
// naming the real conflicting file -- not an infrastructure error. Returning
// an error here re-claims the card forever (the Reflex build's 193-attempt
// loop); routing to rework hands the coder a conflict-resolution turn.
func TestRunUpdateBranchRefusedOnConflictIsRework(t *testing.T) {
	spec, callLog := setupScenario(t, "update_branch_refused")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("expected a Result, not an infrastructure error: %v", err)
	}
	if res.Outcome != OutcomeStaleBaseConflict {
		t.Fatalf("Run() Outcome = %q, want %q (Reason: %q)", res.Outcome, OutcomeStaleBaseConflict, res.Reason)
	}
	if len(res.ConflictingFiles) != 1 || res.ConflictingFiles[0] != "README.md" {
		t.Errorf("Run() ConflictingFiles = %v, want [README.md]", res.ConflictingFiles)
	}
	if res.Reason == "" {
		t.Error("Run() Reason is empty, want an explanation naming the refused update-branch")
	}

	assertWorktreeIntact(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)
	assertNoMergeInProgress(t, spec.Workspace)

	calls := readCallLog(t, callLog)
	if got := countCallsWithPrefix(calls, "pr merge"); got != 0 {
		t.Errorf("gh pr merge should never be called on a refused-update conflict, calls: %v", calls)
	}
}

// TestRun_MergeCommandFails_NotMergeable asserts that even after every
// upfront gate passes, a `gh pr merge` failure itself (e.g. a race against
// a newly-required check) is reported as OutcomeNotMergeable, not treated
// as success and not left uncleaned-up... err, cleaned up.
func TestRun_MergeCommandFails_NotMergeable(t *testing.T) {
	spec, _ := setupScenario(t, "merge_fails")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeNotMergeable {
		t.Fatalf("Run() Outcome = %q, want %q", res.Outcome, OutcomeNotMergeable)
	}
	if !strings.Contains(res.Reason, "required checks") {
		t.Errorf("Run() Reason = %q, want it to surface gh's own message", res.Reason)
	}
	assertWorktreeIntact(t, spec.PrimaryClonePath, spec.Workspace, spec.Branch)
}

// TestRun_AlreadyMergedPR_ReturnsOutcomeMergedWithoutRemerging asserts the
// crash-recovery idempotency case (design map concern #4): a dispatcher
// restart's RecoverOrphans can requeue an orphaned "merging" claim whose PR
// already merged in an earlier pass (the process died between `gh pr merge`
// succeeding and the Transition that would have closed the run out and
// moved the card to Documentation). Run must recognize the PR's own
// terminal state up front and report OutcomeMerged immediately, never
// re-attempting the merge/checks/protection gate against an already-merged
// PR (which real gh would refuse, and which this package would otherwise
// misreport as OutcomeNotMergeable) — mirroring graphs/coder.py's open_PR
// gh-pr-view-first reuse.
func TestRun_AlreadyMergedPR_ReturnsOutcomeMergedWithoutRemerging(t *testing.T) {
	spec, callLog := setupScenario(t, "already_merged")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("Run() Outcome = %q, want %q (Reason: %q)", res.Outcome, OutcomeMerged, res.Reason)
	}
	if res.PRURL == "" || res.PRNumber == 0 {
		t.Errorf("Run() Result missing PR identity: %+v", res)
	}

	calls := readCallLog(t, callLog)
	if got := countCallsWithPrefix(calls, "pr view"); got != 1 {
		t.Errorf("gh pr view called %d times, want exactly 1 (calls: %v)", got, calls)
	}
	if got := countCallsWithPrefix(calls, "pr merge"); got != 0 {
		t.Errorf("gh pr merge should never be called against an already-merged PR, calls: %v", calls)
	}
	if got := countCallsWithPrefix(calls, "pr checks"); got != 0 {
		t.Errorf("gh pr checks should never be called against an already-merged PR, calls: %v", calls)
	}
	if got := countCallsWithPrefix(calls, "api"); got != 0 {
		t.Errorf("gh api (branch protection) should never be called against an already-merged PR, calls: %v", calls)
	}
}

// TestRun_InvalidSpec_ReturnsError asserts a Spec missing a required field
// fails fast, before ever invoking a command.
func TestRun_InvalidSpec_ReturnsError(t *testing.T) {
	if _, err := Run(ctxWithTimeout(t), Spec{}, nil); err == nil {
		t.Fatal("Run: expected an error for an empty Spec, got nil")
	}
}

func TestRun_DraftPR_MarksReadyThenMerges(t *testing.T) {
	spec, callLog := setupScenario(t, "draft")

	res, err := Run(ctxWithTimeout(t), spec, nil)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("Run() Outcome = %q, want %q (Reason: %q)", res.Outcome, OutcomeMerged, res.Reason)
	}

	calls := readCallLog(t, callLog)
	// gh refuses to merge a draft PR (the Coder lane opens drafts), so Run
	// must `gh pr ready` it exactly once, strictly before `gh pr merge`.
	if got := countCallsWithPrefix(calls, "pr ready "+spec.Branch); got != 1 {
		t.Errorf("gh pr ready called %d times, want 1 (calls: %v)", got, calls)
	}
	if got := countCallsWithPrefix(calls, "pr merge "+spec.Branch); got != 1 {
		t.Errorf("gh pr merge called %d times, want 1 (calls: %v)", got, calls)
	}
	readyIdx, mergeIdx := -1, -1
	for i, c := range calls {
		if readyIdx == -1 && strings.HasPrefix(c, "pr ready "+spec.Branch) {
			readyIdx = i
		}
		if mergeIdx == -1 && strings.HasPrefix(c, "pr merge "+spec.Branch) {
			mergeIdx = i
		}
	}
	if readyIdx == -1 || mergeIdx == -1 || readyIdx > mergeIdx {
		t.Errorf("expected `gh pr ready` before `gh pr merge`; readyIdx=%d mergeIdx=%d calls=%v", readyIdx, mergeIdx, calls)
	}
}
