package dispatcher_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/gitops"
	"github.com/xlyk/clipse/internal/linear"
)

func runGitopsTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newGitopsRemoteAndClone(t *testing.T) (origin, primary string) {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatalf("creating seed repo: %v", err)
	}
	runGitopsTestGit(t, seed, "init", "-b", "main")
	runGitopsTestGit(t, seed, "config", "user.name", "Clipse Test")
	runGitopsTestGit(t, seed, "config", "user.email", "clipse-test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("dispatcher gitops fixture\n"), 0o644); err != nil {
		t.Fatalf("writing seed README: %v", err)
	}
	runGitopsTestGit(t, seed, "add", "README.md")
	runGitopsTestGit(t, seed, "commit", "-m", "initial commit")

	origin = filepath.Join(t.TempDir(), "origin.git")
	runGitopsTestGit(t, filepath.Dir(origin), "clone", "--bare", seed, origin)
	primary = filepath.Join(t.TempDir(), "primary")
	runGitopsTestGit(t, filepath.Dir(primary), "clone", origin, primary)
	return origin, primary
}

func cloneGitopsRemote(t *testing.T, origin, name string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), name)
	runGitopsTestGit(t, filepath.Dir(clone), "clone", origin, clone)
	runGitopsTestGit(t, clone, "config", "user.name", "Clipse Test")
	runGitopsTestGit(t, clone, "config", "user.email", "clipse-test@example.com")
	return clone
}

// TestTick_MergingClaim_Mergeable_RoutesToDone asserts a claimed merging
// card whose fake gitops run reports OutcomeMerged flows through the SAME
// applyTerminalOutcome/board.Next path a spawned worker's "done" result
// would: merging -> done (terminal; documentation is written in the Coder
// turn now, so there is no post-merge stage), claim cleared, a setstate
// mirror enqueued for the new column.
func TestTick_MergingClaim_Mergeable_RoutesToDone(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()

	var calls int
	var gotSpec gitops.Spec
	gitOpsFn := func(_ context.Context, spec gitops.Spec) (gitops.Result, error) {
		calls++
		gotSpec = spec
		return gitops.Result{Outcome: gitops.OutcomeMerged, PRURL: "https://github.com/x/y/pull/7", PRNumber: 7}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("gitops calls = %d, want exactly 1", calls)
	}
	if gotSpec.Branch != "issue-1-branch" {
		t.Errorf("Spec.Branch = %q, want %q", gotSpec.Branch, "issue-1-branch")
	}
	if gotSpec.BaseBranch != cfg.Repo.BaseBranch {
		t.Errorf("Spec.BaseBranch = %q, want %q", gotSpec.BaseBranch, cfg.Repo.BaseBranch)
	}
	if gotSpec.PrimaryClonePath != cfg.Repo.Path {
		t.Errorf("Spec.PrimaryClonePath = %q, want %q", gotSpec.PrimaryClonePath, cfg.Repo.Path)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnDone) {
		t.Errorf("BoardStatus = %q, want done", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}

	var sawDoneSetState bool
	for _, c := range lc.SetStateCalls {
		if c.TargetColumn == string(contract.ColumnDone) {
			sawDoneSetState = true
		}
	}
	if !sawDoneSetState {
		t.Errorf("SetState calls = %+v, want a mirror to done", lc.SetStateCalls)
	}

	// No spawned worker process for the merging column — gitops runs inline.
	if spawner.SpawnCount() != 0 {
		t.Errorf("SpawnCount = %d, want 0 (merging never spawns a worker)", spawner.SpawnCount())
	}
}

// fallThroughPreCheck is a GitOpsPreChecker that never resolves, so a test
// drives the full worktree pass through its WithGitOpsRunner stub exactly as
// before the read-only pre-check existed (the pre-check only short-circuits a
// merged or missing PR; every other card falls through to the worktree pass).
func fallThroughPreCheck(context.Context, gitops.Spec) (gitops.Result, bool, error) {
	return gitops.Result{}, false, nil
}

// TestTick_MergingClaim_NoPR_PreCheckParksWithoutWorktree asserts Task 5's
// core ordering fix: the READ-ONLY PR pre-check runs from the PRIMARY CLONE
// (Spec.Workspace == cfg.Repo.Path) BEFORE any worktree is ensured, and a
// terminal verdict (here a missing PR) parks the card without ever calling
// ws.Ensure or the full worktree pass. This is what stops a hand-deleted
// worktree/branch from being resurrected every poll only to fail again at
// `gh pr view` -- the Reflex retro's zombie runs.
func TestTick_MergingClaim_NoPR_PreCheckParksWithoutWorktree(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()

	var preWorkspace string
	preChecker := func(_ context.Context, spec gitops.Spec) (gitops.Result, bool, error) {
		preWorkspace = spec.Workspace
		return gitops.Result{Outcome: gitops.OutcomeNotMergeable, Retriable: false, Reason: "no pull request exists for branch issue-1-branch"}, true, nil
	}
	var fullPassCalls int
	gitOpsFn := func(context.Context, gitops.Spec) (gitops.Result, error) {
		fullPassCalls++
		return gitops.Result{}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(preChecker),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	if preWorkspace != cfg.Repo.Path {
		t.Errorf("pre-check Spec.Workspace = %q, want the primary clone %q (never the worktree)", preWorkspace, cfg.Repo.Path)
	}
	if fullPassCalls != 0 {
		t.Errorf("full gitops pass ran %d times, want 0 (a terminal pre-check resolves the pass with no worktree)", fullPassCalls)
	}
	if ensured := ws.EnsuredIssues(); len(ensured) != 0 {
		t.Errorf("ws.Ensure called for %v, want no worktree ensured for a terminal pre-check", ensured)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnBlocked) {
		t.Errorf("BoardStatus = %q, want blocked", issue.BoardStatus)
	}
}

// TestTick_MergingClaim_OpenPR_PreCheckFallsThroughToWorktreeMerge asserts the
// read-only pre-check does NOT merge from the primary clone for an open,
// unmerged PR: it falls through, ws.Ensure runs, and the full side-effecting
// pass merges from the ISSUE WORKTREE (Spec.Workspace != primary clone),
// carrying the issue identity for the squash subject. Merging from the primary
// clone instead would leak the worktree/branch on every merge and drop the
// squash subject -- the two Criticals this read-only redesign fixes.
func TestTick_MergingClaim_OpenPR_PreCheckFallsThroughToWorktreeMerge(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()

	var fullPassSpec gitops.Spec
	var fullPassCalls int
	gitOpsFn := func(_ context.Context, spec gitops.Spec) (gitops.Result, error) {
		fullPassCalls++
		fullPassSpec = spec
		return gitops.Result{Outcome: gitops.OutcomeMerged, PRURL: "https://github.com/x/y/pull/7", PRNumber: 7}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	if fullPassCalls != 1 {
		t.Fatalf("full gitops pass ran %d times, want 1 (the pre-check falls through for an open PR)", fullPassCalls)
	}
	if ensured := ws.EnsuredIssues(); len(ensured) != 1 || ensured[0] != "issue-1" {
		t.Fatalf("ws.Ensure calls = %v, want [issue-1] (an open PR merges via its own worktree)", ensured)
	}
	if fullPassSpec.Workspace == cfg.Repo.Path {
		t.Errorf("full pass Spec.Workspace = primary clone %q, want the issue worktree (no merge/cleanup from the primary clone)", cfg.Repo.Path)
	}
	if fullPassSpec.IssueID != "issue-1" {
		t.Errorf("full pass Spec.IssueID = %q, want issue-1 (worktree pass carries the squash-subject identity, unlike the pre-check)", fullPassSpec.IssueID)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnDone) {
		t.Errorf("BoardStatus = %q, want done", issue.BoardStatus)
	}
}

// TestTick_MergingClaim_RemoteOnlyBranchRunsGitopsFromRemoteTip covers the
// production Workspacer boundary: a Daytona coder publishes the issue branch
// without creating a host-local branch, then the Git-operator must ensure a
// worktree whose HEAD and contents come from that remote feature tip.
func TestTick_MergingClaim_RemoteOnlyBranchRunsGitopsFromRemoteTip(t *testing.T) {
	origin, primary := newGitopsRemoteAndClone(t)
	daytona := cloneGitopsRemote(t, origin, "daytona")

	const branch = "issue-1-branch"
	runGitopsTestGit(t, daytona, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(daytona, "daytona.txt"), []byte("remote agent commit\n"), 0o644); err != nil {
		t.Fatalf("writing Daytona change: %v", err)
	}
	runGitopsTestGit(t, daytona, "add", "daytona.txt")
	runGitopsTestGit(t, daytona, "commit", "-m", "feat: remote change")
	wantSHA := runGitopsTestGit(t, daytona, "rev-parse", "HEAD")
	runGitopsTestGit(t, daytona, "push", "origin", "HEAD:refs/heads/"+branch)
	if got := runGitopsTestGit(t, primary, "ls-remote", "--heads", "origin", "refs/heads/"+branch); !strings.Contains(got, wantSHA) {
		t.Fatalf("precondition: remote feature ref = %q, want SHA %s", got, wantSHA)
	}

	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.Repo.Path = primary
	ws := dispatcher.NewGitWorkspacer(primary, cfg.Repo.BaseBranch, t.TempDir())

	var gotSHA string
	var gotContents []byte
	var readErr error
	gitOpsFn := func(_ context.Context, spec gitops.Spec) (gitops.Result, error) {
		gotSHA = runGitopsTestGit(t, spec.Workspace, "rev-parse", "HEAD")
		gotContents, readErr = os.ReadFile(filepath.Join(spec.Workspace, "daytona.txt"))
		return gitops.Result{Outcome: gitops.OutcomeCIPending, PRURL: "https://github.com/x/y/pull/7", PRNumber: 7}, nil
	}
	d := dispatcher.New(cfg, s, lc, newFakeSpawner(), ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}
	if gotSHA != wantSHA {
		t.Errorf("gitops workspace HEAD = %s, want remote feature %s", gotSHA, wantSHA)
	}
	if readErr != nil {
		t.Fatalf("reading Daytona commit from gitops workspace: %v", readErr)
	}
	if got := string(gotContents); got != "remote agent commit\n" {
		t.Errorf("daytona.txt = %q, want remote agent commit", got)
	}
}

// TestTick_MergingClaim_NotMergeable_Blocks asserts OutcomeNotMergeable maps
// to a "blocked" outcome from merging, with a comment carrying gitops'
// Reason.
func TestTick_MergingClaim_NotMergeable_Blocks(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()

	gitOpsFn := func(context.Context, gitops.Spec) (gitops.Result, error) {
		return gitops.Result{Outcome: gitops.OutcomeNotMergeable, Reason: "required checks failing: build"}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnBlocked) {
		t.Errorf("BoardStatus = %q, want blocked", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}

	if len(lc.CommentCalls) != 1 {
		t.Fatalf("Comment calls = %d, want 1", len(lc.CommentCalls))
	}
	if got := lc.CommentCalls[0].Body; got == "" {
		t.Errorf("comment body empty, want the not-mergeable reason")
	}
}

// TestTick_MergingClaim_NotMergeable_NonRetriable_ParksNeedsInput asserts
// that a deterministic (Retriable=false) not-mergeable verdict -- e.g.
// required checks failing -- maps to block_kind=needs_input, so the
// dispatcher parks it for a human instead of auto-retrying the identical
// merge gate (the Reflex build burned 5 identical retries per ticket). The
// posted comment names the "Needs input" kind.
func TestTick_MergingClaim_NotMergeable_NonRetriable_ParksNeedsInput(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()

	gitOpsFn := func(context.Context, gitops.Spec) (gitops.Result, error) {
		return gitops.Result{Outcome: gitops.OutcomeNotMergeable, Retriable: false, Reason: "required checks failing: build"}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnBlocked) {
		t.Errorf("BoardStatus = %q, want blocked", issue.BoardStatus)
	}
	if len(lc.CommentCalls) != 1 {
		t.Fatalf("Comment calls = %d, want 1", len(lc.CommentCalls))
	}
	if body := lc.CommentCalls[0].Body; !strings.Contains(body, "Needs input") {
		t.Errorf("comment body = %q, want it to name the needs_input block kind", body)
	}
}

// TestTick_MergingClaim_NotMergeable_Retriable_MapsTransient asserts that a
// retriable not-mergeable verdict (e.g. unsatisfied protection, fixable out
// of band) maps to block_kind=transient -- its comment names the "Transient
// error" kind (with RecoverCap=0 in tests it still parks, but as a transient
// kind eligible for auto-retry when a real deploy sets RecoverCap>0).
func TestTick_MergingClaim_NotMergeable_Retriable_MapsTransient(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()

	gitOpsFn := func(context.Context, gitops.Spec) (gitops.Result, error) {
		return gitops.Result{Outcome: gitops.OutcomeNotMergeable, Retriable: true, Reason: "branch protection unsatisfied for main"}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}
	if len(lc.CommentCalls) != 1 {
		t.Fatalf("Comment calls = %d, want 1", len(lc.CommentCalls))
	}
	if body := lc.CommentCalls[0].Body; !strings.Contains(body, "Transient error") {
		t.Errorf("comment body = %q, want it to name the transient block kind", body)
	}
}

// TestTick_MergingClaim_StaleBaseConflict_RoutesToRework asserts R1: gitops'
// OutcomeStaleBaseConflict maps to changes_requested from merging, which
// board.Next's ONE new additive entry routes to rework — the same
// rework_count bump and cap check a Reviewer's changes_requested gets.
func TestTick_MergingClaim_StaleBaseConflict_RoutesToRework(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()

	// The conflict-file probe only names files from the PR-branch worktree;
	// the primary clone is checked out on the base branch and would probe
	// "already up to date" -> no files. Returning files only for a worktree
	// workspace proves the full pass ran there, not from the primary clone
	// (Critical 2: a primary-clone probe short-circuited rework with an EMPTY
	// file list, so the coder got a conflict turn naming nothing).
	var fullPassWorkspace string
	gitOpsFn := func(_ context.Context, spec gitops.Spec) (gitops.Result, error) {
		fullPassWorkspace = spec.Workspace
		files := []string{"foo.go"}
		if spec.Workspace == cfg.Repo.Path {
			files = nil
		}
		return gitops.Result{
			Outcome:          gitops.OutcomeStaleBaseConflict,
			Reason:           "branch still conflicts with base main after gh pr update-branch",
			ConflictingFiles: files,
		}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	if ensured := ws.EnsuredIssues(); len(ensured) != 1 {
		t.Fatalf("ws.Ensure calls = %v, want [issue-1] (a conflict falls through to the worktree pass)", ensured)
	}
	if fullPassWorkspace == cfg.Repo.Path {
		t.Errorf("full pass Spec.Workspace = primary clone %q, want the issue worktree (the conflict probe needs it)", cfg.Repo.Path)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnRework) {
		t.Errorf("BoardStatus = %q, want rework", issue.BoardStatus)
	}
	if issue.ReworkCount != 1 {
		t.Errorf("ReworkCount = %d, want 1", issue.ReworkCount)
	}

	// Unlike a Reviewer's own changes_requested (which posts its findings
	// as inline PR comments itself — see graphs/reviewer.py), gitops never
	// posts anything to the PR for a stale-base conflict, so the dispatcher
	// posts a Linear comment naming the conflicting files instead (design
	// doc: "route to Rework with a comment naming the conflicting files").
	if len(lc.CommentCalls) != 1 {
		t.Fatalf("Comment calls = %d, want exactly 1 (naming the conflicting files)", len(lc.CommentCalls))
	}
	if got := lc.CommentCalls[0].Body; !strings.Contains(got, "foo.go") {
		t.Errorf("comment body = %q, want it to mention foo.go", got)
	}
}

// TestTick_MergingClaim_CIPending_NoTransition asserts R3: OutcomeCIPending
// produces no board transition at all — no Transition call, no board.Next,
// the claim left exactly as ClaimColumn set it (still "merging", still
// claimed) so the claim's own short TTL naturally re-checks on a later
// poll, and no Linear write of any kind is enqueued.
func TestTick_MergingClaim_CIPending_NoTransition(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()
	cfg.PollIntervalS = 45

	var calls int
	gitOpsFn := func(context.Context, gitops.Spec) (gitops.Result, error) {
		calls++
		return gitops.Result{Outcome: gitops.OutcomeCIPending, PRURL: "https://github.com/x/y/pull/7", PRNumber: 7}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("gitops calls = %d, want exactly 1", calls)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnMerging) {
		t.Errorf("BoardStatus = %q, want unchanged merging (no transition for CI-pending)", issue.BoardStatus)
	}
	if !issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = false, want still held (claim stays in place for CI-pending)")
	}
	// R3: the merging claim's TTL is cfg.PollIntervalS, not cfg.MaxRuntimeS.
	if !issue.ClaimExpires.Valid || issue.ClaimExpires.Int64 != 1000+int64(cfg.PollIntervalS) {
		t.Errorf("ClaimExpires = %+v, want exactly now+PollIntervalS (%d)", issue.ClaimExpires, 1000+int64(cfg.PollIntervalS))
	}

	if len(lc.SetStateCalls) != 0 {
		t.Errorf("SetState calls = %+v, want 0 (no transition happened)", lc.SetStateCalls)
	}
	if len(lc.CommentCalls) != 0 {
		t.Errorf("Comment calls = %+v, want 0 (no transition happened)", lc.CommentCalls)
	}
	pending, err := s.DrainPendingLinearWrites(context.Background(), 100)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending linear_writes = %+v, want 0 (no transition happened)", pending)
	}
}

// TestTick_MergingClaim_CIPendingThenExpiresAndRechecksNextPoll drives R3
// end to end: a CI-pending result leaves the claim in place; once the
// clock advances past the merging claim's short TTL, reconcile's
// ReleaseStaleClaims frees it (board_status still unchanged), and the SAME
// later tick's claimAndRunGitops re-claims and re-checks it — this time
// merging cleanly.
func TestTick_MergingClaim_CIPendingThenExpiresAndRechecksNextPoll(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "merging", 1, 100)

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()
	cfg.PollIntervalS = 10

	var calls int
	gitOpsFn := func(context.Context, gitops.Spec) (gitops.Result, error) {
		calls++
		if calls == 1 {
			return gitops.Result{Outcome: gitops.OutcomeCIPending, PRURL: "https://github.com/x/y/pull/7", PRNumber: 7}, nil
		}
		return gitops.Result{Outcome: gitops.OutcomeMerged, PRURL: "https://github.com/x/y/pull/7", PRNumber: 7}, nil
	}

	clockVal := int64(1000)
	clock := func() int64 { return clockVal }
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(clock),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("gitops calls after tick 1 = %d, want 1", calls)
	}

	precondition, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if precondition.BoardStatus != string(contract.ColumnMerging) || !precondition.ClaimLock.Valid {
		t.Fatalf("precondition: issue = %+v, want still claimed merging", precondition)
	}

	// Advance the clock past the claim's short TTL so reconcile's
	// ReleaseStaleClaims releases it before claimAndRunGitops re-claims.
	clockVal = 1000 + int64(cfg.PollIntervalS) + 1

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("gitops calls after tick 2 = %d, want exactly 2 (released and re-checked)", calls)
	}

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnDone) {
		t.Errorf("BoardStatus = %q, want done (re-check found it mergeable)", issue.BoardStatus)
	}
}

// TestTick_MergingClaim_RespectsGitOperatorAndGlobalCaps asserts
// Caps.PerLane.GitOperator (and the global cap) bound how many merging
// cards claimAndRunGitops processes per tick, even though gitops never
// touches d.inflight (it runs synchronously, not as a spawned worker) —
// exercising the local counter claimAndRunGitops must use instead.
func TestTick_MergingClaim_RespectsGitOperatorAndGlobalCaps(t *testing.T) {
	s := openTestStore(t)
	for i, id := range []string{"issue-a", "issue-b", "issue-c"} {
		seedColumnIssue(t, s, id, "merging", 1, int64(100+i))
	}

	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	cfg := testConfig()
	cfg.Caps.Global = 8
	cfg.Caps.PerLane.GitOperator = 1

	var calls int
	gitOpsFn := func(context.Context, gitops.Spec) (gitops.Result, error) {
		calls++
		return gitops.Result{Outcome: gitops.OutcomeMerged, PRURL: "https://x/y/pull/1", PRNumber: 1}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
		dispatcher.WithGitOpsPreChecker(fallThroughPreCheck),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	if calls != 1 {
		t.Errorf("gitops calls = %d, want exactly 1 (GitOperator per-lane cap)", calls)
	}

	var mergingLeft int
	for _, id := range []string{"issue-a", "issue-b", "issue-c"} {
		issue, err := s.GetIssue(context.Background(), id)
		if err != nil {
			t.Fatalf("GetIssue(%s): unexpected error: %v", id, err)
		}
		if issue.BoardStatus == string(contract.ColumnMerging) {
			mergingLeft++
		}
	}
	if mergingLeft != 2 {
		t.Errorf("cards still in merging = %d, want 2 (only 1 processed this tick)", mergingLeft)
	}
}
