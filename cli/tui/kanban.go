package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// kanbanColumns is the left-to-right board order rendered by the kanban view.
// "blocked" is included (beyond the core pipeline columns) so parked issues
// are never invisible on the board.
var kanbanColumns = []string{"todo", "ready", "running", "review", "rework", "merging", "blocked", "done"}

// kanbanLabel is the short column heading.
func kanbanLabel(status string) string {
	return status
}

// minKanbanColWidth is the narrowest a column may render before the view
// starts dropping columns to stay responsive.
const minKanbanColWidth = 16

// renderKanbanScreen draws the board as side-by-side columns via
// lipgloss.JoinHorizontal. Responsiveness (a hard requirement) is handled by
// progressively shedding columns: first the empty ones, then — if the terminal
// is still too narrow — the rightmost remaining columns, with a "+N cols"
// marker so nothing is silently hidden. The selected card (tracked by
// identifier across the whole board) is highlighted, and enter still opens its
// detail.
func (m Model) renderKanbanScreen(inner int, now int64) string {
	d := m.dims()
	width := d.frameW

	visible, hiddenCols := m.visibleKanbanColumns(width)
	n := maxInt(len(visible), 1)
	colW := clampInt(width/n-2, minKanbanColWidth-2, 28)

	// All columns share one height so the board is a clean equal-height grid
	// that fills the body between the tab bar and the pinned footer.
	boardH := d.bodyH
	if hiddenCols > 0 {
		boardH-- // reserve a line for the "+N more columns" note
	}

	boxes := make([]string, 0, len(visible))
	for _, col := range visible {
		boxes = append(boxes, m.renderKanbanColumn(col, colW, boardH))
	}
	board := lipgloss.JoinHorizontal(lipgloss.Top, boxes...)

	parts := []string{m.renderHeader(d.cw, now), m.renderTabs(d.cw), board}
	if hiddenCols > 0 {
		parts = append(parts, dimStyle.Render(fmt.Sprintf("  +%d more column(s) — widen the terminal to see them", hiddenCols)))
	}
	parts = append(parts, m.renderFooter(d.cw))
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// visibleKanbanColumns decides which columns fit in width, shedding empty
// columns first and then the rightmost remaining ones. It returns the columns
// to render and how many were dropped for lack of space (0 when all fit).
func (m Model) visibleKanbanColumns(width int) (visible []string, hidden int) {
	if width/len(kanbanColumns) >= minKanbanColWidth {
		return kanbanColumns, 0
	}

	// Too narrow for all columns: keep only the non-empty ones.
	nonEmpty := make([]string, 0, len(kanbanColumns))
	for _, col := range kanbanColumns {
		if len(m.byStatus[col]) > 0 {
			nonEmpty = append(nonEmpty, col)
		}
	}
	if len(nonEmpty) == 0 {
		nonEmpty = kanbanColumns[:1] // always show at least one column
	}

	fit := maxInt(1, width/minKanbanColWidth)
	if fit >= len(nonEmpty) {
		return nonEmpty, 0
	}
	return nonEmpty[:fit], len(nonEmpty) - fit
}

// renderKanbanColumn draws one fixed-height bordered column: a heading (status
// + count) over a rule, then the compact cards (or a dim placeholder), with a
// "+N more" marker when they overflow the column height. colH is the column's
// full height including its border; the card capacity is derived from it (each
// card is two lines) so the content never overflows and skews the grid.
func (m Model) renderKanbanColumn(status string, colW, colH int) string {
	rows := m.byStatus[status]
	heading := lipgloss.NewStyle().Bold(true).Foreground(statusColor(status)).
		Render(fmt.Sprintf("%s (%d)", kanbanLabel(status), len(rows)))
	// The rule spans the padded text width (colW − 2 for the 1-col padding), so
	// it never overflows into a wrapped second line.
	lines := []string{heading, ruleStyle.Render(strings.Repeat("─", maxInt(colW-2, 1)))}

	// Content budget = colH − border(2) − heading(1) − rule(1); each card is 2
	// lines, and an overflow marker (if any) costs one more line.
	maxCards := maxInt((colH-4)/2, 0)
	shown := rows
	overflow := 0
	if len(rows) > maxCards {
		shown = rows[:maxInt((colH-5)/2, 0)]
		overflow = len(rows) - len(shown)
	}

	if len(rows) == 0 {
		lines = append(lines, dimStyle.Render("· none"))
	}
	for _, r := range shown {
		lines = append(lines, m.renderKanbanCard(r))
	}
	if overflow > 0 {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("+%d more", overflow)))
	}

	return panelBorderStyle.
		Width(colW).
		Height(maxInt(colH-2, 1)).
		Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

// renderKanbanCard renders one issue as a compact card: identifier over a lane
// badge, with the selected card marked and its identifier reversed. The
// enclosing column box (Width(colW)) bounds the card width, so no manual
// clipping is needed here.
func (m Model) renderKanbanCard(row Row) string {
	selected := row.Identifier == m.selected

	id := idStyle.Render(row.Identifier)
	prefix := "  "
	if selected {
		id = selIDStyle.Render(row.Identifier)
		prefix = selMarkStyle.Render("▌") + " "
	}
	return lipgloss.JoinVertical(lipgloss.Left, prefix+id, "  "+laneBadge(row.LaneLabel))
}
