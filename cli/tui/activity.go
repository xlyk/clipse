package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// renderActivityPanel draws the ACTIVITY feed panel: a heading over the
// activity viewport (whose content layout() has already set). The viewport
// lets the feed scroll when it holds more events than fit.
func (m Model) renderActivityPanel(inner int) string {
	body := lipgloss.JoinVertical(lipgloss.Left,
		panelHeadStyle.Render("⚡ ACTIVITY"),
		m.activityVp.View(),
	)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cBorder).
		Padding(0, 1).
		Width(inner).
		Render(body)
}

// activityLines formats the recent-events feed (newest-first) into one line
// each: "HH:MM:SS  <identifier>  <glyph> <kind> <detail>", the glyph and color
// keyed off the event kind and the detail truncated to fit width. Formatting a
// fixed event ts (time.Unix) is deterministic — it is not a wall-clock read —
// so this is safe to call from the pure layout path.
func (m Model) activityLines(width int) []string {
	if len(m.recentEvents) == 0 {
		return []string{dimStyle.Render("no activity yet")}
	}

	lines := make([]string, 0, len(m.recentEvents))
	for _, e := range m.recentEvents {
		ts := time.Unix(e.Ts, 0).Format("15:04:05")

		ident := "—"
		if e.IssueID.Valid && e.IssueID.String != "" {
			if id := m.identByID[e.IssueID.String]; id != "" {
				ident = id
			} else {
				ident = shortID(e.IssueID.String)
			}
		}

		glyph, glyphColor := eventGlyph(e.Kind)
		kind := truncatePlain(e.Kind, 14)

		// Budget the free-text detail to whatever width is left after the
		// fixed-width lead columns, so the styled line never exceeds width.
		used := 8 + 2 + 8 + 2 + 1 + 1 + len(kind) + 1 // ts, gaps, ident, glyph, kind
		detail := truncatePlain(oneLine(e.Detail), width-used)

		line := dimStyle.Render(ts) + "  " +
			lipgloss.NewStyle().Foreground(cText).Render(fmt.Sprintf("%-8s", truncatePlain(ident, 8))) + "  " +
			lipgloss.NewStyle().Foreground(glyphColor).Render(glyph) + " " +
			lipgloss.NewStyle().Foreground(glyphColor).Render(kind) + " " +
			dimStyle.Render(detail)
		lines = append(lines, line)
	}
	return lines
}

// eventGlyph maps an event kind to a leading glyph and color: merges/dones are
// green ✓, blocks are red ✖, claims are cyan ▶, everything else a dim ·.
func eventGlyph(kind string) (string, lipgloss.Color) {
	switch {
	case strings.Contains(kind, "merge") || kind == "done" || kind == "complete" || kind == "promoted":
		return "✓", cGreen
	case strings.Contains(kind, "block"):
		return "✖", cRed
	case strings.Contains(kind, "claim"):
		return "▶", cCyan
	case strings.Contains(kind, "stale") || strings.Contains(kind, "release"):
		return "⟳", cAmber
	default:
		return "·", cDim
	}
}

// oneLine collapses a possibly multi-line event detail into a single trimmed
// line (event details can carry embedded worker output with newlines).
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}
