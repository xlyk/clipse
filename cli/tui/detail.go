package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/store"
)

// workerResult is a minimal view of the worker's typed JSON result (a subset
// of the generated contract.WorkerResult) — just the fields the detail view
// surfaces. It is parsed leniently: a malformed or empty result_json yields a
// zero value rather than an error.
type workerResult struct {
	Outcome   string  `json:"outcome"`
	Summary   string  `json:"summary"`
	PrURL     *string `json:"pr_url"`
	BlockKind *string `json:"block_kind"`
}

// parseResult decodes a run's result_json, tolerating absent/empty/garbled
// JSON (returns the zero value, ok=false).
func parseResult(rj store.Run) (workerResult, bool) {
	if !rj.ResultJSON.Valid || rj.ResultJSON.String == "" {
		return workerResult{}, false
	}
	var wr workerResult
	if err := json.Unmarshal([]byte(rj.ResultJSON.String), &wr); err != nil {
		return workerResult{}, false
	}
	return wr, true
}

// renderDetailScreen draws the selected issue's detail: the header, then the
// detail content framed in a full-height panel (so it fills the body instead of
// floating in a void), then the pinned footer. esc returns to the dashboard.
// The three regions sum to the frame height, so the footer stays flush bottom.
func (m Model) renderDetailScreen(inner int, now int64) string {
	d := m.dims()
	panelH := maxInt(d.frameH-d.headerH-d.footerH, 3)
	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(d.cw, now),
		panelBox("◆ ISSUE DETAIL", cCyan, m.detailVp.View(), d.cw, panelH),
		m.renderFooter(d.cw),
	)
}

// detailContent builds the scrollable detail body for the selected issue:
// title, branch, PR URL, blockers, a block reason (when blocked), and the full
// per-lane run history. It reads no wall-clock (run times render as absolute
// clock times), so layout can set it deterministically.
func (m Model) detailContent(inner int) string {
	is, ok := m.issuesByIdent[m.selected]
	if !ok {
		return dimStyle.Render("no issue selected")
	}

	var b strings.Builder
	title := lipgloss.JoinHorizontal(lipgloss.Center,
		lipgloss.NewStyle().Bold(true).Foreground(cText).Render(is.Identifier),
		"  ", statusChip(is.BoardStatus),
		"  ", laneBadge(is.LaneLabel),
	)
	b.WriteString(title)
	b.WriteString("\n")
	if is.Title != "" {
		b.WriteString(dimStyle.Render(truncatePlain(is.Title, inner-2)))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if is.BranchName != "" {
		b.WriteString(detailField("branch", is.BranchName))
	}
	if pr := prURLFromRuns(is.Runs); pr != "" {
		b.WriteString(detailField("pr", pr))
	}
	if bs := blockers(is.Deps, m.identByID, m.statusByID); len(bs) > 0 {
		b.WriteString(detailField("blocked-by", renderBlockers(bs)))
	}

	if is.BoardStatus == "blocked" {
		if reason := blockReason(is.LatestRun); reason != "" {
			b.WriteString("\n")
			// Wrap (not just truncate) to the panel's text width so a long block
			// reason folds onto multiple lines instead of overflowing the border.
			w := maxInt(inner-2, 8)
			b.WriteString(lipgloss.NewStyle().Foreground(cRed).Width(w).
				Render("⚠ " + truncatePlain(reason, w*4)))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(panelHeadStyle.Render("RUNS"))
	b.WriteString("\n")
	if len(is.Runs) == 0 {
		b.WriteString(dimStyle.Render("  (no runs yet)"))
		b.WriteString("\n")
	}
	for _, r := range is.Runs {
		b.WriteString(runLine(r))
		b.WriteString("\n")
	}
	return b.String()
}

// detailField renders a "label  value" line for the detail header block.
func detailField(label, value string) string {
	return labelStyle.Render(fmt.Sprintf("%-11s", label)) + lipgloss.NewStyle().Foreground(cText).Render(value) + "\n"
}

// runLine renders one run of the history: lane badge, status, turn, cumulative
// tokens, and an absolute start time.
func runLine(r store.Run) string {
	when := "—"
	if r.StartedAt > 0 {
		when = time.Unix(r.StartedAt, 0).Format("01-02 15:04")
	}
	return "  " + lipgloss.JoinHorizontal(lipgloss.Center,
		lipgloss.NewStyle().Width(13).Render(laneBadge(r.Lane)), " ",
		// Truncate before the fixed-width cell so a long run status
		// (e.g. "changes_requested") pads rather than wraps to a second line.
		lipgloss.NewStyle().Width(16).Foreground(statusColor(r.Status)).Render(truncatePlain(r.Status, 16)), " ",
		dimStyle.Render(fmt.Sprintf("turn %d", r.TurnCount)), "  ",
		lipgloss.NewStyle().Foreground(cCyan).Render("↓"+humanizeTokens(r.TokensIn)), " ",
		lipgloss.NewStyle().Foreground(cPurple).Render("↑"+humanizeTokens(r.TokensOut)), "  ",
		dimStyle.Render(when),
	)
}

// renderBlockers formats resolved dependencies as "CLI-8 ✓, CLI-9 ⏳": a green
// check for a satisfied (terminal) dep, an amber hourglass for a pending one.
func renderBlockers(bs []blockerState) string {
	parts := make([]string, 0, len(bs))
	for _, b := range bs {
		mark := waitingStyle.Render("⏳")
		if b.Met {
			mark = lipgloss.NewStyle().Foreground(cGreen).Render("✓")
		}
		parts = append(parts, b.Identifier+" "+mark)
	}
	return strings.Join(parts, ", ")
}

// prURLFromRuns returns the most recent non-empty PR URL across an issue's
// runs (a later lane — reviewer/git-operator — may re-report the same PR), or
// "" when none carried one.
func prURLFromRuns(runs []store.Run) string {
	var url string
	for _, r := range runs {
		if wr, ok := parseResult(r); ok && wr.PrURL != nil && *wr.PrURL != "" {
			url = *wr.PrURL
		}
	}
	return url
}

// blockReason extracts a human-readable block reason from a blocked issue's
// latest run: the run's stored error if present, else the worker result's
// block_kind + summary. Returns "" when nothing informative is available.
func blockReason(run *store.Run) string {
	if run == nil {
		return ""
	}
	if run.Error.Valid && run.Error.String != "" {
		return oneLine(run.Error.String)
	}
	wr, ok := parseResult(*run)
	if !ok {
		return ""
	}
	reason := oneLine(wr.Summary)
	if wr.BlockKind != nil && *wr.BlockKind != "" {
		reason = "[" + *wr.BlockKind + "] " + reason
	}
	return reason
}
