package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/store"
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))

	sectionStyles = map[string]lipgloss.Style{
		"RUNNING":   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")), // green
		"IN FLIGHT": lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14")), // cyan
		"BLOCKED":   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9")),  // red
		"QUEUED":    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11")), // yellow
	}

	rowStyle   = lipgloss.NewStyle().PaddingLeft(2)
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	footerHint = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
)

// View renders the dashboard: a header line with token totals, four
// sections (RUNNING / IN FLIGHT / BLOCKED / QUEUED), and a quit hint.
// Rendering is pure formatting over the Model's already-folded state — no
// store access.
func (m Model) View() string {
	var b strings.Builder

	b.WriteString(headerStyle.Render(fmt.Sprintf(
		"clipse tui — tokens in: %d  out: %d", m.tokensIn, m.tokensOut,
	)))
	b.WriteString("\n\n")

	if m.lastErr != nil {
		b.WriteString(errorStyle.Render("refresh error: " + m.lastErr.Error()))
		b.WriteString("\n\n")
	}

	b.WriteString(renderSection("RUNNING", m.running, true))
	b.WriteString("\n")
	b.WriteString(renderSection("IN FLIGHT", m.inFlight, false))
	b.WriteString("\n")
	b.WriteString(renderSection("BLOCKED", m.blocked, false))
	b.WriteString("\n")
	b.WriteString(renderSection("QUEUED", m.queued, false))
	b.WriteString("\n")

	b.WriteString(footerHint.Render("press q or ctrl+c to quit"))

	return b.String()
}

// renderSection renders one titled group of rows. showRuntime controls
// whether the row includes an elapsed-runtime counter (only meaningful for
// RUNNING, where StartedAt marks an in-flight run).
func renderSection(title string, rows []Row, showRuntime bool) string {
	style, ok := sectionStyles[title]
	if !ok {
		style = headerStyle
	}

	var b strings.Builder
	b.WriteString(style.Render(fmt.Sprintf("%s (%d)", title, len(rows))))
	b.WriteString("\n")

	if len(rows) == 0 {
		b.WriteString(rowStyle.Render(dimStyle.Render("(none)")))
		b.WriteString("\n")
		return b.String()
	}

	for _, row := range rows {
		b.WriteString(rowStyle.Render(renderRow(row, showRuntime)))
		b.WriteString("\n")
	}
	return b.String()
}

// renderRow formats a single issue line: identifier, lane, board column,
// and latest-run info (or a "-" placeholder if the issue has never run).
// The column is included for every section (not just IN FLIGHT) for a
// consistent row shape, but it earns its place there specifically: IN
// FLIGHT is the one section whose rows span more than one board_status
// value (review/rework/merging/documentation), so it is the only section
// where this field actually disambiguates anything.
func renderRow(row Row, showRuntime bool) string {
	runInfo := "-"
	if row.Run != nil {
		runInfo = fmt.Sprintf("%s turn %d attempt %d (in:%d out:%d)",
			row.Run.Status, row.Run.TurnCount, row.Run.Attempt, row.Run.TokensIn, row.Run.TokensOut)
		if showRuntime {
			runInfo += fmt.Sprintf(" running %s", formatElapsed(row.Run))
		}
	}
	return fmt.Sprintf("%-10s  %-16s  %-14s  %s", row.Identifier, row.LaneLabel, row.Status, runInfo)
}

// formatElapsed renders how long run has been running, based on
// StartedAt (a unix timestamp) vs. wall-clock now.
func formatElapsed(run *store.Run) string {
	if run == nil || run.StartedAt == 0 {
		return "-"
	}
	elapsed := time.Since(time.Unix(run.StartedAt, 0)).Round(time.Second)
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed.String()
}
