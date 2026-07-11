package dispatcher_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// zeroCapConfig returns a Config identical to testConfig but with every cap
// zeroed, so a Tick's selectAndClaim phase never claims anything — letting
// poll-focused tests observe pollAndUpsert's effect in isolation from the
// same-tick claim it would otherwise trigger.
func zeroCapConfig() config.Config {
	cfg := testConfig()
	cfg.Caps = config.Caps{}
	return cfg
}

// TestTick_PollCachesCandidatesFromLinear asserts pollAndUpsert caches every
// LABELED candidate Linear returns, mapping Lane->lane_label and
// Status->board_status on the initial insert -- and drops unlabeled issues
// entirely (see TestTick_UnlabeledIssuesNeverIngested for the full incident
// shape).
func TestTick_PollCachesCandidatesFromLinear(t *testing.T) {
	s := openTestStore(t)
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Title: "Add the thing", Description: "Implement the thing.", Status: "ready", Lane: "coder", Priority: 1, BranchName: "clp-1", UpdatedAt: 100},
			{ID: "issue-2", Identifier: "CLP-2", Status: "todo", Lane: "", Deps: []string{"issue-1"}, Priority: 0, BranchName: "clp-2", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	byID := map[string]string{}
	byLane := map[string]string{}
	for _, is := range snap.Issues {
		byID[is.ID] = is.BoardStatus
		byLane[is.ID] = is.LaneLabel
	}
	if byID["issue-1"] != "ready" {
		t.Errorf("issue-1 board_status = %q, want ready", byID["issue-1"])
	}
	if byLane["issue-1"] != "coder" {
		t.Errorf("issue-1 lane_label = %q, want coder", byLane["issue-1"])
	}
	// issue-2 has no lane label: it never opted into clipse, so it must not
	// be ingested at all -- not even as an inert row.
	if _, ok := byID["issue-2"]; ok {
		t.Errorf("issue-2 was ingested (board_status %q), want dropped at poll", byID["issue-2"])
	}

	// title/description must flow from Linear into the store (Phase-2
	// issue-text plumbing): this is what lets a later claim carry them into
	// the worker's CLIPSE_ISSUE_TEXT. ReadSnapshot's Issue projection
	// doesn't select these columns, so read back via GetIssue instead.
	got1, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue(issue-1): unexpected error: %v", err)
	}
	if got1.Title != "Add the thing" {
		t.Errorf("issue-1 Title = %q, want %q", got1.Title, "Add the thing")
	}
	if got1.Description != "Implement the thing." {
		t.Errorf("issue-1 Description = %q, want %q", got1.Description, "Implement the thing.")
	}
}

// TestTick_UnlabeledIssuesNeverIngested pins the 2026-07-08 Spacelift
// incident shape: on a shared team board, unlabeled issues include real
// teammates' work, and some sit in states whose NAMES collide with board
// columns ("In Review" -> review). Ingesting them let promote move their
// Linear cards (todo -> Ready, outbox-mirrored) and let the column-claiming
// lanes (review/rework/merging claim WITHOUT a lane filter) claim and work
// them. The lane label is the opt-in gate, so an issue without one must be
// invisible to the entire kernel: never upserted, never promoted, never
// claimed, and never the subject of a Linear write.
func TestTick_UnlabeledIssuesNeverIngested(t *testing.T) {
	s := openTestStore(t)
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			// A teammate's ticket mid-human-review: state name collides with
			// the review column -- the exact hijack path from the incident.
			{ID: "stray-review", Identifier: "SPA-876", Title: "Teammate work", Status: "review", Lane: "", Priority: 2, BranchName: "spa-876", UpdatedAt: 100},
			// A teammate's backlog ticket: promote would move it to ready
			// and mirror that move to Linear.
			{ID: "stray-todo", Identifier: "SPA-882", Title: "Backlog thing", Status: "todo", Lane: "", Priority: 3, BranchName: "spa-882", UpdatedAt: 100},
			// The one opted-in ticket.
			{ID: "labeled-1", Identifier: "SPA-854", Title: "Ours", Status: "todo", Lane: "coder", Priority: 3, BranchName: "spa-854", UpdatedAt: 100},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if len(snap.Issues) != 1 {
		ids := make([]string, 0, len(snap.Issues))
		for _, is := range snap.Issues {
			ids = append(ids, is.Identifier)
		}
		t.Fatalf("ingested issues = %v, want exactly [SPA-854]", ids)
	}
	if snap.Issues[0].ID != "labeled-1" {
		t.Errorf("ingested issue = %q, want labeled-1", snap.Issues[0].ID)
	}

	// No Linear write may ever target an unlabeled issue -- the incident's
	// visible damage was 24 teammates' cards moved on the shared board.
	writes, err := s.DrainPendingLinearWrites(context.Background(), 100)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	for _, w := range writes {
		if w.IssueID == "stray-review" || w.IssueID == "stray-todo" {
			t.Errorf("linear write enqueued for unlabeled issue %s (kind %s)", w.IssueID, w.Kind)
		}
	}
}

// TestTick_RepollPreservesRunningBoardStatus asserts that once an issue is
// claimed and running, a later poll returning its old Linear status (e.g.
// "ready", since Linear hasn't been mirrored to "running" from Linear's own
// perspective at poll time) does not reset board_status away from running.
// UpsertIssue's own conflict semantics guarantee this; this test exercises
// it through a full Tick pass.
func TestTick_RepollPreservesRunningBoardStatus(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "clp-1-branch", UpdatedAt: 100},
		},
	}
	spawner := newFakeSpawner()
	// Every re-spawn of issue-1 (Linear identifier CLP-1) reports "continue"
	// with a distant turn cap, so the run stays inflight/running across both
	// ticks rather than resolving to a terminal transition mid-test.
	spawner.Results["CLP-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeContinue, ThreadId: "thread-1"},
	}
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.TurnCap = 1000
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	// First tick: claims and spawns issue-1 (moves it to running).
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap.Issues[0].BoardStatus != "running" {
		t.Fatalf("after first tick, board_status = %q, want running", snap.Issues[0].BoardStatus)
	}

	// Second tick re-polls the same stale "ready" status from Linear.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: unexpected error: %v", err)
	}

	snap2, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	if snap2.Issues[0].BoardStatus != "running" {
		t.Errorf("after second tick (repoll), board_status = %q, want preserved running", snap2.Issues[0].BoardStatus)
	}
}

// TestTick_PollAdoptsHumanMoveWhenUnclaimed asserts A3's adoption rule: when
// an existing issue's SQLite board_status diverges from what Linear now
// reports, and the issue holds no active claim, the poll adopts the human
// move — SQLite is updated to match Linear (no run to close, since nothing
// was in flight) — and the issue becomes claimable in that new state on the
// very same tick (zero caps here isolate the adoption from any claim, but a
// direct read confirms the adopted status is 'ready').
func TestTick_PollAdoptsHumanMoveWhenUnclaimed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Seed issue-1 as 'blocked' with no claim, as if a prior run blocked it.
	issue := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		LaneLabel:   "coder",
		BoardStatus: "blocked",
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "issue-1-branch",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	// A human moved the issue back to Ready in Linear.
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	// Zero caps so this tick's own selectAndClaim doesn't immediately claim
	// the newly-adopted ready issue — we want to observe the adoption alone.
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Errorf("BoardStatus = %q, want ready (adopted human move)", got.BoardStatus)
	}

	// Adoption does not mirror back to Linear (Linear already holds this
	// state) and does not close/open any run.
	pending, err := s.DrainPendingLinearWrites(ctx, 100)
	if err != nil {
		t.Fatalf("DrainPendingLinearWrites: unexpected error: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending linear writes = %d, want 0 (adoption does not mirror back)", len(pending))
	}

	// The adoption is claimable on a later tick: re-tick with real caps and
	// confirm it gets claimed.
	cfg := testConfig()
	d2 := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))
	if err := d2.Tick(ctx); err != nil {
		t.Fatalf("second Tick: unexpected error: %v", err)
	}
	got2, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue after second tick: unexpected error: %v", err)
	}
	if got2.BoardStatus != "running" {
		t.Errorf("BoardStatus after second tick = %q, want running (adopted issue was claimable)", got2.BoardStatus)
	}
}

func TestTick_PendingTerminalSetStatePreventsBackwardAdoptionAndStillConverges(t *testing.T) {
	for _, terminalStatus := range []string{"done", "cancelled"} {
		t.Run(terminalStatus, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			seedColumnIssue(t, s, "issue-1", terminalStatus, 1, 100)
			seedAgentWorkspace(t, s, store.AgentWorkspace{
				OwnerKey: "local:coder:issue-1", IssueID: "issue-1", Provider: "local", Role: "coder",
				WorkspacePath: "/workspace", State: store.WorkspaceCleanupPending,
				LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
			})
			if err := s.EnqueueLinearSetState(ctx, "issue-1", terminalStatus, 200); err != nil {
				t.Fatal(err)
			}
			writes, err := s.DrainPendingLinearWrites(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.MarkLinearWriteFailed(ctx, writes[0].ID, "prior mirror outage", 300); err != nil {
				t.Fatal(err)
			}

			lc := &linear.MockClient{Issues: []linear.Issue{{
				ID: "issue-1", Identifier: "CLP-1", Status: "todo", Lane: "coder", Priority: 1,
				BranchName: "issue-1-branch", UpdatedAt: 400,
			}}}
			ws := newStubWorkspacer(t.TempDir())
			cfg := zeroCapConfig()
			cfg.AgentBackend.Type = "local"
			// A fresh Dispatcher models restart: only SQLite and the outbox carry
			// knowledge of the committed terminal transition.
			d := newTestDispatcher(t, cfg, s, lc, newFakeSpawner(), ws, fixedClock(1000))

			if err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			issue := getIssue(t, s, "issue-1")
			if issue.BoardStatus != terminalStatus {
				t.Fatalf("board status = %q, want terminal %q preserved", issue.BoardStatus, terminalStatus)
			}
			workspace := mustAgentWorkspace(t, s, "issue-1", "local:coder:issue-1")
			if workspace.State != store.WorkspaceDeleted {
				t.Fatalf("cleanup did not proceed while mirror converged: %+v", workspace)
			}
			if got := ws.RemovedIssues(); !slices.Equal(got, []string{"issue-1"}) {
				t.Fatalf("Remove calls = %v", got)
			}
			if len(lc.SetStateCalls) != 1 || lc.SetStateCalls[0].TargetColumn != terminalStatus {
				t.Fatalf("SetState calls = %+v, want terminal convergence", lc.SetStateCalls)
			}
			pending, err := s.DrainPendingLinearWrites(ctx, 10)
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending writes after convergence = %+v, err=%v", pending, err)
			}
		})
	}
}

func TestTick_OutboxFailureCannotReorderTerminalSetState(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnDone), 1, 100)
	if err := s.EnqueueLinearSetState(ctx, "issue-1", string(contract.ColumnReady), 200); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueLinearSetState(ctx, "issue-1", string(contract.ColumnDone), 300); err != nil {
		t.Fatal(err)
	}
	lc := &failFirstSetStateLinear{status: string(contract.ColumnReady)}
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, newFakeSpawner(), newStubWorkspacer(t.TempDir()), fixedClock(1000))

	for tick := 1; tick <= 4; tick++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	issue := getIssue(t, s, "issue-1")
	if issue.BoardStatus != string(contract.ColumnDone) {
		t.Fatalf("out-of-order mirror moved SQLite backward to %q, want done", issue.BoardStatus)
	}
	if lc.status != string(contract.ColumnDone) {
		t.Fatalf("final Linear status = %q, want done", lc.status)
	}
	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending writes = %+v, err=%v", pending, err)
	}
}

func TestTick_OutboxFailureBlocksOnlySameIssue(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for _, item := range []struct {
		issue  string
		target string
	}{
		{issue: "issue-a", target: "ready"},
		{issue: "issue-a", target: "done"},
		{issue: "issue-b", target: "review"},
	} {
		if err := s.EnqueueLinearSetState(ctx, item.issue, item.target, 100); err != nil {
			t.Fatal(err)
		}
	}
	lc := &failIssueLinear{failIssue: "issue-a"}
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, newFakeSpawner(), newStubWorkspacer(t.TempDir()), fixedClock(1000))
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(lc.calls, []string{"issue-a:ready", "issue-b:review"}) {
		t.Fatalf("SetState calls = %v, want issue-a head then independent issue-b; issue-a done must not overtake", lc.calls)
	}
	pending, err := s.DrainPendingLinearWrites(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].IssueID != "issue-a" || pending[0].Target != "ready" || pending[1].IssueID != "issue-a" || pending[1].Target != "done" {
		t.Fatalf("pending after per-issue drain = %+v", pending)
	}
}

func TestTick_OutboxFairnessAdmitsHealthyIssueAfterFailedBatch(t *testing.T) {
	const batchLimit = 100 // dispatcher.drainOutboxLimit
	ctx := context.Background()
	s := openTestStore(t)
	for i := range batchLimit {
		issueID := fmt.Sprintf("issue-fail-%03d", i)
		if err := s.EnqueueLinearSetState(ctx, issueID, "ready", int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnqueueLinearSetState(ctx, "issue-healthy", "done", 1000); err != nil {
		t.Fatal(err)
	}
	lc := &failAllButHealthyLinear{}
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, newFakeSpawner(), newStubWorkspacer(t.TempDir()), fixedClock(2000))

	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(lc.calls) != batchLimit {
		t.Fatalf("tick 1 calls = %d, want saturated batch %d", len(lc.calls), batchLimit)
	}
	if slices.Contains(lc.calls, "issue-healthy:done") {
		t.Fatal("healthy issue unexpectedly fit in saturated first batch")
	}
	firstTickCalls := len(lc.calls)
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(lc.calls[firstTickCalls:], "issue-healthy:done") {
		t.Fatalf("tick 2 retried only failed heads; calls=%v", lc.calls[firstTickCalls:])
	}
	pending, err := s.DrainPendingLinearWrites(ctx, batchLimit+1)
	if err != nil {
		t.Fatal(err)
	}
	for _, write := range pending {
		if write.IssueID == "issue-healthy" {
			t.Fatalf("healthy issue remained pending after tick 2: %+v", write)
		}
	}
}

type failFirstSetStateLinear struct {
	status string
	failed bool
}

type failIssueLinear struct {
	failIssue string
	calls     []string
}

type failAllButHealthyLinear struct {
	calls []string
}

func (*failAllButHealthyLinear) CandidateIssues(context.Context) ([]linear.Issue, error) {
	return nil, nil
}

func (c *failAllButHealthyLinear) SetState(_ context.Context, issueID, target string) error {
	c.calls = append(c.calls, issueID+":"+target)
	if issueID != "issue-healthy" {
		return errors.New("permanent failure")
	}
	return nil
}

func (*failAllButHealthyLinear) Comment(context.Context, string, string) error { return nil }

func (*failAllButHealthyLinear) IssueComments(context.Context, string) ([]linear.Comment, error) {
	return nil, nil
}

func (*failIssueLinear) CandidateIssues(context.Context) ([]linear.Issue, error) { return nil, nil }

func (c *failIssueLinear) SetState(_ context.Context, issueID, target string) error {
	c.calls = append(c.calls, issueID+":"+target)
	if issueID == c.failIssue {
		return errors.New("permanent issue-specific failure")
	}
	return nil
}

func (*failIssueLinear) Comment(context.Context, string, string) error { return nil }

func (*failIssueLinear) IssueComments(context.Context, string) ([]linear.Comment, error) {
	return nil, nil
}

func (c *failFirstSetStateLinear) CandidateIssues(context.Context) ([]linear.Issue, error) {
	return []linear.Issue{{
		ID: "issue-1", Identifier: "CLP-1", Status: c.status, Lane: "coder", Priority: 1,
		BranchName: "issue-1-branch", UpdatedAt: 400,
	}}, nil
}

func (c *failFirstSetStateLinear) SetState(_ context.Context, _ string, target string) error {
	if !c.failed {
		c.failed = true
		return errors.New("first mirror attempt failed")
	}
	c.status = target
	return nil
}

func (*failFirstSetStateLinear) Comment(context.Context, string, string) error { return nil }

func (*failFirstSetStateLinear) IssueComments(context.Context, string) ([]linear.Comment, error) {
	return nil, nil
}

// TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsReworkCount asserts the
// fix for a stale rework_count surviving a human requeue: adopting a
// blocked->ready move (A3, unclaimed) resets issues.rework_count to zero.
// Without this, an issue blocked after tripping amendment C1's rework_cap
// keeps whatever rework_count it accumulated on its PRIOR review/rework
// cycle, so a human's very next requeue could immediately re-trip
// blockIfReworkCapExceeded on the first subsequent changes_requested —
// defeating the point of requeuing it by hand.
func TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsReworkCount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Seed issue-1 as 'blocked' with a stale rework_count left over from a
	// prior rework-cap trip, and no active claim.
	issue := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		LaneLabel:   "coder",
		BoardStatus: "blocked",
		ReworkCount: 3,
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "issue-1-branch",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	// A human moved the issue back to Ready in Linear.
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Fatalf("BoardStatus = %q, want ready (adopted human move)", got.BoardStatus)
	}
	if got.ReworkCount != 0 {
		t.Errorf("ReworkCount = %d, want reset to 0 on human requeue from blocked", got.ReworkCount)
	}
}

// TestTick_PollAdoptsHumanMove_FromNonBlocked_DoesNotResetReworkCount
// asserts the reset above is scoped to a blocked->{ready,todo} requeue
// specifically: an ordinary human-adopted move that doesn't originate from
// Blocked must leave rework_count untouched.
func TestTick_PollAdoptsHumanMove_FromNonBlocked_DoesNotResetReworkCount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	issue := store.Issue{
		ID:          "issue-1",
		Identifier:  "CLP-1",
		LaneLabel:   "coder",
		BoardStatus: "todo",
		ReworkCount: 2,
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "issue-1-branch",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Fatalf("BoardStatus = %q, want ready (adopted human move)", got.BoardStatus)
	}
	if got.ReworkCount != 2 {
		t.Errorf("ReworkCount = %d, want unchanged 2 (adoption did not originate from blocked)", got.ReworkCount)
	}
}

// TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsRecoverAttemptsToo asserts
// the fix for finding 5(b): a human requeue out of Blocked resets
// recover_attempts and clears blocked_until, the same way it already resets
// rework_count -- otherwise a card auto-retried close to RecoverCap before
// parking keeps that near-exhausted budget after a human's fresh requeue.
func TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsRecoverAttemptsToo(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	issue := store.Issue{
		ID: "issue-1", Identifier: "CLP-1", LaneLabel: "coder", BoardStatus: "blocked",
		RecoverAttempts: 2, BlockedUntil: 5000,
		Deps: `[]`, Priority: 1, BranchName: "issue-1-branch",
		UpdatedAt: 100, LastSeen: 100, CreatedAt: 100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Fatalf("BoardStatus = %q, want ready (adopted human move)", got.BoardStatus)
	}
	if got.RecoverAttempts != 0 {
		t.Errorf("RecoverAttempts = %d, want reset to 0 on human requeue from blocked", got.RecoverAttempts)
	}
	if got.BlockedUntil != 0 {
		t.Errorf("BlockedUntil = %d, want cleared to 0 on human requeue from blocked", got.BlockedUntil)
	}
}

// TestTick_PollReassertsDispatcherOwnedStateWhenClaimed asserts A3's other
// half: when an issue's SQLite board_status diverges from Linear's polled
// status BUT the issue holds an active claim (the dispatcher owns it right
// now), the dispatcher does not adopt Linear's stale view — it re-asserts
// its own truth by enqueueing a setstate mirror back to the SQLite status,
// and board_status itself is left untouched.
func TestTick_PollReassertsDispatcherOwnedStateWhenClaimed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	claim, err := s.ClaimReady(ctx, "coder", "run-1", 100, 3600)
	if err != nil {
		t.Fatalf("ClaimReady: unexpected error: %v", err)
	}
	if claim.Issue.BoardStatus != "running" {
		t.Fatalf("precondition: claimed issue status = %q, want running", claim.Issue.BoardStatus)
	}

	// Linear still reports the pre-claim "ready" status (its own mirror
	// write for the claim hasn't landed/been observed yet from Linear's
	// perspective at poll time).
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	// The inflight run must not resolve mid-test: script "continue" so
	// nothing else changes board_status out from under this assertion.
	spawner.Results["CLP-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeContinue, ThreadId: "thread-1"},
	}
	ws := newStubWorkspacer(t.TempDir())
	cfg := zeroCapConfig()
	cfg.TurnCap = 1000
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "running" {
		t.Errorf("BoardStatus = %q, want running (dispatcher-owned state preserved, not reset to Linear's stale ready)", got.BoardStatus)
	}

	// The reassert enqueue is drained within the same Tick (drainOutbox is
	// the last phase), so assert on the MockClient's recorded SetState calls
	// rather than on still-pending rows.
	var sawRunningReassert bool
	for _, c := range lc.SetStateCalls {
		if c.IssueID == "issue-1" && c.TargetColumn == "running" {
			sawRunningReassert = true
		}
	}
	if !sawRunningReassert {
		t.Errorf("SetStateCalls = %+v, want a setstate -> running reassertion", lc.SetStateCalls)
	}
}

// TestTick_PollNeverAdoptsRunningWithoutClaim asserts the fix for adopting an
// unclaimed "running" status from Linear: a human dragging a card to Running
// (or a restart-requeue race observing a stale label) must not be adopted --
// board_status='running' is entered ONLY via the CAS claim (store.ClaimReady/
// ClaimColumn). Adopting it here would write claim_lock=NULL, board_status=
// 'running': unclaimable by ClaimReady's CAS (which requires board_status=
// 'ready') and unreleasable by ReleaseStaleClaims (which only looks at
// claim_expires, permanently NULL on this row).
func TestTick_PollNeverAdoptsRunningWithoutClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// issue-1 sits at 'ready' with no active claim.
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	// Linear reports "running" for this issue with no backing claim.
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "running", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Fatalf("BoardStatus = %q, want unchanged ready (an unclaimed running status must never be adopted)", got.BoardStatus)
	}
	if got.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want still unclaimed")
	}

	// Instead of adopting, the dispatcher corrects Linear's stale view: a
	// setstate mirror pushing the store's real status (ready) back.
	var sawReadyReassert bool
	for _, c := range lc.SetStateCalls {
		if c.IssueID == "issue-1" && c.TargetColumn == "ready" {
			sawReadyReassert = true
		}
	}
	if !sawReadyReassert {
		t.Errorf("SetStateCalls = %+v, want a setstate -> ready reassertion correcting Linear's stray running", lc.SetStateCalls)
	}

	// The issue remains genuinely claimable on a later tick with real caps.
	cfg := testConfig()
	d2 := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))
	if err := d2.Tick(ctx); err != nil {
		t.Fatalf("second Tick: unexpected error: %v", err)
	}
	got2, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue after second tick: unexpected error: %v", err)
	}
	if got2.BoardStatus != "running" {
		t.Errorf("BoardStatus after second tick = %q, want running (claimed for real this time)", got2.BoardStatus)
	}
	if !got2.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = false after second tick, want a real claim backing running")
	}
}
