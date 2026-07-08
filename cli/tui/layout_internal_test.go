package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/store"
)

// snapWithNTodo builds a snapshot of n "todo" issues — a sparse board whose
// pipeline content is much shorter than a tall terminal's body.
func snapWithNTodo(n int) store.Snapshot {
	issues := make([]store.IssueSnapshot, 0, n)
	for i := 0; i < n; i++ {
		issues = append(issues, store.IssueSnapshot{
			Issue: store.Issue{
				ID:          fmt.Sprintf("id-%02d", i),
				Identifier:  fmt.Sprintf("CLI-%02d", i),
				LaneLabel:   "coder",
				BoardStatus: "todo",
			},
		})
	}
	return store.Snapshot{Issues: issues}
}

// TestDims_PipelineContentSizedOnTallTerminal asserts the P1 reflow: on a
// tall terminal with a sparse board (10 cards), the pipeline panel shrinks
// to its natural content height and the activity feed absorbs every
// remaining body row — well past the old fixed 18-row clamp.
func TestDims_PipelineContentSizedOnTallTerminal(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	m, _ = m.Update(SnapshotMsg{Snap: snapWithNTodo(10)})

	d := m.dims()
	natural := lipgloss.Height(m.renderBody(d.pipeTextW, 0)) + 3 // border(2) + title(1)
	if d.pipeH != natural {
		t.Errorf("pipeH = %d, want natural content height %d (content-sized pipeline)", d.pipeH, natural)
	}
	if got, want := d.actH, d.bodyH-d.pipeH; got != want {
		t.Errorf("actH = %d, want the full remainder %d (activity absorbs spare rows)", got, want)
	}
	if d.actH <= 18 {
		t.Errorf("actH = %d, want > 18 (the old fixed clamp) on a tall sparse board", d.actH)
	}
	if d.pipeH+d.actH != d.bodyH {
		t.Errorf("pipeH(%d) + actH(%d) != bodyH(%d) — panels must tile the body exactly", d.pipeH, d.actH, d.bodyH)
	}
}

// TestDims_PipelineCappedLeavesActivityMinimum asserts the other direction:
// a full board caps the pipeline at bodyH − actMin so the feed always keeps
// its minimum band, and the pipeline viewport scrolls the overflow.
func TestDims_PipelineCappedLeavesActivityMinimum(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = m.Update(SnapshotMsg{Snap: snapWithNTodo(40)})

	d := m.dims()
	if got, want := d.pipeH, d.bodyH-6; got != want {
		t.Errorf("pipeH = %d, want bodyH−actMin = %d when content overflows", got, want)
	}
	if got, want := d.actH, 6; got != want {
		t.Errorf("actH = %d, want the actMin floor %d", got, want)
	}
}

// TestRenderBody_OmitsEmptySections asserts empty sections vanish from the
// body — no "· none" filler, no empty band headings; their zero-counts live
// in the header chips instead.
func TestRenderBody_OmitsEmptySections(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: snapWithNTodo(3)})

	body := m.renderBody(100, 0)
	if strings.Contains(body, "· none") {
		t.Errorf("renderBody still renders %q filler:\n%s", "· none", body)
	}
	if strings.Contains(body, "BLOCKED") {
		t.Errorf("renderBody renders an empty BLOCKED band:\n%s", body)
	}
	if !strings.Contains(body, "QUEUED") {
		t.Errorf("renderBody dropped the non-empty QUEUED band:\n%s", body)
	}
}

// TestOrderedLineIndex_MatchesRenderBodyOnSparseBoard pins the render/index
// lockstep on a MIXED board: ACTIVE and QUEUED are populated while the
// section BETWEEN them (BLOCKED) is empty, so the empty-section skip fires
// mid-walk. For every ordered row, orderedLineIndex must return
// the exact line of that row inside renderBody's "\n"-joined output — the
// geometry ensureSelectionVisible scrolls by. A drift between the two skip
// predicates breaks j/k selection-follow, which is this task's main risk.
func TestOrderedLineIndex_MatchesRenderBodyOnSparseBoard(t *testing.T) {
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			{Issue: store.Issue{ID: "1", Identifier: "RUN-01", BoardStatus: "running"}},
			{Issue: store.Issue{ID: "2", Identifier: "RUN-02", BoardStatus: "running"}},
			{Issue: store.Issue{ID: "3", Identifier: "QUE-01", BoardStatus: "ready"}},
			{Issue: store.Issue{ID: "4", Identifier: "QUE-02", BoardStatus: "todo"}},
		},
	}
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: snap})

	if len(m.ordered) != 4 {
		t.Fatalf("ordered rows = %d, want 4", len(m.ordered))
	}
	inner := m.dims().pipeTextW
	lines := strings.Split(m.renderBody(inner, 0), "\n")
	for i, row := range m.ordered {
		want := -1
		for li, l := range lines {
			if strings.Contains(l, row.Identifier) {
				want = li
				break
			}
		}
		if want < 0 {
			t.Fatalf("row %s not found in renderBody output:\n%s", row.Identifier, strings.Join(lines, "\n"))
		}
		if got := m.orderedLineIndex(i); got != want {
			t.Errorf("orderedLineIndex(%d) = %d, want %d (the rendered line of %s)", i, got, want, row.Identifier)
		}
	}
}

// TestRenderBody_EmptyBoardPlaceholder asserts a board with no issues still
// renders something (never an empty string, which would collapse the panel).
func TestRenderBody_EmptyBoardPlaceholder(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	body := m.renderBody(100, 0)
	if !strings.Contains(body, "no issues") {
		t.Errorf("empty-board renderBody = %q, want a %q placeholder", body, "no issues")
	}
}
