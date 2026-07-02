package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// activityLines formats the recent-events feed (newest-first) into one aligned
// line each: "HH:MM:SS  <id>  <glyph> <kind>  <detail>". The kind is mapped to
// a short human label (so "rework_cap_exceeded" reads as "rework cap" instead
// of truncating mid-word) and the free-text detail is cleaned and truncated to
// the width left after the fixed lead columns. A pending refresh error is
// surfaced as the first line. Formatting a fixed event ts (time.Unix) is
// deterministic — not a wall-clock read — so this is safe from layout().
func (m Model) activityLines(width int) []string {
	var lines []string

	if m.lastErr != nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(cRed).Render("⚠ ")+
			dimStyle.Render(truncatePlain("refresh error: "+oneLine(m.lastErr.Error()), maxInt(width-3, 6))))
	}

	if len(m.recentEvents) == 0 {
		if len(lines) == 0 {
			lines = append(lines, dimStyle.Render("no activity yet"))
		}
		return lines
	}

	// Fixed lead columns: ts(8) + gap(2) + id(7) + gap(2) + glyph(1) + gap(1) +
	// label(11) + gap(2). The detail fills whatever remains.
	const lead = 8 + 2 + 7 + 2 + 1 + 1 + 11 + 2

	for _, e := range m.recentEvents {
		ts := dimStyle.Render(time.Unix(e.Ts, 0).Format("15:04:05"))

		ident := "—"
		if e.IssueID.Valid && e.IssueID.String != "" {
			if id := m.identByID[e.IssueID.String]; id != "" {
				ident = id
			} else {
				ident = shortID(e.IssueID.String)
			}
		}
		idCell := lipgloss.NewStyle().Foreground(cText).Render(fmt.Sprintf("%-7s", truncatePlain(ident, 7)))

		glyph, color := eventGlyph(e.Kind)
		kindCell := lipgloss.NewStyle().Foreground(color).Bold(true).
			Render(glyph + " " + fmt.Sprintf("%-11s", kindLabel(e.Kind)))

		detail := truncatePlain(cleanActivityDetail(e.Kind, e.Detail), maxInt(width-lead, 6))

		lines = append(lines, ts+"  "+idCell+"  "+kindCell+"  "+dimStyle.Render(detail))
	}
	return lines
}

// kindLabel maps a raw event kind to a short, human label that fits the feed's
// fixed kind column without truncating mid-word.
func kindLabel(kind string) string {
	switch {
	case strings.Contains(kind, "merge"):
		return "merged"
	case kind == "done" || kind == "complete":
		return "complete"
	case kind == "promoted":
		return "promoted"
	case strings.Contains(kind, "cap"):
		return "rework cap"
	case strings.Contains(kind, "request") || strings.Contains(kind, "changes"):
		return "changes req"
	case strings.Contains(kind, "review"):
		return "review"
	case strings.Contains(kind, "claim"):
		return "claimed"
	case strings.Contains(kind, "block"):
		return "blocked"
	case strings.Contains(kind, "stale"):
		return "stale"
	case strings.Contains(kind, "release"):
		return "released"
	default:
		return truncatePlain(strings.ReplaceAll(kind, "_", " "), 11)
	}
}

// cleanActivityDetail collapses an event detail to one tidy line, stripping the
// noise that dominated the raw feed. A "claimed" event's detail is just
// "claimed by run <uuid>" — redundant with the kind — so it is reduced to a
// short run id; everything else is flattened to a single line.
func cleanActivityDetail(kind, detail string) string {
	d := oneLine(detail)
	if strings.Contains(kind, "claim") {
		d = strings.TrimPrefix(d, "claimed by run ")
		return "run " + shortID(d)
	}
	return d
}

// eventGlyph maps an event kind to a leading glyph and color: merges/dones are
// green ✓, blocks are red ✖, claims are cyan ▶, reviews cyan ◆, rework/changes
// amber ⟳, and everything else a dim ·.
func eventGlyph(kind string) (string, lipgloss.Color) {
	switch {
	case strings.Contains(kind, "merge") || kind == "done" || kind == "complete" || kind == "promoted":
		return "✓", cGreen
	case strings.Contains(kind, "block"):
		return "✖", cRed
	case strings.Contains(kind, "claim"):
		return "▶", cCyan
	case strings.Contains(kind, "request") || strings.Contains(kind, "changes") || strings.Contains(kind, "cap"):
		return "⟳", cAmber
	case strings.Contains(kind, "review"):
		return "◆", cCyan
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
