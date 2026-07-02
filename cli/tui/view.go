package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/store"
)

// Palette — a GitHub-dark-ish scheme so the dashboard looks at home in a
// modern terminal. Truecolor hex degrades gracefully on 256-color terminals.
const (
	cText   = lipgloss.Color("#c9d1d9")
	cDim    = lipgloss.Color("#6e7681")
	cBorder = lipgloss.Color("#30363d")
	cInk    = lipgloss.Color("#0d1117") // near-black, for text on a bright badge

	cGreen  = lipgloss.Color("#3fb950")
	cCyan   = lipgloss.Color("#58a6ff")
	cRed    = lipgloss.Color("#f85149")
	cAmber  = lipgloss.Color("#d29922")
	cPurple = lipgloss.Color("#bc8cff")
	cTeal   = lipgloss.Color("#39c5cf")
	cOrange = lipgloss.Color("#db6d28")
)

// spinnerFrames animates running rows; braille cells give a smooth spin.
var spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(cInk).Background(cCyan).Padding(0, 1)
	subtitleStyle = lipgloss.NewStyle().Foreground(cDim).Italic(true)
	tokenNumStyle = lipgloss.NewStyle().Bold(true).Foreground(cText)
	dimStyle      = lipgloss.NewStyle().Foreground(cDim)
	idStyle       = lipgloss.NewStyle().Bold(true).Foreground(cText)
	errorStyle    = lipgloss.NewStyle().Bold(true).Foreground(cRed)
	footerStyle   = lipgloss.NewStyle().Foreground(cDim)
	keyStyle      = lipgloss.NewStyle().Bold(true).Foreground(cText)
)

// section describes one dashboard group: its title, the accent color that
// tints its border + heading, the glyph that leads each of its rows, and
// whether its rows show a live elapsed/spinner (only RUNNING does).
type section struct {
	title  string
	accent lipgloss.Color
	glyph  string
	rows   []Row
	live   bool
}

// View renders the whole dashboard: a header panel (title, status chips, token
// counters) then one bordered panel per section, then a footer. Pure
// formatting over the Model's folded state — no store access.
func (m Model) View() string {
	width := m.width
	if width <= 0 {
		width = 96
	}
	// Inner content width for a panel: terminal width minus the box's border
	// (2) and horizontal padding (2).
	inner := width - 4
	if inner < 24 {
		inner = 24
	}

	var b strings.Builder
	b.WriteString(m.renderHeader(inner))
	b.WriteString("\n\n")

	if m.lastErr != nil {
		b.WriteString(errorStyle.Render("⚠ refresh error: " + m.lastErr.Error()))
		b.WriteString("\n\n")
	}

	sections := []section{
		{"RUNNING", cGreen, "▶", m.running, true},
		{"IN FLIGHT", cCyan, "◐", m.inFlight, false},
		{"BLOCKED", cRed, "✖", m.blocked, false},
		{"QUEUED", cAmber, "•", m.queued, false},
	}
	for i, s := range sections {
		b.WriteString(m.renderSection(s, inner))
		if i < len(sections)-1 {
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(m.renderFooter())
	return b.String()
}

// renderHeader draws the top panel: the "clipse" title, a live pulse, a row of
// status-count chips, and the cumulative token counters.
func (m Model) renderHeader(inner int) string {
	pulse := lipgloss.NewStyle().Foreground(cGreen)
	if m.frame%2 == 0 {
		pulse = pulse.Foreground(cDim)
	}
	titleLine := lipgloss.JoinHorizontal(lipgloss.Center,
		titleStyle.Render("clipse"),
		"  ",
		subtitleStyle.Render("pipeline dashboard"),
		"  ",
		pulse.Render("●"),
	)

	chips := lipgloss.JoinHorizontal(lipgloss.Center,
		countChip("▶", "running", m.count("running"), cGreen),
		"  ",
		countChip("◐", "in flight", m.inFlightCount(), cCyan),
		"  ",
		countChip("•", "queued", m.count("ready")+m.count("todo"), cAmber),
		"  ",
		countChip("✖", "blocked", m.count("blocked"), cRed),
		"  ",
		countChip("✓", "done", m.count("done"), cPurple),
	)

	tokens := lipgloss.JoinHorizontal(lipgloss.Center,
		dimStyle.Render("tokens  "),
		lipgloss.NewStyle().Foreground(cCyan).Render("↓ "),
		tokenNumStyle.Render(humanizeTokens(m.tokensIn)),
		dimStyle.Render(" in    "),
		lipgloss.NewStyle().Foreground(cPurple).Render("↑ "),
		tokenNumStyle.Render(humanizeTokens(m.tokensOut)),
		dimStyle.Render(" out"),
	)

	body := lipgloss.JoinVertical(lipgloss.Left, titleLine, "", chips, "", tokens)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cCyan).
		Padding(0, 1).
		Width(inner).
		Render(body)
}

// renderSection draws one titled, bordered panel of rows (or a dim placeholder
// when empty), tinted with the section's accent color.
func (m Model) renderSection(s section, inner int) string {
	heading := lipgloss.NewStyle().Bold(true).Foreground(s.accent).
		Render(fmt.Sprintf("%s %s", s.glyph, s.title))
	count := dimStyle.Render(fmt.Sprintf(" (%d)", len(s.rows)))

	var lines []string
	lines = append(lines, heading+count)
	if len(s.rows) == 0 {
		lines = append(lines, dimStyle.Render("  —"))
	} else {
		for _, row := range s.rows {
			lines = append(lines, m.renderRow(row, s, inner))
		}
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cBorder).
		Padding(0, 1).
		Width(inner).
		Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

// renderRow formats one issue line: a lead glyph/spinner, a lane badge, the
// identifier, a status chip, and a right-aligned detail (turn/attempt, tokens,
// and — for a running row — elapsed time).
func (m Model) renderRow(row Row, s section, inner int) string {
	lead := lipgloss.NewStyle().Foreground(s.accent).Render(s.glyph)
	if s.live && row.Run != nil {
		lead = lipgloss.NewStyle().Foreground(cGreen).Render(spinnerFrames[m.frame%len(spinnerFrames)])
	}

	// Fixed-width cells so lane / id / status line up as columns across rows,
	// regardless of lane-name or status-name length.
	badgeCell := lipgloss.NewStyle().Width(14).Render(laneBadge(row.LaneLabel))
	statusCell := lipgloss.NewStyle().Width(15).Render(statusChip(row.Status))
	left := lipgloss.JoinHorizontal(lipgloss.Center,
		lead, " ",
		badgeCell, " ",
		idStyle.Render(fmt.Sprintf("%-9s", row.Identifier)), " ",
		statusCell,
	)

	detail := m.rowDetail(row, s.live)
	// Right-align the detail within the panel's text area. The panel sets
	// Width(inner) but its Padding(0,1) consumes 2 of that, so the usable text
	// width is inner-2; targeting inner here would overflow and wrap.
	avail := inner - 2
	gap := avail - lipgloss.Width(left) - lipgloss.Width(detail)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + detail
}

// rowDetail renders the trailing metadata: turn/attempt, cumulative tokens,
// and elapsed runtime for a live row.
func (m Model) rowDetail(row Row, live bool) string {
	var parts []string
	if row.Run != nil {
		parts = append(parts, dimStyle.Render(fmt.Sprintf("turn %d", row.Run.TurnCount)))
	}
	if row.TokensIn > 0 || row.TokensOut > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cCyan).Render("↓"+humanizeTokens(row.TokensIn))+
			dimStyle.Render(" ")+
			lipgloss.NewStyle().Foreground(cPurple).Render("↑"+humanizeTokens(row.TokensOut)))
	}
	if live && row.Run != nil {
		parts = append(parts, lipgloss.NewStyle().Foreground(cGreen).Render("⏱ "+formatElapsed(row.Run)))
	}
	if len(parts) == 0 {
		return dimStyle.Render("—")
	}
	return strings.Join(parts, dimStyle.Render(" · "))
}

// renderFooter draws the key hints.
func (m Model) renderFooter() string {
	return footerStyle.Render(
		keyStyle.Render("q") + " quit  " +
			dimStyle.Render("·") + "  refreshes every 2s",
	)
}

// count returns the board-wide number of issues in a given board_status.
func (m Model) count(status string) int { return m.counts[status] }

// inFlightCount sums the downstream active columns for the header chip.
func (m Model) inFlightCount() int {
	return m.count("review") + m.count("rework") + m.count("merging") + m.count("documentation")
}

// countChip renders a "glyph N label" stat chip in the given accent color.
func countChip(glyph, label string, n int, accent lipgloss.Color) string {
	return lipgloss.NewStyle().Foreground(accent).Bold(true).Render(fmt.Sprintf("%s %d", glyph, n)) +
		dimStyle.Render(" "+label)
}

// laneColor maps a bare lane to its badge color.
func laneColor(lane string) lipgloss.Color {
	switch lane {
	case "coder":
		return cCyan
	case "reviewer":
		return cPurple
	case "git_operator":
		return cOrange
	case "scribe":
		return cTeal
	default:
		return cDim
	}
}

// laneBadge renders a lane as a filled color chip (or a dim placeholder for an
// issue with no lane yet).
func laneBadge(lane string) string {
	if lane == "" {
		return dimStyle.Render("· · ·")
	}
	return lipgloss.NewStyle().
		Foreground(cInk).Background(laneColor(lane)).Bold(true).Padding(0, 1).
		Render(lane)
}

// statusColor maps a board column to its text color.
func statusColor(status string) lipgloss.Color {
	switch status {
	case "running":
		return cGreen
	case "review":
		return cCyan
	case "rework":
		return cAmber
	case "merging":
		return cPurple
	case "documentation":
		return cTeal
	case "ready":
		return cCyan
	case "blocked":
		return cRed
	case "done":
		return cGreen
	default: // todo, cancelled, ...
		return cDim
	}
}

// statusChip renders a board column as a subtle colored chip.
func statusChip(status string) string {
	return lipgloss.NewStyle().Foreground(statusColor(status)).Render(status)
}

// humanizeTokens renders a token count compactly: 1234 -> "1.2k",
// 1_500_000 -> "1.5M". Small counts render verbatim.
func humanizeTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// formatElapsed renders how long run has been running (StartedAt unix vs. now)
// as m:ss, e.g. "1:04". Returns "—" when there's no start time.
func formatElapsed(run *store.Run) string {
	if run == nil || run.StartedAt == 0 {
		return "—"
	}
	d := time.Since(time.Unix(run.StartedAt, 0)).Round(time.Second)
	if d < 0 {
		d = 0
	}
	m := int(d.Minutes())
	sec := int(d.Seconds()) % 60
	return fmt.Sprintf("%d:%02d", m, sec)
}
