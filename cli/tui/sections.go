package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// sectionList returns the dashboard groups in render/navigation order.
// The order here MUST match fold's construction of m.ordered and
// orderedLineIndex's geometry, since the selection cursor walks them in
// lockstep.
func (m Model) sectionList() []section {
	return []section{
		{title: "ACTIVE", accent: cGreen, glyph: "⚡", rows: m.active, dimIdle: true},
		{title: "BLOCKED", accent: cRed, glyph: "✖", rows: m.blocked},
		{title: "QUEUED", accent: cAmber, glyph: "•", rows: m.queued, waiting: true},
	}
}

// visibleSections filters sectionList down to the sections that actually
// render: empty sections are omitted from the body entirely (their
// zero-counts live in the header chips). It is the ONE skip predicate shared
// by renderBody and orderedLineIndex, so the rendered geometry and the
// selection-index math can never diverge — a drift there breaks
// selection-follow scrolling.
func (m Model) visibleSections() []section {
	var vis []section
	for _, s := range m.sectionList() {
		if len(s.rows) == 0 {
			continue
		}
		vis = append(vis, s)
	}
	return vis
}

// renderBody renders the scrollable dashboard body: the non-empty section
// panels stacked, then a compact DONE summary. Empty sections are omitted
// entirely (visibleSections) — their zero-counts already live in the header
// chips — so a sparse board never spends rows saying "none" (P1). now feeds
// the live rows' elapsed (View passes the wall clock; layout passes 0 for a
// stable line count, since elapsed is inline and never adds lines).
func (m Model) renderBody(inner int, now int64) string {
	var parts []string
	for _, s := range m.visibleSections() {
		parts = append(parts, m.renderSection(s, inner, now))
	}
	body := strings.Join(parts, "\n")
	if done := m.renderDoneSummary(inner); done != "" {
		if body != "" {
			body += "\n"
		}
		body += done
	}
	if body == "" {
		return dimStyle.Render("no issues on the board yet")
	}
	return body
}

// renderSection renders one titled group of rows, tinted with the section's
// accent color. Callers skip empty sections (renderBody / orderedLineIndex),
// so there is no empty-placeholder branch. It is borderless — the enclosing
// PIPELINE panel supplies the single frame — so the groups read as one board
// rather than separate boxes.
func (m Model) renderSection(s section, inner int, now int64) string {
	head := lipgloss.NewStyle().Bold(true).Foreground(s.accent).Render(s.glyph+" "+s.title) +
		dimStyle.Render(fmt.Sprintf(" (%d)", len(s.rows)))
	// Extend the heading into a full-width divider so each group reads as a
	// labeled band, giving the board structure even when it is sparse.
	if fill := inner - lipgloss.Width(head) - 1; fill > 0 {
		head += " " + ruleStyle.Render(strings.Repeat("─", fill))
	}

	lines := []string{head}
	for _, row := range s.rows {
		lines = append(lines, m.renderRow(row, s, inner, now))
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

	// Liveness is per-row (an active claim = a worker on it now), so the
	// spinner lights up for a working agent in ANY lane — reviewer or
	// git_operator — not only the coder. An unclaimed ACTIVE row is parked
	// awaiting its next pickup: dim diamond, dim identifier (P2).
	lead := lipgloss.NewStyle().Foreground(s.accent).Render(s.glyph)
	switch {
	case row.Live:
		lead = lipgloss.NewStyle().Foreground(cGreen).Render(spinnerFrames[m.frame%len(spinnerFrames)])
	case s.dimIdle:
		lead = dimStyle.Render("◇")
	}

	idText := fmt.Sprintf("%-9s", row.Identifier)
	idCell := idStyle.Render(idText)
	if s.dimIdle && !row.Live {
		idCell = dimStyle.Render(idText)
	}
	if selected {
		idCell = selIDStyle.Render(idText)
	}

	// Badge the lane actually working the card when it's live (reviewer while
	// a review card is being reviewed, git_operator while it merges), falling
	// back to the issue's home label when nothing is actively on it.
	badgeLane := row.LaneLabel
	if row.Live && row.ActiveLane != "" {
		badgeLane = row.ActiveLane
	}

	// Fixed-width, left-aligned cells so lane / id / status / meta read as an
	// aligned table row, rather than flinging the metadata to the far edge of a
	// wide panel where it reads as disconnected from its row.
	badgeCell := lipgloss.NewStyle().Width(15).Render(laneBadge(badgeLane))
	statusCell := lipgloss.NewStyle().Width(13).Render(statusChip(row.Status))

	line := mark + lipgloss.JoinHorizontal(lipgloss.Center,
		lead, " ",
		badgeCell, " ",
		idCell, " ",
		statusCell, "  ",
		m.rowDetail(row, s, now),
	)
	// Bound the composed line to the panel's text width: a non-live row can
	// stack turn + ⟳ rework + tokens + retry countdown + ⇅ linear pending —
	// wider than a narrow panel exactly when a Linear outage plus a transient
	// burst lights every chip at once. MaxWidth truncates ANSI-aware (never
	// splitting a styled chip's escape sequence), and keeping the row to one
	// line also protects orderedLineIndex's wrap-free geometry.
	return lipgloss.NewStyle().MaxWidth(inner).Render(line)
}

// rowDetail renders the trailing metadata. For a QUEUED row with unmet
// dependencies it shows a "waiting on …" hint instead; otherwise it shows
// turn count, the rework/recover chip, cumulative tokens, a retry-backoff
// countdown, an outbox-pending badge, and — for a live row — elapsed runtime
// plus a stale-heartbeat warning (P4).
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
	if row.ReworkCount > 0 || row.RecoverAttempts > 0 {
		// ⟳ ×<rework> r<recover>: how many times this card bounced back to
		// the coder, and how many transient-failure auto-retries it has
		// burned. The caps live in clipse.yaml, which the TUI deliberately
		// doesn't load, so the counts render bare.
		chip := "⟳"
		if row.ReworkCount > 0 {
			chip += fmt.Sprintf(" ×%d", row.ReworkCount)
		}
		if row.RecoverAttempts > 0 {
			chip += fmt.Sprintf(" r%d", row.RecoverAttempts)
		}
		parts = append(parts, waitingStyle.Render(chip))
	}
	if row.TokensIn > 0 || row.TokensOut > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cCyan).Render("↓"+humanizeTokens(row.TokensIn))+
			dimStyle.Render(" ")+
			lipgloss.NewStyle().Foreground(cPurple).Render("↑"+humanizeTokens(row.TokensOut)))
	}
	if !row.Live && row.BlockedUntil > now {
		// The retry-backoff countdown: the kernel sets blocked_until on a
		// re-queued card (its release column), making it invisible to every
		// claim until the window passes — show when it becomes claimable.
		parts = append(parts, waitingStyle.Render("retry in "+formatAge(row.BlockedUntil-now)))
	}
	if row.Unmirrored {
		parts = append(parts, waitingStyle.Render("⇅ linear pending"))
	}
	if row.Live {
		parts = append(parts, lipgloss.NewStyle().Foreground(cGreen).Render("⏱ "+formatElapsed(row.Run, now)))
		if row.Run != nil && row.Run.HeartbeatAt > 0 && now-row.Run.HeartbeatAt > staleHeartbeatS {
			// The claim is heartbeated every dispatcher tick; this much
			// silence on a still-claimed row means a wedged worker or a
			// stopped dispatcher.
			parts = append(parts, waitingStyle.Render("♥ "+formatAge(now-row.Run.HeartbeatAt)))
		}
	}
	if len(parts) == 0 {
		return dimStyle.Render("—")
	}
	return strings.Join(parts, dimStyle.Render(" · "))
}

// renderDoneSummary renders a single compact line listing completed issues
// (dim) with their merged PR numbers when a run carried one — "CLI-52 #38" —
// so terminal "done" cards remain visible and satisfying (P6). Returns ""
// when nothing is done.
func (m Model) renderDoneSummary(inner int) string {
	done := m.byStatus["done"]
	if len(done) == 0 {
		return ""
	}
	idents := make([]string, 0, len(done))
	for _, r := range done {
		label := r.Identifier
		if is, ok := m.issuesByIdent[r.Identifier]; ok {
			if n := prNumber(prURLFromRuns(is.Runs)); n != "" {
				label += " " + n
			}
		}
		idents = append(idents, label)
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

// prNumber extracts a "#<digits>" display form from a PR URL's trailing path
// segment ("…/pull/38" → "#38"), or "" when the URL doesn't end in a number.
func prNumber(url string) string {
	i := strings.LastIndex(url, "/")
	if i < 0 || i == len(url)-1 {
		return ""
	}
	tail := url[i+1:]
	for _, r := range tail {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return "#" + tail
}

// orderedLineIndex returns the 0-based line, within renderBody's output, of the
// ordered row at global index g. It measures actual rendered heights rather
// than assuming one line per row, so a row that wraps at a narrow width can't
// drift the result: preceding groups are summed via lipgloss.Height of the
// whole (borderless) group, and rows preceding g within its group via
// lipgloss.Height of each rendered row (the heading is a single line). It
// walks the same visibleSections list renderBody renders, so empty sections
// are skipped by the identical predicate. Used to keep the selected row
// visible when scrolling the pipeline viewport.
func (m Model) orderedLineIndex(g int) int {
	inner := m.dims().pipeTextW

	line := 0
	seen := 0
	for _, s := range m.visibleSections() {
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
