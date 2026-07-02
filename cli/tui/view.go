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

// spinnerFrames animates running rows; braille cells give a smooth spin. The
// frame-based spinner (advanced by spinnerTickMsg) is kept over the bubbles
// spinner: it is already pure/deterministic under a fast tick, and swapping in
// bubbles/spinner would add its own id-tagged tick plumbing for no visible
// gain. The bubbles library is used where it earns its keep instead —
// viewport, progress, help, and key.
var spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(cInk).Background(cCyan).Padding(0, 1)
	subtitleStyle = lipgloss.NewStyle().Foreground(cDim).Italic(true)
	tokenNumStyle = lipgloss.NewStyle().Bold(true).Foreground(cText)
	dimStyle      = lipgloss.NewStyle().Foreground(cDim)
	idStyle       = lipgloss.NewStyle().Bold(true).Foreground(cText)
	errorStyle    = lipgloss.NewStyle().Bold(true).Foreground(cRed)
	footerStyle   = lipgloss.NewStyle().Foreground(cDim)

	liveStyle    = lipgloss.NewStyle().Bold(true).Foreground(cGreen)
	idleStyle    = lipgloss.NewStyle().Foreground(cDim)
	costStyle    = lipgloss.NewStyle().Foreground(cGreen)
	waitingStyle = lipgloss.NewStyle().Foreground(cAmber)

	// selection highlight: a left bar plus a reversed identifier chip. A left
	// bar (rather than a full-line background) keeps the row's colored badges
	// legible and avoids ANSI reset seams across a styled line.
	selMarkStyle = lipgloss.NewStyle().Foreground(cAmber).Bold(true)
	selIDStyle   = lipgloss.NewStyle().Bold(true).Foreground(cInk).Background(cAmber)

	panelHeadStyle = lipgloss.NewStyle().Bold(true).Foreground(cText)
	labelStyle     = lipgloss.NewStyle().Foreground(cDim)
	doneHeadStyle  = lipgloss.NewStyle().Bold(true).Foreground(cGreen)
)

// section describes one dashboard group: its title, the accent color that
// tints its border + heading, the glyph that leads each of its rows, whether
// its rows show a live elapsed/spinner (only RUNNING does), and whether it is
// the QUEUED group (whose rows get a dependency "waiting on …" hint).
type section struct {
	title   string
	accent  lipgloss.Color
	glyph   string
	rows    []Row
	live    bool
	waiting bool
}

// View renders the active screen. It is the one place wall-clock time enters
// the TUI: the pure Update never calls time.Now, so all "elapsed"/"ago"
// readouts are computed here against a single now captured per frame.
func (m Model) View() string {
	now := time.Now().Unix()
	width := m.width
	if width <= 0 {
		width = 96
	}
	inner := width - 4
	if inner < 24 {
		inner = 24
	}

	if m.showHelp {
		return m.renderHelpScreen(inner, now)
	}
	switch m.mode {
	case modeDetail:
		return m.renderDetailScreen(inner, now)
	case modeKanban:
		return m.renderKanbanScreen(inner, now)
	default:
		return m.renderDashboard(inner, now)
	}
}

// renderDashboard is the default stacked view: a fixed header, the scrollable
// body of section panels (in a viewport), the activity feed, and a footer.
func (m Model) renderDashboard(inner int, now int64) string {
	var b strings.Builder
	b.WriteString(m.renderHeader(inner, now))
	b.WriteString("\n")

	if m.lastErr != nil {
		b.WriteString(errorStyle.Render("⚠ refresh error: " + m.lastErr.Error()))
		b.WriteString("\n")
	}

	// The body viewport's content is (re)built here with the live now so the
	// RUNNING rows' elapsed timers tick every frame; layout() has already set
	// the same content sans elapsed to size the viewport and clamp its offset
	// (elapsed is inline, so line counts match).
	vp := m.bodyVp
	vp.SetContent(m.renderBody(inner, now))
	b.WriteString(vp.View())
	b.WriteString("\n")

	b.WriteString(m.renderActivityPanel(inner))
	b.WriteString("\n")
	b.WriteString(m.renderFooter())
	return b.String()
}

// layout recomputes widget sizes and viewport content from the current model
// state. It is pure: the render helpers it calls take now=0 (elapsed/age are
// display-only and don't affect line counts), so it never reads wall-clock.
// Called after every state change that can alter sizing (snapshot, resize,
// selection, mode).
func (m *Model) layout() {
	width := m.width
	if width <= 0 {
		width = 96
	}
	inner := width - 4
	if inner < 24 {
		inner = 24
	}
	height := m.height
	if height <= 0 {
		height = 40
	}

	m.help.Width = width
	m.progress.Width = clampInt(inner/2, 12, 48)

	// Activity feed viewport, sized to its panel's inner text width.
	m.activityVp.Width = inner - 2
	actN := len(m.recentEvents)
	if actN == 0 {
		actN = 1
	}
	m.activityVp.Height = clampInt(actN, 1, 8)
	m.activityVp.SetContent(strings.Join(m.activityLines(m.activityVp.Width), "\n"))
	activityPanelH := m.activityVp.Height + 3 // heading (1) + border (2)

	headerH := lipgloss.Height(m.renderHeader(inner, 0))
	footerH := 1

	// Body viewport: fit content when it fits, else cap to the remaining
	// space so it scrolls. Content set with now=0 for a stable line count.
	// Width matches the section boxes' outer width (inner + 2 borders) so the
	// viewport pads its lines flush with the header rather than 2 cols wider.
	m.bodyVp.Width = inner + 2
	m.bodyVp.SetContent(m.renderBody(inner, 0))
	contentH := m.bodyVp.TotalLineCount()
	avail := height - headerH - activityPanelH - footerH - 3 // 3 = inter-panel newlines
	if avail < 3 {
		avail = 3
	}
	bodyH := contentH
	if bodyH > avail {
		bodyH = avail
	}
	if bodyH < 1 {
		bodyH = 1
	}
	m.bodyVp.Height = bodyH
	m.bodyVp.SetYOffset(m.bodyVp.YOffset) // re-clamp against the new bounds

	// Detail viewport fills the space below the header (its content carries
	// its own heading, so there is no wrapping box to budget for).
	m.detailVp.Width = width
	detailAvail := height - headerH - footerH - 3
	if detailAvail < 3 {
		detailAvail = 3
	}
	m.detailVp.Height = detailAvail
	m.detailVp.SetContent(m.detailContent(inner))
	m.detailVp.SetYOffset(m.detailVp.YOffset)
}

// ensureSelectionVisible scrolls the body viewport just enough to keep the
// selected row on screen after a cursor move. It relies on orderedLineIndex,
// which mirrors renderBody's panel geometry.
func (m *Model) ensureSelectionVisible() {
	if len(m.ordered) == 0 || m.bodyVp.Height <= 0 {
		return
	}
	li := m.orderedLineIndex(m.selectedIndex())
	top := m.bodyVp.YOffset
	switch {
	case li < top:
		m.bodyVp.SetYOffset(li)
	case li >= top+m.bodyVp.Height:
		m.bodyVp.SetYOffset(li - m.bodyVp.Height + 1)
	}
}

// renderHeader draws the top panel: the "clipse" title, a liveness dot with a
// "last activity" age, status-count chips, a completion progress bar with a
// rough cost estimate, and the cumulative token counters.
func (m Model) renderHeader(inner int, now int64) string {
	titleLine := lipgloss.JoinHorizontal(lipgloss.Center,
		titleStyle.Render("clipse"),
		"  ",
		subtitleStyle.Render("pipeline dashboard"),
		"   ",
		m.livenessBadge(now),
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

	progressLine := m.renderProgress()

	tokens := lipgloss.JoinHorizontal(lipgloss.Center,
		dimStyle.Render("tokens  "),
		lipgloss.NewStyle().Foreground(cCyan).Render("↓ "),
		tokenNumStyle.Render(humanizeTokens(m.tokensIn)),
		dimStyle.Render(" in    "),
		lipgloss.NewStyle().Foreground(cPurple).Render("↑ "),
		tokenNumStyle.Render(humanizeTokens(m.tokensOut)),
		dimStyle.Render(" out"),
	)

	body := lipgloss.JoinVertical(lipgloss.Left, titleLine, "", chips, "", progressLine, "", tokens)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cCyan).
		Padding(0, 1).
		Width(inner).
		Render(body)
}

// livenessBadge renders the dispatcher liveness dot plus a "last activity Ns
// ago" reading. The dot is authoritative (it reflects whether a dispatcher
// holds the singleton lock); the age is a data-freshness hint derived from the
// newest event's timestamp.
func (m Model) livenessBadge(now int64) string {
	var dot string
	if m.live {
		dot = liveStyle.Render("● live")
	} else {
		dot = idleStyle.Render("○ idle")
	}
	var age string
	if m.lastEventAt == 0 {
		age = dimStyle.Render("no activity yet")
	} else {
		age = dimStyle.Render("updated " + formatAge(now-m.lastEventAt) + " ago")
	}
	return lipgloss.JoinHorizontal(lipgloss.Center, dot, "  ", age)
}

// renderProgress draws the completion progress bar (done/total issues) via the
// bubbles progress widget, rendered statically with ViewAs so Update carries
// none of its animation state, plus a rough "$ spent" estimate.
func (m Model) renderProgress() string {
	var frac float64
	if m.totalIssues > 0 {
		frac = float64(m.doneCount) / float64(m.totalIssues)
	}
	bar := m.progress.ViewAs(frac)
	label := dimStyle.Render(fmt.Sprintf("  %d/%d done", m.doneCount, m.totalIssues))
	cost := costStyle.Render(fmt.Sprintf("   ~$%.2f spent", estimateCostUSD(m.tokensIn, m.tokensOut)))
	return lipgloss.JoinHorizontal(lipgloss.Center, bar, label, cost)
}

// renderFooter draws the one-line key hints via the bubbles help widget
// (short form). '?' toggles the expanded overlay.
func (m Model) renderFooter() string {
	return footerStyle.Render(m.help.View(m.keys))
}

// renderHelpScreen overlays the full, column-grouped keybinding list (bubbles
// help, ShowAll) beneath the header.
func (m Model) renderHelpScreen(inner int, now int64) string {
	h := m.help
	h.ShowAll = true
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cCyan).
		Padding(0, 1).
		Width(inner).
		Render(lipgloss.JoinVertical(lipgloss.Left, panelHeadStyle.Render("HELP — keybindings"), "", h.View(m.keys)))
	return m.renderHeader(inner, now) + "\n\n" + box + "\n" + footerStyle.Render("esc / ? to close")
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

// bareLane strips the "agent:" prefix a Linear label may still carry, so the
// badge colors/labels match whether the store holds a bare lane (the kernel
// invariant) or a prefixed one.
func bareLane(lane string) string {
	return strings.TrimPrefix(lane, "agent:")
}

// laneBadge renders a lane as a filled color chip (or a dim placeholder for an
// issue with no lane yet).
func laneBadge(lane string) string {
	bare := bareLane(lane)
	if bare == "" {
		return dimStyle.Render("· · ·")
	}
	return lipgloss.NewStyle().
		Foreground(cInk).Background(laneColor(bare)).Bold(true).Padding(0, 1).
		Render(bare)
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

// formatElapsed renders how long run has been running (StartedAt unix vs. the
// supplied now) as m:ss, e.g. "1:04". now is passed in (never read from the
// clock) so callers control determinism; View supplies the wall clock.
func formatElapsed(run *store.Run, now int64) string {
	if run == nil || run.StartedAt == 0 {
		return "—"
	}
	secs := now - run.StartedAt
	if secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

// formatAge renders a non-negative second count as a compact relative age:
// "8s", "3m", "2h", "5d".
func formatAge(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	case secs < 86400:
		return fmt.Sprintf("%dh", secs/3600)
	default:
		return fmt.Sprintf("%dd", secs/86400)
	}
}

// clampInt bounds v to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// maxInt returns the larger of a, b.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// truncatePlain trims an unstyled string to max runes, marking a cut with an
// ellipsis. Used before styling so no ANSI escape is ever split.
func truncatePlain(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
