package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/store"
)

// activityLines formats the recent-events feed (newest-first) into one aligned
// line each: "HH:MM:SS  <lane-dot> <id>  <glyph> <kind>  <detail>". Kinds are
// split into two classes (P3): verdicts — the board-moving outcomes — render a
// bold colored label with the prose detail kept at full text weight (it's the
// reviewer's voice); mechanics — kernel bookkeeping — render entirely dim with
// kernel-speak translated and long hex ids shortened. A pending refresh error
// is surfaced as the first line. Formatting a fixed event ts (time.Unix) is
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

	// Fixed lead columns: ts(8) + gap(2) + lane dot(1) + gap(1) + id(7) +
	// gap(2) + glyph(1) + gap(1) + label(11) + gap(2). The detail fills
	// whatever remains.
	const lead = 8 + 2 + 1 + 1 + 7 + 2 + 1 + 1 + 11 + 2

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
		identText := fmt.Sprintf("%-7s", truncatePlain(ident, 7))

		class := classifyEvent(e.Kind)
		glyph, color := eventGlyph(e.Kind)
		label := fmt.Sprintf("%-11s", kindLabel(e.Kind))

		idCell := lipgloss.NewStyle().Foreground(cText).Render(identText)
		kindCell := lipgloss.NewStyle().Foreground(color).Render(glyph + " " + label)
		detailStyle := dimStyle
		switch class {
		case classVerdict:
			// Verdicts are the only loud feed lines: bold label, prose kept
			// at full text weight.
			kindCell = lipgloss.NewStyle().Foreground(color).Bold(true).Render(glyph + " " + label)
			detailStyle = lipgloss.NewStyle().Foreground(cText)
		case classMechanic:
			// Mechanics are bookkeeping: the whole line goes quiet.
			idCell = dimStyle.Render(identText)
			kindCell = dimStyle.Render(glyph + " " + label)
		}

		detail := truncatePlain(cleanActivityDetail(e.Kind, e.Detail), maxInt(width-lead, 6))

		lines = append(lines, ts+"  "+m.laneDot(e)+" "+idCell+"  "+kindCell+"  "+detailStyle.Render(detail))
	}
	return lines
}

// laneDot renders a 1-cell lane marker for a feed row, colored by the lane
// of the run the event references (lane color is identity — the same
// cyan/purple/orange code the row badges use). An event with no resolvable
// run lane gets a dim placeholder dot.
func (m Model) laneDot(e store.Event) string {
	if e.RunID.Valid {
		if lane := m.laneByRunID[e.RunID.String]; lane != "" {
			return lipgloss.NewStyle().Foreground(laneColor(bareLane(lane))).Render("●")
		}
	}
	return dimStyle.Render("·")
}

// eventClass buckets an event kind for the feed's two-class treatment (P3):
// verdicts are the board-moving outcomes and the only loud lines; mechanics
// are kernel bookkeeping and always render dim; everything else (open_review
// hand-offs, unknown kinds) keeps the neutral middle weight.
type eventClass int

const (
	classNeutral eventClass = iota
	classVerdict
	classMechanic
)

// classifyEvent maps an event kind to its class. Matching is substring-based
// (like eventGlyph/kindLabel) so kind variants — auto_merged, comment_block,
// orphan_requeue — land in the right bucket without an exhaustive list.
func classifyEvent(kind string) eventClass {
	switch {
	case strings.Contains(kind, "merge"),
		kind == "done",
		kind == "complete",
		strings.Contains(kind, "block"),
		strings.Contains(kind, "request"),
		strings.Contains(kind, "changes"),
		strings.Contains(kind, "cap"):
		return classVerdict
	case strings.Contains(kind, "claim"),
		kind == "promoted",
		kind == "adopted",
		strings.Contains(kind, "stale"),
		strings.Contains(kind, "release"),
		kind == "retry_scheduled",
		strings.Contains(kind, "orphan"),
		kind == "respawn":
		return classMechanic
	default:
		return classNeutral
	}
}

// kindLabel maps a raw event kind to a short, human label that fits the feed's
// fixed kind column without truncating mid-word. Order matters: the stale
// check must precede the claim check so "stale_release" reads "requeued", and
// the cap check must precede "request"/"changes" ones staying as-is.
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
	case strings.Contains(kind, "stale") || strings.Contains(kind, "release"):
		return "requeued"
	case kind == "retry_scheduled":
		return "retry"
	case strings.Contains(kind, "orphan"):
		return "orphaned"
	case strings.Contains(kind, "claim"):
		return "claimed"
	case strings.Contains(kind, "block"):
		return "blocked"
	default:
		return truncatePlain(strings.ReplaceAll(kind, "_", " "), 11)
	}
}

var (
	// staleColRe pulls the requeue target out of store.ReleaseStaleClaims's
	// detail: "released stale claim <token> (column <from> -> <to>)".
	staleColRe = regexp.MustCompile(`\(column \S+ -> (\S+)\)`)
	// retryRe pulls attempt/cap/reason out of dispatcher's retry detail:
	// "auto-retry <n>/<cap> after transient failure: <reason>".
	retryRe = regexp.MustCompile(`^auto-retry (\d+)/(\d+) after transient failure: (.*)$`)
	// hexIDRe matches the long hex identifiers (claim tokens, run/issue ids)
	// that leak into event details.
	hexIDRe = regexp.MustCompile(`[0-9a-f]{16,}`)
)

// cleanActivityDetail collapses an event detail to one tidy line and
// translates kernel-speak into operator language (P3): a "claimed" detail is
// reduced to its short run id; a "stale_release" reads "claim expired —
// requeued in <col>" instead of a 32-char claim token and a "merging ->
// merging" arrow; a "retry_scheduled" reads "transient failure — retry
// <n>/<cap>: <reason>"; every remaining long hex id is shortened. An
// unparseable detail degrades to the flattened original — translation must
// never hide an event.
func cleanActivityDetail(kind, detail string) string {
	d := oneLine(detail)
	switch {
	case strings.Contains(kind, "claim"):
		d = strings.TrimPrefix(d, "claimed by run ")
		return "run " + shortID(d)
	case strings.Contains(kind, "stale"):
		if m := staleColRe.FindStringSubmatch(d); m != nil {
			return "claim expired — requeued in " + m[1]
		}
		return "claim expired — requeued"
	case kind == "retry_scheduled":
		if m := retryRe.FindStringSubmatch(d); m != nil {
			return fmt.Sprintf("transient failure — retry %s/%s: %s", m[1], m[2], m[3])
		}
		return shortenHexIDs(d)
	default:
		return shortenHexIDs(d)
	}
}

// shortenHexIDs truncates every long hex id in s to its first 8 chars, the
// same display form shortID gives a bare id.
func shortenHexIDs(s string) string {
	return hexIDRe.ReplaceAllStringFunc(s, shortID)
}

// eventGlyph maps an event kind to a leading glyph and color: merges/dones are
// green ✓, blocks are red ✖, claims cyan ▶, reviews cyan ◆, rework/changes
// amber ⟳, requeue/retry/orphan mechanics a dim ·, and everything else a dim ·.
func eventGlyph(kind string) (string, lipgloss.AdaptiveColor) {
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
	case strings.Contains(kind, "stale") || strings.Contains(kind, "release") || kind == "retry_scheduled" || strings.Contains(kind, "orphan"):
		return "·", cDim
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
