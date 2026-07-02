package dispatcher_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
)

// TestTick_ReviewColumnClaim_DispatchesReviewerLane asserts an unclaimed
// "review" card is claimed and dispatched to the Reviewer lane (not the
// issue's own — always "coder" — lane_label), and a "done" (pass) result
// routes it on to merging.
func TestTick_ReviewColumnClaim_DispatchesReviewerLane(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "review", 1, 100)

	spawner := newFakeSpawner()
	prURL := "https://github.com/x/y/pull/1"
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeDone, Summary: "LGTM", PrUrl: &prURL},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, testConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1", len(specs))
	}
	if specs[0].Lane != string(contract.LaneReviewer) {
		t.Errorf("spawn Lane = %q, want %q", specs[0].Lane, contract.LaneReviewer)
	}

	// Claiming a review card does not itself move it off review.
	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnReview) {
		t.Errorf("BoardStatus after claim = %q, want unchanged review", issue.BoardStatus)
	}
	if !issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = false, want claimed")
	}

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: unexpected error: %v", err)
	}

	issue2, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue2.BoardStatus != string(contract.ColumnMerging) {
		t.Errorf("BoardStatus after reviewer pass = %q, want merging", issue2.BoardStatus)
	}
}

// TestTick_DocumentationColumnClaim_DispatchesScribeLane asserts an
// unclaimed "documentation" card is claimed and dispatched to the Scribe
// lane, and a "done" result reaches the terminal Done column with
// rework_count reset to 0.
func TestTick_DocumentationColumnClaim_DispatchesScribeLane(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", "documentation", 1, 100)

	spawner := newFakeSpawner()
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeDone, Summary: "no docs needed"},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, testConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1", len(specs))
	}
	if specs[0].Lane != string(contract.LaneScribe) {
		t.Errorf("spawn Lane = %q, want %q", specs[0].Lane, contract.LaneScribe)
	}

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: unexpected error: %v", err)
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
	if issue.ReworkCount != 0 {
		t.Errorf("ReworkCount = %d, want reset to 0 on done", issue.ReworkCount)
	}
}

// TestTick_ColumnClaim_DoesNotMirrorLinearForTheClaimItself asserts R5: a
// downstream column claim (review/rework/documentation) never enqueues a
// Linear mirror write on its own — only a later Transition (once the
// claimed lane's result comes back) does. Every scripted result hangs
// forever so no transition ever fires, isolating the claim's own effect on
// the outbox.
func TestTick_ColumnClaim_DoesNotMirrorLinearForTheClaimItself(t *testing.T) {
	for _, column := range []string{"review", "rework", "documentation"} {
		t.Run(column, func(t *testing.T) {
			s := openTestStore(t)
			seedColumnIssue(t, s, "issue-1", column, 1, 100)

			spawner := newFakeSpawner()
			spawner.Results["issue-1"] = spawn.Result{Err: context.DeadlineExceeded}
			ws := newStubWorkspacer(t.TempDir())
			lc := &linear.MockClient{}
			cfg := testConfig()
			cfg.MaxRuntimeS = 3600
			d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

			if err := d.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: unexpected error: %v", err)
			}

			issue, err := s.GetIssue(context.Background(), "issue-1")
			if err != nil {
				t.Fatalf("GetIssue: unexpected error: %v", err)
			}
			if !issue.ClaimLock.Valid {
				t.Fatalf("precondition: issue should be claimed after tick 1")
			}

			pending, err := s.DrainPendingLinearWrites(context.Background(), 100)
			if err != nil {
				t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
			}
			if len(pending) != 0 {
				t.Errorf("pending linear_writes = %+v, want 0 (a column claim must not enqueue a Linear mirror)", pending)
			}
			if len(lc.SetStateCalls) != 0 {
				t.Errorf("SetState calls = %+v, want 0", lc.SetStateCalls)
			}
		})
	}
}

// TestTick_CoderPool_SharesCapAcrossReadyAndRework asserts R4: with
// Caps.PerLane.Coder=1 and exactly one ready candidate plus one rework
// candidate present, both are eventually claimed across ticks — neither
// column starves the other, even though only one claim is possible per
// tick under the cap.
func TestTick_CoderPool_SharesCapAcrossReadyAndRework(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-ready", "coder", 1, 100)
	seedColumnIssue(t, s, "issue-rework", "rework", 1, 100)

	spawner := newFakeSpawner()
	spawner.Results["issue-ready"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "opened PR"},
	}
	spawner.Results["issue-rework"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "addressed review"},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.Caps = config.Caps{
		Global:  1,
		PerLane: config.PerLaneCaps{Coder: 1, Reviewer: 0, GitOperator: 0, Scribe: 0},
	}
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	// Run enough ticks for the cap=1 pool to process both candidates one at
	// a time: claim+spawn, drain+transition, claim+spawn the other,
	// drain+transition.
	for i := 0; i < 8; i++ {
		if err := d.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: unexpected error: %v", i, err)
		}
	}

	claimedIdentifiers := make(map[string]bool)
	for _, spec := range spawner.Specs() {
		claimedIdentifiers[spec.Issue] = true
	}
	if !claimedIdentifiers["issue-ready"] {
		t.Errorf("issue-ready was never claimed/spawned — starved by the rework pool")
	}
	if !claimedIdentifiers["issue-rework"] {
		t.Errorf("issue-rework was never claimed/spawned — starved by the ready pool")
	}

	readyIssue, err := s.GetIssue(context.Background(), "issue-ready")
	if err != nil {
		t.Fatalf("GetIssue(issue-ready): unexpected error: %v", err)
	}
	if readyIssue.BoardStatus != string(contract.ColumnReview) {
		t.Errorf("issue-ready BoardStatus = %q, want review", readyIssue.BoardStatus)
	}
	reworkIssue, err := s.GetIssue(context.Background(), "issue-rework")
	if err != nil {
		t.Fatalf("GetIssue(issue-rework): unexpected error: %v", err)
	}
	if reworkIssue.BoardStatus != string(contract.ColumnReview) {
		t.Errorf("issue-rework BoardStatus = %q, want review", reworkIssue.BoardStatus)
	}

	// The shared cap was never exceeded: at most 1 coder run in flight at
	// any spawn point (SpawnCount only ever grows one at a time given
	// Global=1, PerLane.Coder=1 — checked implicitly by both issues
	// reaching review without a caps violation surfacing as an error).
	if spawner.SpawnCount() != 2 {
		t.Errorf("SpawnCount = %d, want exactly 2 (one claim per issue, no re-claims)", spawner.SpawnCount())
	}
}

// TestTick_CoderPool_PrefersHigherPriorityAcrossColumns asserts the coder
// pool's cross-column ordering (R4): with cap=1 and a candidate in each of
// ready and rework, the more urgent one (by the same priority/created_at/
// identifier rule store.selectClaimCandidate uses within one column) is
// claimed first, regardless of which column it's in.
func TestTick_CoderPool_PrefersHigherPriorityAcrossColumns(t *testing.T) {
	s := openTestStore(t)
	// ready: priority "low" (4). rework: priority "urgent" (1) — more
	// urgent despite being created later and sitting in the "other" column.
	seedReadyIssue(t, s, "issue-ready-low", "coder", 4, 100)
	seedColumnIssue(t, s, "issue-rework-urgent", "rework", 1, 200)

	spawner := newFakeSpawner()
	spawner.Results["issue-ready-low"] = spawn.Result{Err: context.DeadlineExceeded}
	spawner.Results["issue-rework-urgent"] = spawn.Result{Err: context.DeadlineExceeded}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.MaxRuntimeS = 3600
	cfg.Caps = config.Caps{
		Global:  1,
		PerLane: config.PerLaneCaps{Coder: 1, Reviewer: 0, GitOperator: 0, Scribe: 0},
	}
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1 (cap=1)", len(specs))
	}
	if specs[0].Issue != "issue-rework-urgent" {
		t.Errorf("claimed %q, want %q (more urgent, across columns)", specs[0].Issue, "issue-rework-urgent")
	}
}
