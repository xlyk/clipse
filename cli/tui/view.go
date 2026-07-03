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

	// Panel framing: one subtle rounded border shared by every panel so the
	// whole dashboard reads as a single system. The accent lives on the title,
	// never the border.
	panelBorderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1)
	panelTitleStyle  = lipgloss.NewStyle().Bold(true)
	ruleStyle        = lipgloss.NewStyle().Foreground(cBorder)

	// Tab switcher: the active view is a filled chip, inactive views are dim.
	tabActiveStyle   = lipgloss.NewStyle().Bold(true).Foreground(cInk).Background(cCyan).Padding(0, 1)
	tabInactiveStyle = lipgloss.NewStyle().Foreground(cDim).Padding(0, 1)
)

// section describes one dashboard group: its title, the accent color that
// tints its border + heading, the glyph that leads each of its rows, whether
// its rows show a live elapsed/spinner (only RUNNING does), and whether it is
// the QUEUED group (whose rows get a dependency "waiting on …" hint).
type section struct {
	title  string
	accent lipgloss.Color
	glyph  string
	rows   []Row
	// waiting marks the QUEUED section, whose rows render a "waiting on …"
	// dependency hint instead of run metadata. (Liveness is per-row — see
	// Row.Live — not a section-wide flag.)
	waiting bool
}

// layoutDims is the per-frame geometry derived purely from the terminal size
// and current mode. Both layout() (which sizes the viewports) and the render
// helpers read it, so panel sizes can never drift between the two. cw is the
// width passed to a full-span bordered box's .Width() (its outer width is
// cw+2 = frameW); textW inside such a box is cw-2 after the 1-col padding.
type layoutDims struct {
	frameW, frameH int
	cw             int
	headerH        int
	tabsH          int
	footerH        int
	bodyH          int // rows available to the body between tabs and footer

	pipeTextW   int
	actTextW    int
	pipeH, actH int // full heights of the stacked pipeline / activity panels
	pipeVpH     int // pipeline viewport height (panel − border − title)
	actVpH      int
}

// dims computes the frame geometry. It is pure (now=0 into the measured
// helpers) so it is safe to call from both Update's layout() and View.
func (m Model) dims() layoutDims {
	w := m.width
	if w <= 0 {
		w = 120
	}
	h := m.height
	if h <= 0 {
		h = 40
	}
	d := layoutDims{frameW: w, frameH: h}
	d.cw = maxInt(w-2, 24)

	d.headerH = lipgloss.Height(m.renderHeader(d.cw, 0))
	d.tabsH = lipgloss.Height(m.renderTabs(d.cw))
	d.footerH = lipgloss.Height(m.renderFooter(d.cw))
	d.bodyH = maxInt(h-d.headerH-d.tabsH-d.footerH, 6)

	// Panels stack vertically: PIPELINE on top, the ACTIVITY feed as a
	// full-width band below it (the feed reads better full-width under the
	// pipeline than squeezed into a side column). Activity gets a bounded
	// bottom band so the pipeline keeps the majority of the height.
	d.actH = clampInt(d.bodyH*2/5, 6, 18)
	d.pipeH = maxInt(d.bodyH-d.actH, 4)
	d.pipeTextW = maxInt(d.cw-2, 8)
	d.actTextW = maxInt(d.cw-2, 8)
	d.pipeVpH = maxInt(d.pipeH-3, 1) // border(2) + title(1)
	d.actVpH = maxInt(d.actH-3, 1)
	return d
}

// View renders the active screen. It is the one place wall-clock time enters
// the TUI: the pure Update never calls time.Now, so all "elapsed"/"ago"
// readouts are computed here against a single now captured per frame.
func (m Model) View() string {
	now := time.Now().Unix()
	d := m.dims()

	if m.showHelp {
		return m.renderHelpScreen(now)
	}
	switch m.mode {
	case modeDetail:
		return m.renderDetailScreen(d.cw, now)
	case modeKanban:
		return m.renderKanbanScreen(d.cw, now)
	default:
		return m.renderDashboard(now)
	}
}

// renderDashboard is the default view: a dense header, a tab bar, a stacked
// body (the PIPELINE panel above the full-width ACTIVITY feed) that fills the
// whole terminal height, and a pinned footer. Every region is sized from
// dims() so header+tabs+body+footer sum to exactly the frame height — no dead
// space, footer flush to the bottom.
func (m Model) renderDashboard(now int64) string {
	d := m.dims()

	// Rebuild the pipeline content with the live now so RUNNING rows' elapsed
	// timers tick every frame; layout() already sized/clamped the viewport off
	// the now=0 content (elapsed is inline, so the line counts match).
	pipeVp := m.bodyVp
	pipeVp.SetContent(m.renderBody(d.pipeTextW, now))
	pipe := panelBox("PIPELINE", cText, pipeVp.View(), d.cw, d.pipeH)
	feed := panelBox("⚡ ACTIVITY", cAmber, m.activityVp.View(), d.cw, d.actH)
	body := lipgloss.JoinVertical(lipgloss.Left, pipe, feed)

	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(d.cw, now),
		m.renderTabs(d.cw),
		body,
		m.renderFooter(d.cw),
	)
}

// panelBox frames a titled panel: an accent-colored title over its body inside
// the shared subtle rounded border. colW is the .Width() arg (outer = colW+2);
// totalH is the panel's full height including the border, so body must already
// be (totalH−3) lines tall — one title line plus (totalH−3) body lines fills
// the (totalH−2) content box exactly.
func panelBox(title string, accent lipgloss.Color, body string, colW, totalH int) string {
	head := panelTitleStyle.Foreground(accent).Render(title)
	return panelBorderStyle.
		Width(colW).
		Height(maxInt(totalH-2, 1)).
		Render(lipgloss.JoinVertical(lipgloss.Left, head, body))
}

// layout recomputes widget sizes and viewport content from the current model
// state. It is pure: the render helpers it calls take now=0 (elapsed/age are
// display-only and don't affect line counts), so it never reads wall-clock.
// Called after every state change that can alter sizing (snapshot, resize,
// selection, mode).
func (m *Model) layout() {
	d := m.dims()

	m.help.Width = d.cw
	m.progress.Width = clampInt(d.cw/3, 16, 60)

	// Pipeline viewport (the left / top panel body).
	m.bodyVp.Width = d.pipeTextW
	m.bodyVp.Height = d.pipeVpH
	m.bodyVp.SetContent(m.renderBody(d.pipeTextW, 0))
	m.bodyVp.SetYOffset(m.bodyVp.YOffset) // re-clamp against the new bounds

	// Activity viewport (the right / bottom panel body). Newest-first content,
	// top-anchored, so a full feed scrolls under the wheel / pgup.
	m.activityVp.Width = d.actTextW
	m.activityVp.Height = d.actVpH
	m.activityVp.SetContent(strings.Join(m.activityLines(d.actTextW), "\n"))
	m.activityVp.SetYOffset(m.activityVp.YOffset)

	// Detail viewport is the body of the ISSUE DETAIL panel: it fills the space
	// under the header (the detail screen has no tab bar) minus the panel's own
	// border(2) and title(1). The panel width's padding leaves cw−2 for text.
	detailW := maxInt(d.cw-2, 8)
	m.detailVp.Width = detailW
	m.detailVp.Height = maxInt(d.frameH-d.headerH-d.footerH-3, 1)
	m.detailVp.SetContent(m.detailContent(detailW))
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

// renderHeader draws the top panel in three dense rows — title + liveness, the
// status-count chips, and the progress bar + cost + token counters — inside a
// cyan-bordered box. The liveness badge and token counters are pushed flush
// right on their rows, so the header spends horizontal width, not the vertical
// blank lines it used to.
func (m Model) renderHeader(cw int, now int64) string {
	textW := maxInt(cw-2, 8)

	row1 := padBetween(
		lipgloss.JoinHorizontal(lipgloss.Center,
			titleStyle.Render("clipse"), " ",
			subtitleStyle.Render("pipeline dashboard"),
		),
		m.livenessBadge(now),
		textW,
	)

	chips := lipgloss.JoinHorizontal(lipgloss.Center,
		countChip("▶", "running", m.count("running"), cGreen), "   ",
		countChip("◐", "in flight", m.inFlightCount(), cCyan), "   ",
		countChip("•", "queued", m.count("ready")+m.count("todo"), cAmber), "   ",
		countChip("✖", "blocked", m.count("blocked"), cRed), "   ",
		countChip("✓", "done", m.count("done"), cPurple),
	)

	row3 := padBetween(m.renderProgress(), m.renderTokens(), textW)

	body := lipgloss.JoinVertical(lipgloss.Left, row1, chips, row3)
	return panelBorderStyle.Width(cw).BorderForeground(cCyan).Render(body)
}

// renderTabs draws the view switcher (dashboard / board) as a chip row with a
// right-aligned hint. The active view is a filled chip; the other is dim.
func (m Model) renderTabs(cw int) string {
	mk := func(icon, label string, active bool) string {
		if active {
			return tabActiveStyle.Render(icon + " " + label)
		}
		return tabInactiveStyle.Render(icon + " " + label)
	}
	tabs := lipgloss.JoinHorizontal(lipgloss.Center,
		mk("▚", "dashboard", m.mode == modeDashboard), " ",
		mk("▦", "board", m.mode == modeKanban),
	)
	return padBetween(tabs, dimStyle.Render("tab switches view  "), cw)
}

// renderTokens renders the cumulative token counters (↓ in / ↑ out).
func (m Model) renderTokens() string {
	return lipgloss.JoinHorizontal(lipgloss.Center,
		dimStyle.Render("tokens "),
		lipgloss.NewStyle().Foreground(cCyan).Render("↓"),
		tokenNumStyle.Render(humanizeTokens(m.tokensIn)),
		dimStyle.Render(" in  "),
		lipgloss.NewStyle().Foreground(cPurple).Render("↑"),
		tokenNumStyle.Render(humanizeTokens(m.tokensOut)),
		dimStyle.Render(" out"),
	)
}

// padBetween joins left and right with a space filler so right sits flush
// against width, giving a justified line. The two never touch (min 1-col gap).
func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
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
	segs := []string{dot, "  ", age}
	// A prominent tally of how many agents are actively working right now
	// (claims held, across every lane) so concurrent coder/reviewer work is
	// legible without scanning the sections for spinners.
	if n := m.workingCount(); n > 0 {
		segs = append(segs, "  ", lipgloss.NewStyle().Foreground(cGreen).Bold(true).Render(fmt.Sprintf("⚡ %d working", n)))
	}
	return lipgloss.JoinHorizontal(lipgloss.Center, segs...)
}

// workingCount is the number of issues a worker is actively on right now (a
// held dispatcher claim), across every lane. It sums Row.Live over the ordered
// rows (blocked/queued rows are never live), giving the header its live-agent
// tally.
func (m Model) workingCount() int {
	n := 0
	for _, r := range m.ordered {
		if r.Live {
			n++
		}
	}
	return n
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

// renderFooter draws the pinned bottom bar: a thin full-width rule, then the
// key hints (bubbles help, short form) on the left with a "mode · selection"
// context flush right.
func (m Model) renderFooter(cw int) string {
	ctx := m.ViewMode()
	if m.selected != "" {
		ctx += " · " + m.selected
	}
	line := padBetween(footerStyle.Render(m.help.View(m.keys)), dimStyle.Render(ctx), cw)
	return lipgloss.JoinVertical(lipgloss.Left, ruleStyle.Render(strings.Repeat("─", cw)), line)
}

// renderHelpScreen overlays the full, column-grouped keybinding list (bubbles
// help, ShowAll) as a card centered in the body area, beneath the header and
// tab bar, so the frame stays full-height and consistent with the dashboard.
func (m Model) renderHelpScreen(now int64) string {
	d := m.dims()
	h := m.help
	h.ShowAll = true
	cardW := clampInt(d.cw*3/5, 40, 84)
	card := panelBorderStyle.Width(cardW).BorderForeground(cCyan).Render(
		lipgloss.JoinVertical(lipgloss.Left,
			panelTitleStyle.Foreground(cCyan).Render("HELP · keybindings"), "",
			h.View(m.keys),
		),
	)
	body := lipgloss.Place(d.cw, d.bodyH, lipgloss.Center, lipgloss.Center, card)
	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(d.cw, now),
		m.renderTabs(d.cw),
		body,
		m.renderFooter(d.cw),
	)
}

// count returns the board-wide number of issues in a given board_status.
func (m Model) count(status string) int { return m.counts[status] }

// inFlightCount sums the downstream active columns for the header chip.
func (m Model) inFlightCount() int {
	return m.count("review") + m.count("rework") + m.count("merging")
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
