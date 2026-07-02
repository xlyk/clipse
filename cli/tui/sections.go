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

// renderSection renders one titled group of rows (or a dim placeholder when
// empty), tinted with the section's accent color. It is borderless — the
// enclosing PIPELINE panel supplies the single frame — so the four groups read
// as one board rather than four separate boxes.
func (m Model) renderSection(s section, inner int, now int64) string {
	head := lipgloss.NewStyle().Bold(true).Foreground(s.accent).Render(s.glyph+" "+s.title) +
		dimStyle.Render(fmt.Sprintf(" (%d)", len(s.rows)))
	// Extend the heading into a full-width divider so each group reads as a
	// labeled band, giving the board structure even when it is sparse.
	if fill := inner - lipgloss.Width(head) - 1; fill > 0 {
		head += " " + ruleStyle.Render(strings.Repeat("─", fill))
	}

	lines := []string{head}
	if len(s.rows) == 0 {
		lines = append(lines, dimStyle.Render("   · none"))
	} else {
		for _, row := range s.rows {
			lines = append(lines, m.renderRow(row, s, inner, now))
		}
	}
	lines = append(lines, "") // trailing spacer for breathing room between groups
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
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

	// Fixed-width, left-aligned cells so lane / id / status / meta read as an
	// aligned table row, rather than flinging the metadata to the far edge of a
	// wide panel where it reads as disconnected from its row.
	badgeCell := lipgloss.NewStyle().Width(15).Render(laneBadge(row.LaneLabel))
	statusCell := lipgloss.NewStyle().Width(13).Render(statusChip(row.Status))

	return mark + lipgloss.JoinHorizontal(lipgloss.Center,
		lead, " ",
		badgeCell, " ",
		idCell, " ",
		statusCell, "  ",
		m.rowDetail(row, s, now),
	)
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
	// Match the section groups: a full-width labeled band, then the completed
	// identifiers on the line below (budgeted so the line never wraps).
	head := doneHeadStyle.Render("✓ DONE") + dimStyle.Render(fmt.Sprintf(" (%d)", len(done)))
	if fill := inner - lipgloss.Width(head) - 1; fill > 0 {
		head += " " + ruleStyle.Render(strings.Repeat("─", fill))
	}
	list := dimStyle.Render("   " + truncatePlain(strings.Join(idents, "  "), maxInt(inner-4, 4)))
	return head + "\n" + list
}

// orderedLineIndex returns the 0-based line, within renderBody's output, of the
// ordered row at global index g. It measures actual rendered heights rather
// than assuming one line per row, so a row that wraps at a narrow width can't
// drift the result: preceding groups are summed via lipgloss.Height of the
// whole (borderless) group, and rows preceding g within its group via
// lipgloss.Height of each rendered row (the heading is a single line). Used to
// keep the selected row visible when scrolling the pipeline viewport.
func (m Model) orderedLineIndex(g int) int {
	inner := m.dims().pipeTextW

	line := 0
	seen := 0
	for _, s := range m.sectionList() {
		if g < seen+len(s.rows) {
			line++ // heading
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
