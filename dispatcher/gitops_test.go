package dispatcher_test

import (
	"context"
	"strings"
	"testing"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/gitops"
	"github.com/xlyk/clipse/internal/linear"
)

// TestTick_MergingClaim_Mergeable_RoutesToDocumentation asserts a claimed
// merging card whose fake gitops run reports OutcomeMerged flows through
// the SAME applyTerminalOutcome/board.Next path a spawned worker's "done"
// result would: merging -> documentation, claim cleared, a setstate mirror
// enqueued for the new column.
func TestTick_MergingClaim_Mergeable_RoutesToDocumentation(t *testing.T) {
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
	if issue.BoardStatus != string(contract.ColumnDocumentation) {
		t.Errorf("BoardStatus = %q, want documentation", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}

	var sawDocSetState bool
	for _, c := range lc.SetStateCalls {
		if c.TargetColumn == string(contract.ColumnDocumentation) {
			sawDocSetState = true
		}
	}
	if !sawDocSetState {
		t.Errorf("SetState calls = %+v, want a mirror to documentation", lc.SetStateCalls)
	}

	// No spawned worker process for the merging column — gitops runs inline.
	if spawner.SpawnCount() != 0 {
		t.Errorf("SpawnCount = %d, want 0 (merging never spawns a worker)", spawner.SpawnCount())
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

	gitOpsFn := func(context.Context, gitops.Spec) (gitops.Result, error) {
		return gitops.Result{
			Outcome:          gitops.OutcomeStaleBaseConflict,
			Reason:           "branch still conflicts with base main after gh pr update-branch",
			ConflictingFiles: []string{"foo.go"},
		}, nil
	}
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsRunner(gitOpsFn),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
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
	if issue.BoardStatus != string(contract.ColumnDocumentation) {
		t.Errorf("BoardStatus = %q, want documentation (re-check found it mergeable)", issue.BoardStatus)
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
