package tui_test

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/cli/tui"
	"github.com/xlyk/clipse/internal/store"
)

// buildSnapshot returns a hand-built store.Snapshot spanning the groupings
// the Model needs to derive: a running issue with an open run, a blocked
// issue, and both a "ready" and a "todo" issue that should fold into the
// same QUEUED group. No DB access — everything the Model needs is
// constructed in memory so Update stays testable without a TTY or store.
func buildSnapshot() store.Snapshot {
	return store.Snapshot{
		CountsByStatus: map[string]int{
			"running": 1,
			"blocked": 1,
			"ready":   1,
			"todo":    1,
		},
		Issues: []store.IssueSnapshot{
			{
				Issue: store.Issue{ID: "issue-1", Identifier: "CLP-1", LaneLabel: "agent:coder", BoardStatus: "running"},
				LatestRun: &store.Run{
					RunID: "run-1", IssueID: "issue-1", Status: "running",
					StartedAt: 100, TurnCount: 2, Attempt: 1, TokensIn: 10, TokensOut: 20,
				},
			},
			{
				Issue: store.Issue{ID: "issue-2", Identifier: "CLP-2", LaneLabel: "agent:reviewer", BoardStatus: "blocked"},
				LatestRun: &store.Run{
					RunID: "run-2", IssueID: "issue-2", Status: "blocked",
					StartedAt: 50, TurnCount: 1, Attempt: 1, TokensIn: 5, TokensOut: 7,
				},
			},
			{
				Issue:     store.Issue{ID: "issue-3", Identifier: "CLP-3", LaneLabel: "agent:coder", BoardStatus: "ready"},
				LatestRun: nil,
			},
			{
				Issue:     store.Issue{ID: "issue-4", Identifier: "CLP-4", LaneLabel: "agent:coder", BoardStatus: "todo"},
				LatestRun: nil,
			},
		},
	}
}

// TestUpdate_SnapshotMsg_FoldsGroupsAndTotals asserts that feeding a
// snapshotMsg into Update deterministically recomputes the
// running/blocked/queued groupings plus the token/count totals, with no DB
// access inside Update itself.
func TestUpdate_SnapshotMsg_FoldsGroupsAndTotals(t *testing.T) {
	m := tui.NewModel()
	snap := buildSnapshot()

	updated, cmd := m.Update(tui.SnapshotMsg{Snap: snap})
	if cmd != nil {
		t.Errorf("Update(snapshotMsg) cmd = %v, want nil (folding state should not itself schedule work)", cmd)
	}

	if got, want := len(updated.Running()), 1; got != want {
		t.Fatalf("Running() len = %d, want %d", got, want)
	}
	if got, want := updated.Running()[0].Identifier, "CLP-1"; got != want {
		t.Errorf("Running()[0].Identifier = %q, want %q", got, want)
	}

	if got, want := len(updated.Blocked()), 1; got != want {
		t.Fatalf("Blocked() len = %d, want %d", got, want)
	}
	if got, want := updated.Blocked()[0].Identifier, "CLP-2"; got != want {
		t.Errorf("Blocked()[0].Identifier = %q, want %q", got, want)
	}

	// QUEUED groups ready + todo together.
	queued := updated.Queued()
	if got, want := len(queued), 2; got != want {
		t.Fatalf("Queued() len = %d, want %d", got, want)
	}
	gotIDs := map[string]bool{queued[0].Identifier: true, queued[1].Identifier: true}
	for _, want := range []string{"CLP-3", "CLP-4"} {
		if !gotIDs[want] {
			t.Errorf("Queued() missing %q, got %+v", want, queued)
		}
	}

	if got, want := updated.TotalTokensIn(), 15; got != want {
		t.Errorf("TotalTokensIn() = %d, want %d", got, want)
	}
	if got, want := updated.TotalTokensOut(), 27; got != want {
		t.Errorf("TotalTokensOut() = %d, want %d", got, want)
	}

	if updated.Err() != nil {
		t.Errorf("Err() = %v, want nil after a clean snapshotMsg", updated.Err())
	}
}

// TestFold_DownstreamColumnsAppearInInFlightBucket asserts that issues
// sitting in review/rework/merging/documentation — active downstream
// columns a spawned worker or gitops is (or will be) working through — are
// visible in the TUI via a dedicated in-flight bucket, rather than silently
// dropped the way fold's original running/blocked/queued-only switch left
// them (none of those three cases match any of the four). A "done" issue
// (terminal, nothing left to watch) still shows up nowhere, exactly as
// before — this must be an explicit set of active columns, not a catch-all
// default that would also sweep in "done".
func TestFold_DownstreamColumnsAppearInInFlightBucket(t *testing.T) {
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			{Issue: store.Issue{ID: "i-review", Identifier: "CLP-10", LaneLabel: "agent:reviewer", BoardStatus: "review"}},
			{Issue: store.Issue{ID: "i-rework", Identifier: "CLP-11", LaneLabel: "agent:coder", BoardStatus: "rework"}},
			{Issue: store.Issue{ID: "i-merging", Identifier: "CLP-12", LaneLabel: "agent:git_operator", BoardStatus: "merging"}},
			{Issue: store.Issue{ID: "i-docs", Identifier: "CLP-13", LaneLabel: "agent:scribe", BoardStatus: "documentation"}},
			{Issue: store.Issue{ID: "i-done", Identifier: "CLP-14", LaneLabel: "agent:coder", BoardStatus: "done"}},
		},
	}

	m := tui.NewModel()
	updated, _ := m.Update(tui.SnapshotMsg{Snap: snap})

	inFlight := updated.InFlight()
	if got, want := len(inFlight), 4; got != want {
		t.Fatalf("InFlight() len = %d, want %d (review/rework/merging/documentation); got %+v", got, want, inFlight)
	}
	gotIDs := make(map[string]bool, len(inFlight))
	for _, row := range inFlight {
		gotIDs[row.Identifier] = true
	}
	for _, want := range []string{"CLP-10", "CLP-11", "CLP-12", "CLP-13"} {
		if !gotIDs[want] {
			t.Errorf("InFlight() missing %q, got %+v", want, inFlight)
		}
	}
	if gotIDs["CLP-14"] {
		t.Errorf("done issue CLP-14 leaked into InFlight(), want it to stay invisible (terminal)")
	}

	if got := len(updated.Running()); got != 0 {
		t.Errorf("Running() len = %d, want 0", got)
	}
	if got := len(updated.Blocked()); got != 0 {
		t.Errorf("Blocked() len = %d, want 0", got)
	}
	if got := len(updated.Queued()); got != 0 {
		t.Errorf("Queued() len = %d, want 0", got)
	}
}

// TestUpdate_SnapshotMsg_ClearsPriorError asserts that a fresh snapshotMsg
// clears any error recorded by a prior errMsg: a successful refresh should
// supersede a transient failure rather than sticking forever.
func TestUpdate_SnapshotMsg_ClearsPriorError(t *testing.T) {
	m := tui.NewModel()

	m, _ = m.Update(tui.ErrMsg{Err: assertErr{"boom"}})
	if m.Err() == nil {
		t.Fatalf("Err() = nil after errMsg, want an error")
	}

	m, _ = m.Update(tui.SnapshotMsg{Snap: buildSnapshot()})
	if m.Err() != nil {
		t.Errorf("Err() = %v after a subsequent clean snapshotMsg, want nil", m.Err())
	}
}

// TestUpdate_TickMsg_SchedulesRefresh asserts that a tickMsg produces a
// non-nil tea.Cmd (the injected refresh command), without Update itself
// touching the store.
func TestUpdate_TickMsg_SchedulesRefresh(t *testing.T) {
	called := false
	refresh := func() tea.Msg {
		called = true
		return tui.SnapshotMsg{Snap: buildSnapshot()}
	}

	m := tui.NewModel(tui.WithRefreshCmd(refresh))
	_, cmd := m.Update(tui.TickMsg{})
	if cmd == nil {
		t.Fatalf("Update(tickMsg) cmd = nil, want a non-nil tea.Cmd scheduling refresh")
	}

	// The tickMsg-returned cmd batches the injected refresh alongside the
	// next tick's scheduling (tea.Batch), so executing the top-level cmd
	// yields a tea.BatchMsg of sub-commands rather than calling refresh
	// directly. Run only the non-blocking sub-command (refresh); the other
	// is scheduleTick's real tea.Tick, which blocks for tickInterval when
	// invoked and is the runtime's job to await, not this unit test's.
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("cmd() = %T, want tea.BatchMsg", msg)
	}
	if got, want := len(batch), 2; got != want {
		t.Fatalf("batch has %d sub-commands, want %d (refresh + reschedule)", got, want)
	}
	batch[0]()
	if !called {
		t.Errorf("running the tickMsg batch's first sub-command did not invoke the injected refresh command")
	}
}

// TestUpdate_ErrMsg_RecordsError asserts that an errMsg is recorded on the
// model and surfaces via Err().
func TestUpdate_ErrMsg_RecordsError(t *testing.T) {
	m := tui.NewModel()
	wantErr := assertErr{"snapshot fetch failed"}

	updated, cmd := m.Update(tui.ErrMsg{Err: wantErr})
	if cmd != nil {
		t.Errorf("Update(errMsg) cmd = %v, want nil", cmd)
	}
	if updated.Err() == nil || updated.Err().Error() != wantErr.Error() {
		t.Errorf("Err() = %v, want %v", updated.Err(), wantErr)
	}
}

// TestUpdate_QuitKey_ReturnsTeaQuit asserts 'q' and ctrl+c both quit the
// program via tea.Quit.
func TestUpdate_QuitKey_ReturnsTeaQuit(t *testing.T) {
	for _, key := range []string{"q", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			m := tui.NewModel()
			var keyMsg tea.KeyMsg
			if key == "ctrl+c" {
				keyMsg = tea.KeyMsg{Type: tea.KeyCtrlC}
			} else {
				keyMsg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
			}

			_, cmd := m.Update(keyMsg)
			if cmd == nil {
				t.Fatalf("Update(key %q) cmd = nil, want tea.Quit", key)
			}
			msg := cmd()
			if _, ok := msg.(tea.QuitMsg); !ok {
				t.Errorf("Update(key %q) cmd() = %T, want tea.QuitMsg", key, msg)
			}
		})
	}
}

// assertErr is a minimal error implementation for tests that need a
// specific, comparable error value without importing errors.New into every
// call site.
type assertErr struct{ msg string }

func (e assertErr) Error() string { return e.msg }
