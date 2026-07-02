package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// sectionList returns the four dashboard groups in render/navigation order.
// The order here MUST match fold's construction of m.ordered and
// orderedLineIndex's geometry, since the selection cursor walks all three in
// lockstep.
func (m Model) sectionList() []section {
	return []section{
		{"RUNNING", cGreen, "▶", m.running, true, false},
		{"IN FLIGHT", cCyan, "◐", m.inFlight, false, false},
		{"BLOCKED", cRed, "✖", m.blocked, false, false},
		{"QUEUED", cAmber, "•", m.queued, false, true},
	}
}

// renderBody renders the scrollable dashboard body: the four section panels
// stacked, then a compact DONE summary. now feeds the RUNNING rows' live
// elapsed (View passes the wall clock; layout passes 0 for a stable line
// count, since elapsed is inline and never adds lines).
func (m Model) renderBody(inner int, now int64) string {
	var parts []string
	for _, s := range m.sectionList() {
		parts = append(parts, m.renderSection(s, inner, now))
	}
	body := strings.Join(parts, "\n")
	if done := m.renderDoneSummary(inner); done != "" {
		body += "\n" + done
	}
	return body
}

// renderSection draws one titled, bordered panel of rows (or a dim placeholder
// when empty), tinted with the section's accent color.
func (m Model) renderSection(s section, inner int, now int64) string {
	heading := lipgloss.NewStyle().Bold(true).Foreground(s.accent).
		Render(fmt.Sprintf("%s %s", s.glyph, s.title))
	count := dimStyle.Render(fmt.Sprintf(" (%d)", len(s.rows)))

	lines := []string{heading + count}
	if len(s.rows) == 0 {
		lines = append(lines, dimStyle.Render("  —"))
	} else {
		for _, row := range s.rows {
			lines = append(lines, m.renderRow(row, s, inner, now))
		}
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cBorder).
		Padding(0, 1).
		Width(inner).
		Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

// renderRow formats one issue line: a selection bar, a lead glyph/spinner, a
// lane badge, the identifier, a status chip, and a right-aligned detail
// (turn/tokens/elapsed, or a "waiting on …" dependency hint for QUEUED rows).
func (m Model) renderRow(row Row, s section, inner int, now int64) string {
	selected := row.Identifier == m.selected

	// A fixed 2-cell selection gutter keeps columns aligned whether or not a
	// row is selected.
	mark := "  "
	if selected {
		mark = selMarkStyle.Render("▌") + " "
	}

	lead := lipgloss.NewStyle().Foreground(s.accent).Render(s.glyph)
	if s.live && row.Run != nil {
		lead = lipgloss.NewStyle().Foreground(cGreen).Render(spinnerFrames[m.frame%len(spinnerFrames)])
	}

	idText := fmt.Sprintf("%-9s", row.Identifier)
	idCell := idStyle.Render(idText)
	if selected {
		idCell = selIDStyle.Render(idText)
	}

	// Fixed-width cells so lane / id / status line up as columns across rows.
	badgeCell := lipgloss.NewStyle().Width(14).Render(laneBadge(row.LaneLabel))
	statusCell := lipgloss.NewStyle().Width(15).Render(statusChip(row.Status))
	left := mark + lipgloss.JoinHorizontal(lipgloss.Center,
		lead, " ",
		badgeCell, " ",
		idCell, " ",
		statusCell,
	)

	detail := m.rowDetail(row, s, now)
	// Right-align the detail within the panel's text area. The panel sets
	// Width(inner) but its Padding(0,1) consumes 2 of that, so the usable
	// text width is inner-2; targeting inner here would overflow and wrap.
	avail := inner - 2
	gap := avail - lipgloss.Width(left) - lipgloss.Width(detail)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + detail
}

// rowDetail renders the trailing metadata. For a QUEUED row with unmet
// dependencies it shows a "waiting on …" hint instead; otherwise it shows
// turn/attempt, cumulative tokens, and (for a live row) elapsed runtime.
func (m Model) rowDetail(row Row, s section, now int64) string {
	if s.waiting {
		if unmet := unmetDeps(row.Deps, m.identByID, m.statusByID); len(unmet) > 0 {
			// Cap the listed deps so a heavily-blocked card's detail can't grow
			// wide enough to wrap the row (which would also throw off the body
			// line geometry orderedLineIndex measures).
			const maxShown = 3
			suffix := ""
			if len(unmet) > maxShown {
				suffix = fmt.Sprintf(" +%d", len(unmet)-maxShown)
				unmet = unmet[:maxShown]
			}
			return waitingStyle.Render("⏳ waiting on " + strings.Join(unmet, ", ") + suffix)
		}
	}

	var parts []string
	if row.Run != nil {
		parts = append(parts, dimStyle.Render(fmt.Sprintf("turn %d", row.Run.TurnCount)))
	}
	if row.TokensIn > 0 || row.TokensOut > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cCyan).Render("↓"+humanizeTokens(row.TokensIn))+
			dimStyle.Render(" ")+
			lipgloss.NewStyle().Foreground(cPurple).Render("↑"+humanizeTokens(row.TokensOut)))
	}
	if s.live && row.Run != nil {
		parts = append(parts, lipgloss.NewStyle().Foreground(cGreen).Render("⏱ "+formatElapsed(row.Run, now)))
	}
	if len(parts) == 0 {
		return dimStyle.Render("—")
	}
	return strings.Join(parts, dimStyle.Render(" · "))
}

// renderDoneSummary renders a single compact line listing the identifiers of
// completed issues (dim), so terminal "done" cards — which the stacked
// sections omit — remain visible. Returns "" when nothing is done.
func (m Model) renderDoneSummary(inner int) string {
	done := m.byStatus["done"]
	if len(done) == 0 {
		return ""
	}
	idents := make([]string, 0, len(done))
	for _, r := range done {
		idents = append(idents, r.Identifier)
	}
	head := doneHeadStyle.Render(fmt.Sprintf("✓ DONE (%d)  ", len(done)))
	// Budget the identifier list to the remaining width so the line never
	// wraps (which would throw off the body's line geometry).
	budget := inner - lipgloss.Width(head) - 2
	list := truncatePlain(strings.Join(idents, "  "), budget)
	return head + dimStyle.Render(list)
}

// orderedLineIndex returns the 0-based line, within renderBody's output, of the
// ordered row at global index g. It measures actual rendered heights rather
// than assuming one line per row, so a row that wraps at a narrow width can't
// drift the result: preceding panels are summed via lipgloss.Height of the
// whole panel, and rows preceding g within its panel via lipgloss.Height of
// each rendered row (the heading is a single short line). Used to keep the
// selected row visible when scrolling.
func (m Model) orderedLineIndex(g int) int {
	width := m.width
	if width <= 0 {
		width = 96
	}
	inner := width - 4
	if inner < 24 {
		inner = 24
	}

	line := 0
	seen := 0
	for _, s := range m.sectionList() {
		if g < seen+len(s.rows) {
			line += 2 // top border + heading
			for i := 0; i < g-seen; i++ {
				line += lipgloss.Height(m.renderRow(s.rows[i], s, inner, 0))
			}
			return line
		}
		seen += len(s.rows)
		line += lipgloss.Height(m.renderSection(s, inner, 0))
	}
	return line
}
