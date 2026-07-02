package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// kanbanColumns is the left-to-right board order rendered by the kanban view.
// "blocked" is included (beyond the core pipeline columns) so parked issues
// are never invisible on the board.
var kanbanColumns = []string{"todo", "ready", "running", "review", "rework", "merging", "documentation", "blocked", "done"}

// kanbanLabel is the short column heading — "documentation" is abbreviated so
// the heading fits a narrow column without wrapping.
func kanbanLabel(status string) string {
	if status == "documentation" {
		return "docs"
	}
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
	width := m.width
	if width <= 0 {
		width = 96
	}

	visible, hiddenCols := m.visibleKanbanColumns(width)
	colW := clampInt(width/maxInt(1, len(visible))-1, minKanbanColWidth-1, 30)

	// Card capacity per column from the height left under the header/footer.
	height := m.height
	if height <= 0 {
		height = 40
	}
	headerH := lipgloss.Height(m.renderHeader(inner, 0))
	cardCap := clampInt(height-headerH-6, 3, 30)

	boxes := make([]string, 0, len(visible))
	for _, col := range visible {
		boxes = append(boxes, m.renderKanbanColumn(col, colW, cardCap))
	}
	board := lipgloss.JoinHorizontal(lipgloss.Top, boxes...)

	var b strings.Builder
	b.WriteString(m.renderHeader(inner, now))
	b.WriteString("\n")
	b.WriteString(board)
	if hiddenCols > 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("  +%d more column(s) — widen the terminal to see them", hiddenCols)))
	}
	b.WriteString("\n")
	b.WriteString(footerStyle.Render(m.help.View(m.keys)))
	return b.String()
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

// renderKanbanColumn draws one bordered column: a heading (status + count) and
// up to cardCap compact cards, with a "+N more" footer when it overflows.
func (m Model) renderKanbanColumn(status string, colW, cardCap int) string {
	rows := m.byStatus[status]
	heading := lipgloss.NewStyle().Bold(true).Foreground(statusColor(status)).
		Render(fmt.Sprintf("%s (%d)", kanbanLabel(status), len(rows)))

	lines := []string{heading}
	if len(rows) == 0 {
		lines = append(lines, dimStyle.Render("—"))
	}
	shown := rows
	overflow := 0
	if len(shown) > cardCap {
		overflow = len(shown) - cardCap
		shown = shown[:cardCap]
	}
	for _, r := range shown {
		lines = append(lines, m.renderKanbanCard(r))
	}
	if overflow > 0 {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("+%d more", overflow)))
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cBorder).
		Padding(0, 1).
		Width(colW).
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
