package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/store"
)

// TestRowDetail_ReworkAndRecoverChip asserts the ⟳ chip renders the rework
// count and rides the recover-attempts counter on the same chip (P4):
// ⟳ ×<rework> r<recover>, each part present only when > 0.
func TestRowDetail_ReworkAndRecoverChip(t *testing.T) {
	m := NewModel()
	tests := []struct {
		name string
		row  Row
		want []string
		not  []string
	}{
		{"rework only", Row{Identifier: "CLI-1", Status: "rework", ReworkCount: 1}, []string{"⟳ ×1"}, []string{"r1"}},
		{"recover only", Row{Identifier: "CLI-2", Status: "ready", RecoverAttempts: 2}, []string{"⟳ r2"}, []string{"×"}},
		{"both", Row{Identifier: "CLI-3", Status: "rework", ReworkCount: 2, RecoverAttempts: 1}, []string{"⟳ ×2 r1"}, nil},
		{"neither", Row{Identifier: "CLI-4", Status: "ready"}, nil, []string{"⟳"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.rowDetail(tt.row, section{}, 1000)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("rowDetail = %q, want it to contain %q", got, w)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(got, n) {
					t.Errorf("rowDetail = %q, want it NOT to contain %q", got, n)
				}
			}
		})
	}
}

// TestRowDetail_RetryCountdown asserts a non-live row inside its
// retry-backoff window shows when it becomes claimable again, and that the
// countdown disappears once the window passes (P4). blocked_until is set on
// the re-queued card (its release column), so this must not be gated on the
// BLOCKED section.
func TestRowDetail_RetryCountdown(t *testing.T) {
	m := NewModel()
	row := Row{Identifier: "CLI-1", Status: "ready", BlockedUntil: 1040}

	if got := m.rowDetail(row, section{}, 1000); !strings.Contains(got, "retry in 40s") {
		t.Errorf("rowDetail inside backoff window = %q, want %q", got, "retry in 40s")
	}
	if got := m.rowDetail(row, section{}, 2000); strings.Contains(got, "retry in") {
		t.Errorf("rowDetail after backoff window = %q, want no countdown", got)
	}
}

// TestRowDetail_UnmirroredBadge asserts a row with a pending Linear outbox
// write carries the ⇅ badge (P4) — the outbox backlog visible where the
// operator looks.
func TestRowDetail_UnmirroredBadge(t *testing.T) {
	m := NewModel()
	row := Row{Identifier: "CLI-1", Status: "review", Unmirrored: true}
	if got := m.rowDetail(row, section{}, 1000); !strings.Contains(got, "⇅ linear pending") {
		t.Errorf("rowDetail = %q, want it to contain %q", got, "⇅ linear pending")
	}
}

// TestRowDetail_StaleHeartbeat asserts a live row whose run's heartbeat has
// gone quiet past staleHeartbeatS shows the ♥ warning, and a fresh heartbeat
// doesn't (P4).
func TestRowDetail_StaleHeartbeat(t *testing.T) {
	m := NewModel()
	stale := Row{
		Identifier: "CLI-1", Status: "running", Live: true,
		Run: &store.Run{RunID: "r1", Lane: "coder", StartedAt: 900, HeartbeatAt: 900},
	}
	if got := m.rowDetail(stale, section{}, 1000); !strings.Contains(got, "♥") {
		t.Errorf("rowDetail with %ds-old heartbeat = %q, want a ♥ warning", 100, got)
	}
	fresh := Row{
		Identifier: "CLI-2", Status: "running", Live: true,
		Run: &store.Run{RunID: "r2", Lane: "coder", StartedAt: 900, HeartbeatAt: 990},
	}
	if got := m.rowDetail(fresh, section{}, 1000); strings.Contains(got, "♥") {
		t.Errorf("rowDetail with fresh heartbeat = %q, want no ♥ warning", got)
	}
}

// TestRenderRow_BoundedWidth asserts the composed row line never exceeds the
// panel's inner width, even when every detail chip is active at once — turn,
// ⟳ rework/recover, tokens, retry countdown, ⇅ linear pending — exactly the
// stack a Linear outage plus a transient burst produces on a narrow terminal.
// A row wider than the panel would also wrap, drifting orderedLineIndex's
// line geometry.
func TestRenderRow_BoundedWidth(t *testing.T) {
	m := NewModel()
	allChips := Row{
		Identifier: "CLI-1", LaneLabel: "coder", Status: "ready",
		Run:         &store.Run{RunID: "r1", Lane: "coder", TurnCount: 3},
		TokensIn:    123456,
		TokensOut:   654321,
		ReworkCount: 2, RecoverAttempts: 3,
		BlockedUntil: 1040,
		Unmirrored:   true,
	}
	staleLive := Row{
		Identifier: "CLI-2", LaneLabel: "coder", Status: "running", Live: true,
		Run:       &store.Run{RunID: "r2", Lane: "coder", StartedAt: 900, HeartbeatAt: 900},
		TokensIn:  1000,
		TokensOut: 2000, Unmirrored: true,
	}
	tests := []struct {
		name  string
		row   Row
		s     section
		inner int
	}{
		{"all chips, narrow", allChips, section{}, 40},
		{"all chips, medium", allChips, section{}, 60},
		{"live stale heartbeat, narrow", staleLive, section{dimIdle: true}, 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.renderRow(tt.row, tt.s, tt.inner, 1000)
			if w := lipgloss.Width(got); w > tt.inner {
				t.Errorf("renderRow width = %d, want <= %d (line %q)", w, tt.inner, got)
			}
		})
	}
}

// TestFold_QueuedSortsByPriority asserts QUEUED orders by the kernel's own
// claim order (P4, mirroring store.selectClaimCandidate): priority 1 (urgent)
// first, 0 ("none") last, ties by identifier.
func TestFold_QueuedSortsByPriority(t *testing.T) {
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			{Issue: store.Issue{ID: "a", Identifier: "CLI-1", BoardStatus: "todo", Priority: 0}},
			{Issue: store.Issue{ID: "b", Identifier: "CLI-2", BoardStatus: "todo", Priority: 4}},
			{Issue: store.Issue{ID: "c", Identifier: "CLI-3", BoardStatus: "ready", Priority: 1}},
			{Issue: store.Issue{ID: "d", Identifier: "CLI-4", BoardStatus: "todo", Priority: 1}},
		},
	}
	m := NewModel()
	m, _ = m.Update(SnapshotMsg{Snap: snap})

	want := []string{"CLI-3", "CLI-4", "CLI-2", "CLI-1"}
	queued := m.Queued()
	if len(queued) != len(want) {
		t.Fatalf("Queued() len = %d, want %d", len(queued), len(want))
	}
	for i, w := range want {
		if queued[i].Identifier != w {
			t.Errorf("Queued()[%d] = %q, want %q (priority order: 1,1,4,none; ties by identifier)", i, queued[i].Identifier, w)
		}
	}
}

// TestHeader_UnmirroredChip asserts the header grows an amber unmirrored
// count when the outbox has pending Linear writes, and stays silent at zero.
func TestHeader_UnmirroredChip(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: store.Snapshot{UnmirroredCount: 3}})
	if view := m.View(); !strings.Contains(view, "3 unmirrored") {
		t.Errorf("View() missing %q chip", "3 unmirrored")
	}

	m, _ = m.Update(SnapshotMsg{Snap: store.Snapshot{}})
	if view := m.View(); strings.Contains(view, "unmirrored") {
		t.Errorf("View() shows an unmirrored chip at zero pending writes")
	}
}
