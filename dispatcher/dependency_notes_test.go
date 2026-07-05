package dispatcher_test

import (
	"context"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/store"
)

// seedIssueWithDeps seeds a ready coder issue whose deps JSON names blockerIDs,
// so dependencyNotes has blockers to fetch comments for at claim time.
func seedIssueWithDeps(t *testing.T, s *store.Store, id string, deps string) {
	t.Helper()
	issue := store.Issue{
		ID:          id,
		Identifier:  id,
		LaneLabel:   "coder",
		BoardStatus: "ready",
		Deps:        deps,
		Priority:    1,
		BranchName:  id + "-branch",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(context.Background(), issue); err != nil {
		t.Fatalf("seed UpsertIssue(%s): unexpected error: %v", id, err)
	}
}

// seedTerminalIssue seeds a non-claimable issue (board_status=done) that exists
// only so its identifier resolves for a dependency-notes blocker heading.
func seedTerminalIssue(t *testing.T, s *store.Store, id string) {
	t.Helper()
	issue := store.Issue{
		ID:          id,
		Identifier:  id,
		LaneLabel:   "coder",
		BoardStatus: "done",
		Deps:        "[]",
		Priority:    1,
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}
	if err := s.UpsertIssue(context.Background(), issue); err != nil {
		t.Fatalf("seed UpsertIssue(%s): unexpected error: %v", id, err)
	}
}

// TestTick_CoderSpawn_InjectsDependencyNotes is the read side of the handoff
// loop: a coder claim carries its blockers' and its own Linear comments to the
// worker via CLIPSE_DEPENDENCY_NOTES, blockers first then the issue itself.
func TestTick_CoderSpawn_InjectsDependencyNotes(t *testing.T) {
	s := openTestStore(t)
	seedTerminalIssue(t, s, "blocker-1")
	seedIssueWithDeps(t, s, "issue-1", `["blocker-1"]`)

	lc := &linear.MockClient{
		Comments: map[string][]linear.Comment{
			"blocker-1": {
				{Body: "### coder handoff — done\n- schema uses integer epoch-ms timestamps", CreatedAt: "2026-07-01T00:00:00.000Z"},
			},
			"issue-1": {
				{Body: "reviewer asked for a retry with the config removed", CreatedAt: "2026-07-03T00:00:00.000Z"},
			},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, testConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1 (claim+spawn): unexpected error: %v", err)
	}

	var coderSpec *spec
	specs := spawner.Specs()
	for i := range specs {
		if specs[i].Issue == "issue-1" {
			coderSpec = &spec{env: specs[i].Env}
		}
	}
	if coderSpec == nil {
		t.Fatalf("no coder spawn recorded for issue-1 (specs=%d)", len(specs))
	}

	notes, ok := envValue(coderSpec.env, "CLIPSE_DEPENDENCY_NOTES")
	if !ok {
		t.Fatalf("coder spawn env missing CLIPSE_DEPENDENCY_NOTES (env=%v)", coderSpec.env)
	}
	if !strings.Contains(notes, "blocker-1 (blocker)") {
		t.Errorf("notes missing blocker section heading, got:\n%s", notes)
	}
	if !strings.Contains(notes, "schema uses integer epoch-ms timestamps") {
		t.Errorf("notes missing blocker comment body, got:\n%s", notes)
	}
	if !strings.Contains(notes, "issue-1 (this issue)") {
		t.Errorf("notes missing this-issue section heading, got:\n%s", notes)
	}
	if !strings.Contains(notes, "reviewer asked for a retry") {
		t.Errorf("notes missing this-issue comment body, got:\n%s", notes)
	}
	// Blockers come first (the decisions the ticket template says to read),
	// the issue's own comments last.
	if strings.Index(notes, "blocker-1 (blocker)") > strings.Index(notes, "issue-1 (this issue)") {
		t.Errorf("blocker section must precede this-issue section, got:\n%s", notes)
	}
}

// TestTick_CoderSpawn_DependencyNotesFetchFailureDegradesToUnset asserts a
// Linear fetch error never fails the spawn: the issue is still claimed and
// spawned, just without the CLIPSE_DEPENDENCY_NOTES env var.
func TestTick_CoderSpawn_DependencyNotesFetchFailureDegradesToUnset(t *testing.T) {
	s := openTestStore(t)
	seedIssueWithDeps(t, s, "issue-1", `[]`)

	lc := &linear.MockClient{IssueCommentsErr: errFakeLinearDown}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, testConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1 (claim+spawn): unexpected error: %v", err)
	}

	if spawner.SpawnCount() == 0 {
		t.Fatalf("issue was not spawned despite a dependency-notes fetch failure")
	}
	specs := spawner.Specs()
	if _, ok := envValue(specs[0].Env, "CLIPSE_DEPENDENCY_NOTES"); ok {
		t.Errorf("CLIPSE_DEPENDENCY_NOTES set despite fetch failure; want unset (env=%v)", specs[0].Env)
	}
}

// spec is a tiny shim so a test can hold onto just a spawned worker's env
// without copying the whole spawn.WorkerSpec.
type spec struct{ env []string }
