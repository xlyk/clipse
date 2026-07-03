package tui_test

import (
	"database/sql"
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
		// Board-wide cumulative token totals (store.ReadSnapshot's SUM over all
		// runs); the TUI header reads these directly.
		TotalTokensIn:  15,
		TotalTokensOut: 27,
		Issues: []store.IssueSnapshot{
			{
				Issue: store.Issue{ID: "issue-1", Identifier: "CLP-1", LaneLabel: "agent:coder", BoardStatus: "running"},
				LatestRun: &store.Run{
					RunID: "run-1", IssueID: "issue-1", Status: "running",
					StartedAt: 100, TurnCount: 2, Attempt: 1, TokensIn: 10, TokensOut: 20,
				},
				TokensInTotal: 10, TokensOutTotal: 20,
			},
			{
				Issue: store.Issue{ID: "issue-2", Identifier: "CLP-2", LaneLabel: "agent:reviewer", BoardStatus: "blocked"},
				LatestRun: &store.Run{
					RunID: "run-2", IssueID: "issue-2", Status: "blocked",
					StartedAt: 50, TurnCount: 1, Attempt: 1, TokensIn: 5, TokensOut: 7,
				},
				TokensInTotal: 5, TokensOutTotal: 7,
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
// sitting in review/rework/merging — active downstream columns a spawned
// worker or gitops is (or will be) working through — are visible in the TUI
// via a dedicated in-flight bucket, rather than silently dropped the way
// fold's original running/blocked/queued-only switch left them. A "done"
// issue (terminal, nothing left to watch) still shows up nowhere — this must
// be an explicit set of active columns, not a catch-all default that would
// also sweep in "done".
func TestFold_DownstreamColumnsAppearInInFlightBucket(t *testing.T) {
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			{Issue: store.Issue{ID: "i-review", Identifier: "CLP-10", LaneLabel: "agent:reviewer", BoardStatus: "review"}},
			{Issue: store.Issue{ID: "i-rework", Identifier: "CLP-11", LaneLabel: "agent:coder", BoardStatus: "rework"}},
			{Issue: store.Issue{ID: "i-merging", Identifier: "CLP-12", LaneLabel: "agent:git_operator", BoardStatus: "merging"}},
			{Issue: store.Issue{ID: "i-done", Identifier: "CLP-14", LaneLabel: "agent:coder", BoardStatus: "done"}},
		},
	}

	m := tui.NewModel()
	updated, _ := m.Update(tui.SnapshotMsg{Snap: snap})

	inFlight := updated.InFlight()
	if got, want := len(inFlight), 3; got != want {
		t.Fatalf("InFlight() len = %d, want %d (review/rework/merging); got %+v", got, want, inFlight)
	}
	gotIDs := make(map[string]bool, len(inFlight))
	for _, row := range inFlight {
		gotIDs[row.Identifier] = true
	}
	for _, want := range []string{"CLP-10", "CLP-11", "CLP-12"} {
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

// TestFold_ActiveClaimMarksRowLiveWithWorkingLane asserts that liveness is
// per-row and keyed off the active claim, NOT the board_status: any card the
// dispatcher currently holds a claim on (a worker running in ANY lane) is
// Live, and its ActiveLane reports the lane actually working it — which for a
// downstream column differs from the issue's coder home label. A card parked
// in a downstream column with no active claim is not Live. This is what lets
// the dashboard show a spinner + the reviewer/git_operator badge +
// elapsed for every working agent, not just the coder-lane "running" one.
func TestFold_ActiveClaimMarksRowLiveWithWorkingLane(t *testing.T) {
	claimed := sql.NullString{String: "claim-tok", Valid: true}
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			// Coder actively running (claimed): live, working lane = coder.
			{
				Issue:     store.Issue{ID: "i-run", Identifier: "CLP-1", LaneLabel: "coder", BoardStatus: "running", ClaimLock: claimed},
				LatestRun: &store.Run{RunID: "r1", Lane: "coder", Status: "running", StartedAt: 100},
			},
			// Reviewer actively working a review card (claimed): live, working
			// lane = reviewer, even though the issue's home label is coder.
			{
				Issue:     store.Issue{ID: "i-rev", Identifier: "CLP-2", LaneLabel: "coder", BoardStatus: "review", ClaimLock: claimed},
				LatestRun: &store.Run{RunID: "r2", Lane: "reviewer", Status: "running", StartedAt: 100},
			},
			// Review card parked with no active claim (its latest run is the
			// completed coder handoff): NOT live.
			{
				Issue:     store.Issue{ID: "i-park", Identifier: "CLP-3", LaneLabel: "coder", BoardStatus: "review"},
				LatestRun: &store.Run{RunID: "r3", Lane: "coder", Status: "needs_review", StartedAt: 100},
			},
		},
	}

	m := tui.NewModel()
	updated, _ := m.Update(tui.SnapshotMsg{Snap: snap})

	byID := make(map[string]tui.Row)
	for _, r := range append(append([]tui.Row{}, updated.Running()...), updated.InFlight()...) {
		byID[r.Identifier] = r
	}

	if r := byID["CLP-1"]; !r.Live || r.ActiveLane != "coder" {
		t.Errorf("CLP-1 (running, claimed): Live=%v ActiveLane=%q, want true/\"coder\"", r.Live, r.ActiveLane)
	}
	if r := byID["CLP-2"]; !r.Live || r.ActiveLane != "reviewer" {
		t.Errorf("CLP-2 (review, claimed): Live=%v ActiveLane=%q, want true/\"reviewer\" (the working lane, not the coder home label)", r.Live, r.ActiveLane)
	}
	if r := byID["CLP-3"]; r.Live || r.ActiveLane != "" {
		t.Errorf("CLP-3 (review, unclaimed): Live=%v ActiveLane=%q, want false/\"\"", r.Live, r.ActiveLane)
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

// down/up/enter/esc/tab/help are keystroke constructors for the navigation
// tests, mirroring how bubbletea delivers them.
var (
	keyDown  = tea.KeyMsg{Type: tea.KeyDown}
	keyUp    = tea.KeyMsg{Type: tea.KeyUp}
	keyEnter = tea.KeyMsg{Type: tea.KeyEnter}
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
	keyTab   = tea.KeyMsg{Type: tea.KeyTab}
	keyHelp  = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")}
)

// TestSelectionNavigation_Clamps asserts j/k (down/up) walk the flattened
// ordered rows — running → in flight → blocked → queued — clamping at both
// ends rather than wrapping, and that the initial selection is the first row.
func TestSelectionNavigation_Clamps(t *testing.T) {
	m := tui.NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = m.Update(tui.SnapshotMsg{Snap: buildSnapshot()})

	// ordered = [CLP-1 (running), CLP-2 (blocked), CLP-3 (ready), CLP-4 (todo)].
	if got, want := m.Selected(), "CLP-1"; got != want {
		t.Fatalf("initial Selected() = %q, want %q", got, want)
	}

	wantDownSeq := []string{"CLP-2", "CLP-3", "CLP-4", "CLP-4"} // last press clamps
	for i, want := range wantDownSeq {
		m, _ = m.Update(keyDown)
		if got := m.Selected(); got != want {
			t.Errorf("after %d down presses Selected() = %q, want %q", i+1, got, want)
		}
	}

	wantUpSeq := []string{"CLP-3", "CLP-2", "CLP-1", "CLP-1"} // last press clamps
	for i, want := range wantUpSeq {
		m, _ = m.Update(keyUp)
		if got := m.Selected(); got != want {
			t.Errorf("after %d up presses Selected() = %q, want %q", i+1, got, want)
		}
	}
}

// TestSelectionSurvivesRefresh asserts the cursor stays pinned to the same
// issue identifier across a re-fold (a fresh SnapshotMsg), not to a slice
// index that reordering could shift out from under it.
func TestSelectionSurvivesRefresh(t *testing.T) {
	m := tui.NewModel()
	m, _ = m.Update(tui.SnapshotMsg{Snap: buildSnapshot()})
	m, _ = m.Update(keyDown) // -> CLP-2
	if got, want := m.Selected(), "CLP-2"; got != want {
		t.Fatalf("Selected() = %q, want %q", got, want)
	}

	m, _ = m.Update(tui.SnapshotMsg{Snap: buildSnapshot()})
	if got, want := m.Selected(), "CLP-2"; got != want {
		t.Errorf("after refresh Selected() = %q, want preserved %q", got, want)
	}
}

// TestViewModeToggling drives the mode/help transitions through Update key
// messages: tab flips dashboard↔kanban, enter opens detail and esc leaves it,
// ? toggles the help overlay, and esc closes help before backing out a view.
func TestViewModeToggling(t *testing.T) {
	m := tui.NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = m.Update(tui.SnapshotMsg{Snap: buildSnapshot()})

	if got, want := m.ViewMode(), "dashboard"; got != want {
		t.Fatalf("initial ViewMode() = %q, want %q", got, want)
	}

	// dashboard -> kanban -> dashboard
	m, _ = m.Update(keyTab)
	if got, want := m.ViewMode(), "kanban"; got != want {
		t.Errorf("after tab ViewMode() = %q, want %q", got, want)
	}
	m, _ = m.Update(keyTab)
	if got, want := m.ViewMode(), "dashboard"; got != want {
		t.Errorf("after 2nd tab ViewMode() = %q, want %q", got, want)
	}

	// dashboard -> detail -> (esc) dashboard
	m, _ = m.Update(keyEnter)
	if got, want := m.ViewMode(), "detail"; got != want {
		t.Errorf("after enter ViewMode() = %q, want %q", got, want)
	}
	m, _ = m.Update(keyEsc)
	if got, want := m.ViewMode(), "dashboard"; got != want {
		t.Errorf("after esc ViewMode() = %q, want %q", got, want)
	}

	// help overlay toggles independently of mode
	m, _ = m.Update(keyHelp)
	if !m.HelpVisible() {
		t.Error("after ? HelpVisible() = false, want true")
	}
	m, _ = m.Update(keyHelp)
	if m.HelpVisible() {
		t.Error("after 2nd ? HelpVisible() = true, want false")
	}

	// esc closes the help overlay first, without also leaving the view
	m, _ = m.Update(keyTab) // -> kanban
	m, _ = m.Update(keyHelp)
	if !m.HelpVisible() || m.ViewMode() != "kanban" {
		t.Fatalf("setup: HelpVisible=%v ViewMode=%q, want true/kanban", m.HelpVisible(), m.ViewMode())
	}
	m, _ = m.Update(keyEsc)
	if m.HelpVisible() {
		t.Error("esc did not close help overlay")
	}
	if got, want := m.ViewMode(), "kanban"; got != want {
		t.Errorf("esc closing help also changed ViewMode() to %q, want %q (unchanged)", got, want)
	}
}

// TestView_RendersWithoutPanicAcrossModes is a smoke test: View must produce
// non-empty output in every mode without panicking, including before any
// snapshot (empty board) and after one.
func TestView_RendersWithoutPanicAcrossModes(t *testing.T) {
	m := tui.NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})

	// empty board (no snapshot yet)
	if m.View() == "" {
		t.Error("View() on empty model returned empty string")
	}

	m, _ = m.Update(tui.SnapshotMsg{Snap: buildSnapshot(), Live: true})
	for _, msg := range []tea.KeyMsg{keyEnter, keyEsc, keyTab, keyHelp} {
		m, _ = m.Update(msg)
		if m.View() == "" {
			t.Errorf("View() returned empty after key %v", msg)
		}
	}
}

// assertErr is a minimal error implementation for tests that need a
// specific, comparable error value without importing errors.New into every
// call site.
type assertErr struct{ msg string }

func (e assertErr) Error() string { return e.msg }
