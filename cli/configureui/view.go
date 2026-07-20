package configureui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/setup"
)

var (
	cyan    = lipgloss.AdaptiveColor{Light: "#0969da", Dark: "#58f6ff"}
	magenta = lipgloss.AdaptiveColor{Light: "#8250df", Dark: "#ff4fd8"}
	green   = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#8cff66"}
	amber   = lipgloss.AdaptiveColor{Light: "#9a6700", Dark: "#ffd166"}
	red     = lipgloss.AdaptiveColor{Light: "#cf222e", Dark: "#ff5f6d"}
	dim     = lipgloss.AdaptiveColor{Light: "#57606a", Dark: "#768390"}
	border  = lipgloss.AdaptiveColor{Light: "#afb8c1", Dark: "#33405b"}
)

func (m Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	header := m.renderHeader()
	body := m.renderBody()
	footer := m.renderFooter()
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m Model) renderHeader() string {
	progress := fmt.Sprintf("%02d/09", int(m.page)+1)
	if m.page > pageFinish {
		progress = "09/09"
	}
	title := "CLIPSE // CONFIG SYNTH"
	if !m.ascii {
		title = "◢ CLIPSE // CONFIG SYNTH ◣"
	}
	music := "MUSIC OFF"
	if m.musicOn {
		music = "♫ 125 BPM"
		if m.ascii {
			music = "MUSIC ON"
		}
	}
	left := m.paint(title, cyan, true)
	right := m.paint(progress+"  "+music, magenta, true)
	space := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right)-2)
	line := left + strings.Repeat(" ", space) + right
	if !m.noAnimation && m.width >= 76 {
		line += "\n" + m.paint(spectrum(m.width, m.phase, m.ascii), magenta, false)
	}
	return line
}

func (m Model) renderBody() string {
	content := m.renderPage()
	if m.width < 76 {
		return "\n" + content
	}
	railWidth := 22
	rail := m.renderRail(railWidth)
	contentWidth := max(30, m.width-railWidth-3)
	panel := lipgloss.NewStyle().Width(contentWidth).Render(content)
	return "\n" + lipgloss.JoinHorizontal(lipgloss.Top, rail, "   ", panel)
}

func (m Model) renderRail(width int) string {
	lines := make([]string, 0, len(pageNames)+2)
	lines = append(lines, m.paint("TRACK / SETUP", dim, true), "")
	for i, name := range pageNames {
		marker := "·"
		color := dim
		if m.ascii {
			marker = "."
		}
		if pageID(i) < m.page {
			marker, color = "✓", green
			if m.ascii {
				marker = "x"
			}
		} else if pageID(i) == m.page {
			marker, color = "◆", cyan
			if m.ascii {
				marker = ">"
			}
		}
		lines = append(lines, m.paint(fmt.Sprintf("%s %02d %-12s", marker, i+1, name), color, pageID(i) == m.page))
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

func (m Model) renderPage() string {
	title := pageNames[m.page]
	var body string
	switch m.page {
	case pageReview:
		body = m.renderReview()
	case pageFinish:
		body = m.renderFinish()
	default:
		body = m.renderFields()
	}
	return m.paint(title, cyan, true) + "\n" + m.paint(strings.Repeat("─", min(58, max(12, m.width-28))), border, false) + "\n\n" + body
}

func (m Model) renderFields() string {
	visible := m.visibleFields()
	if len(visible) == 0 {
		return "No fields on this page."
	}
	start, end := 0, len(visible)
	limit := max(3, m.height-11)
	if end-start > limit {
		start = max(0, m.cursor-limit/2)
		end = min(len(visible), start+limit)
		start = max(0, end-limit)
	}
	lines := make([]string, 0, (end-start)*3)
	for position := start; position < end; position++ {
		field := m.fields[visible[position]]
		selected := position == m.cursor
		marker := "  "
		if selected {
			marker = "> "
		}
		label := marker + field.label
		if field.advanced {
			label += " [ADV]"
		}
		lines = append(lines, m.paint(label, chooseColor(selected, cyan, dim), selected))
		value := field.input.View()
		if len(field.options) > 0 {
			value = "‹ " + field.input.Value() + " ›"
		}
		lines = append(lines, "    "+value)
		if selected {
			lines = append(lines, "    "+m.paint(field.help, dim, false))
		}
	}
	if len(m.teams) > 0 {
		return m.renderTeamPicker()
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderTeamPicker() string {
	lines := []string{m.paint("SELECT LINEAR TEAM", magenta, true), ""}
	for i, team := range m.teams {
		marker := "  "
		if i == m.teamCursor {
			marker = "> "
		}
		lines = append(lines, fmt.Sprintf("%s%-8s %s", marker, team.Key, team.Name))
	}
	lines = append(lines, "", m.paint("↑↓ select  enter accept  esc cancel", dim, false))
	return strings.Join(lines, "\n")
}

func (m Model) renderReview() string {
	tabs := []string{"READINESS", "YAML"}
	if len(m.original) > 0 {
		tabs = append(tabs, "DIFF")
	}
	for i := range tabs {
		if i == m.reviewTab {
			tabs[i] = "[" + tabs[i] + "]"
		}
	}
	tabLine := m.paint(strings.Join(tabs, "  "), magenta, true) + "\n\n"
	if m.reviewTab == 1 {
		return tabLine + m.renderDocument(string(m.raw))
	}
	if m.reviewTab == 2 && len(m.original) > 0 {
		return tabLine + m.renderDocument(simpleDiff(redactDocument(string(m.original)), string(m.raw)))
	}
	if m.busy {
		return tabLine + m.paint("SCANNING HOST + REMOTES…", magenta, true) + "\n\n" + m.paint("No resources will be created by this scan.", dim, false)
	}
	if !m.haveReport {
		return tabLine + m.paint("Readiness scan has not completed.", amber, true)
	}
	lines := []string{m.paint("OUTCOME  "+strings.ToUpper(string(m.report.Outcome)), outcomeColor(m.report.Outcome), true), ""}
	for _, result := range m.report.Results {
		glyph, color := "PASS", green
		if result.Severity == setup.SeverityWarning {
			glyph, color = "WARN", amber
		} else if result.Severity == setup.SeverityBlocked {
			glyph, color = "BLOCK", red
		}
		lines = append(lines, m.paint(fmt.Sprintf("[%s]", glyph), color, true)+" "+result.Summary)
		if result.Detail != "" {
			lines = append(lines, "        "+m.paint(result.Detail, dim, false))
		}
	}
	lines = append(lines, "", m.paint("ENTER/W write  R recheck  TAB preview  ESC edit", cyan, true))
	return tabLine + strings.Join(lines, "\n")
}

func (m Model) renderDocument(document string) string {
	lines := strings.Split(document, "\n")
	limit := max(4, m.height-12)
	maxOffset := max(0, len(lines)-limit)
	offset := min(m.reviewOffset, maxOffset)
	end := min(len(lines), offset+limit)
	visible := strings.Join(lines[offset:end], "\n")
	return visible + "\n\n" + m.paint(fmt.Sprintf("lines %d-%d/%d  ↑↓ scroll  TAB switch", offset+1, end, len(lines)), dim, false)
}

func simpleDiff(before, after string) string {
	oldLines := strings.Split(before, "\n")
	newLines := strings.Split(after, "\n")
	limit := max(len(oldLines), len(newLines))
	var out []string
	for i := 0; i < limit; i++ {
		var oldLine, newLine string
		if i < len(oldLines) {
			oldLine = oldLines[i]
		}
		if i < len(newLines) {
			newLine = newLines[i]
		}
		if oldLine == newLine {
			out = append(out, "  "+oldLine)
			continue
		}
		if oldLine != "" {
			out = append(out, "- "+oldLine)
		}
		if newLine != "" {
			out = append(out, "+ "+newLine)
		}
	}
	return strings.Join(out, "\n")
}

func redactDocument(document string) string {
	lines := strings.Split(document, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		sensitive := strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "api_key")
		if !sensitive {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			lines[i] = "# [REDACTED SENSITIVE COMMENT]"
			continue
		}
		if before, _, ok := strings.Cut(line, ":"); ok {
			lines[i] = before + ": [REDACTED]"
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderFinish() string {
	lines := []string{
		m.paint("CONFIGURATION LOCKED IN", green, true),
		"",
		"Config: " + m.result.WrittenPath,
		"Board:  " + m.draft.Config.BoardDir,
	}
	if m.result.BackupPath != "" {
		lines = append(lines, "Backup: "+m.result.BackupPath)
	}
	lines = append(lines, "", m.paint("Press Enter to return to the shell.", dim, false))
	return strings.Join(lines, "\n")
}

func (m Model) renderFooter() string {
	status := m.status
	if m.err != nil {
		status = m.paint("ERROR  "+m.err.Error(), red, true)
	}
	if status == "" {
		status = m.paint("↑↓ field  enter continue  esc back  F2 advanced  F3 music  F4 teams  F5 codex auth  ctrl+c quit", dim, false)
	}
	return "\n" + status
}

func (m Model) paint(value string, color lipgloss.TerminalColor, bold bool) string {
	if m.noColor {
		return lipgloss.NewStyle().Bold(bold).Render(value)
	}
	return lipgloss.NewStyle().Foreground(color).Bold(bold).Render(value)
}

func spectrum(width, phase int, ascii bool) string {
	glyphs := []rune("▁▂▃▄▅▆▇█")
	if ascii {
		glyphs = []rune("._-=+*#@")
	}
	count := min(max(8, width-1), 90)
	var b strings.Builder
	for i := 0; i < count; i++ {
		value := (i*i + phase*3 + i*phase) % len(glyphs)
		b.WriteRune(glyphs[value])
	}
	return b.String()
}

func chooseColor(condition bool, yes, no lipgloss.TerminalColor) lipgloss.TerminalColor {
	if condition {
		return yes
	}
	return no
}

func outcomeColor(value setup.Outcome) lipgloss.TerminalColor {
	switch value {
	case setup.OutcomeReady:
		return green
	case setup.OutcomeWarning:
		return amber
	default:
		return red
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
