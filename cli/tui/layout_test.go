package tui_test

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/cli/tui"
)

// TestView_FillsFrameHeight asserts every screen renders to exactly the
// terminal height: the layout is built so header + body + footer sum to the
// frame, which is what pins the footer to the bottom with no dead space (and,
// under the alt-screen renderer, what keeps a shorter frame from leaving stale
// rows behind). A regression here is the whole "fills the screen" property.
func TestView_FillsFrameHeight(t *testing.T) {
	sizes := [][2]int{{120, 40}, {100, 50}, {160, 45}, {90, 30}}
	// keystrokes reach each mode: dashboard (none), kanban (tab), detail
	// (enter), help (?).
	modes := []struct {
		name string
		keys []tea.KeyMsg
	}{
		{"dashboard", nil},
		{"kanban", []tea.KeyMsg{{Type: tea.KeyTab}}},
		{"detail", []tea.KeyMsg{{Type: tea.KeyEnter}}},
		{"help", []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("?")}}},
	}

	for _, sz := range sizes {
		for _, md := range modes {
			m := tui.NewModel()
			m, _ = m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
			m, _ = m.Update(tui.SnapshotMsg{Snap: buildSnapshot(), Live: true})
			for _, k := range md.keys {
				m, _ = m.Update(k)
			}
			if got := lipgloss.Height(m.View()); got != sz[1] {
				t.Errorf("%s @ %dx%d: View height = %d, want %d", md.name, sz[0], sz[1], got, sz[1])
			}
		}
	}
}

// TestMouseWheel_ScrollsPipeline asserts a wheel event over the pipeline pane
// scrolls it (and wheel-up returns it to the top), exercising the tea.MouseMsg
// path Update forwards to the viewport under the pointer. The 14-row terminal
// forces the pipeline content to overflow its tiny viewport so scrolling is
// observable.
func TestMouseWheel_ScrollsPipeline(t *testing.T) {
	m := tui.NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 14})
	m, _ = m.Update(tui.SnapshotMsg{Snap: buildSnapshot(), Live: true})

	before := m.View()
	down := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown, X: 4, Y: 9}
	for i := 0; i < 4; i++ {
		m, _ = m.Update(down)
	}
	if m.View() == before {
		t.Fatal("wheel-down over the pipeline did not scroll the pane")
	}

	up := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 4, Y: 9}
	for i := 0; i < 8; i++ {
		m, _ = m.Update(up)
	}
	if m.View() != before {
		t.Error("wheel-up did not return the pipeline to its original scroll position")
	}
}
