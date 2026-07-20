package configureui

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/setup"
	setupaudio "github.com/xlyk/clipse/internal/setup/audio"
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
	title := "[CLIPSE] // CONFIG OVERDRIVE"
	if !m.ascii {
		title = "◢◤ CLIPSE // CONFIG OVERDRIVE ◥◣"
	}
	music := "MUSIC OFF"
	if m.musicOn {
		music = fmt.Sprintf("♫ %d BPM", setupaudio.SoundtrackBPM)
		if m.ascii {
			music = fmt.Sprintf("MUSIC %d BPM", setupaudio.SoundtrackBPM)
		}
	}
	mode := "QUICK"
	if m.advanced {
		mode = "DEEP"
	}
	left := m.paint(title, cyan, true)
	right := m.paint(progress+" // "+mode+" // "+music, magenta, true)
	line := m.chromeRow(left, right)
	if m.width >= 96 && m.height >= 28 {
		line = m.renderLogo() + "\n" + line
	}
	if !m.noAnimation && m.width >= 76 {
		pulses := []string{"◈", "✦", "◆", "✧"}
		if m.ascii {
			pulses = []string{"*", "+", "#", "+"}
		}
		bus := fmt.Sprintf("NEON BUS %s UPLINK", pulses[m.phase%len(pulses)])
		tail := fmt.Sprintf(" RX:%02X", (m.phase*17+0xC1)%256)
		available := max(8, m.width-lipgloss.Width(bus)-lipgloss.Width(tail)-2)
		line += "\n" + m.paint(bus, magenta, true) + " " + m.renderSpectrum(available) + m.paint(tail, green, true)
	}
	return line
}

func (m Model) renderLogo() string {
	if m.ascii {
		logoWidth := min(72, max(48, m.width-2))
		top := "+" + strings.Repeat("=", logoWidth-2) + "+"
		line := func(value string) string {
			padding := max(0, logoWidth-4-len(value))
			return "| " + value + strings.Repeat(" ", padding) + " |"
		}
		return strings.Join([]string{
			m.paint(top, magenta, true),
			m.paint(line("CLIPSE NETWORK // CYBER CONFIGURATION UNIT"), cyan, true),
			m.paint(line("HARD-SYNC CONFIG CRACKTRO // 0xC11P53"), amber, true),
			m.paint(top, magenta, true),
		}, "\n")
	}
	logo := []string{
		"╔═╗╦  ╦╔═╗╔═╗╔═╗   CLIPSE NETWORK // N E O N   N O D E",
		"║  ║  ║╠═╝╚═╗║╣    CYBER CONFIGURATION UNIT // 0xC11P53",
		"╚═╝╩═╝╩╩  ╚═╝╚═╝   HARD-SYNC CONFIG CRACKTRO // STAY CUTE",
	}
	colors := []lipgloss.TerminalColor{cyan, magenta, green}
	for i := range logo {
		logo[i] = m.paint(logo[i], colors[(i+m.phase/3)%len(colors)], true)
	}
	return strings.Join(logo, "\n")
}

func (m Model) chromeRow(left, right string) string {
	if lipgloss.Width(left)+lipgloss.Width(right)+2 > m.width {
		return left + "\n" + right
	}
	space := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", space) + right
}

func (m Model) renderBody() string {
	content := m.renderPage()
	if m.width < 76 {
		return "\n" + content
	}
	railWidth := 24
	rail := m.renderRail(railWidth)
	contentWidth := max(30, m.width-railWidth-4)
	panelStyle := lipgloss.NewStyle().Width(contentWidth).PaddingLeft(2).BorderStyle(lipgloss.ThickBorder()).BorderLeft(true)
	if !m.noColor {
		panelStyle = panelStyle.BorderForeground(magenta)
	}
	if m.ascii {
		panelStyle = lipgloss.NewStyle().Width(contentWidth).PaddingLeft(3)
	}
	panel := panelStyle.Render(content)
	return "\n" + lipgloss.JoinHorizontal(lipgloss.Top, rail, " ", panel)
}

func (m Model) renderRail(width int) string {
	percent := (int(m.page) + 1) * 100 / len(pageNames)
	lines := make([]string, 0, len(pageNames)+7)
	rule := "─────────────"
	if m.ascii {
		rule = "-------------"
	}
	lines = append(lines,
		m.paint("LOAD SEQUENCE", magenta, true),
		m.paint(fmt.Sprintf("PROTOCOL // %03d%%", percent), cyan, true),
		m.renderProgress(18, int(m.page)+1, len(pageNames)),
		m.paint(rule, border, false),
	)
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
			active := []string{"◆", "✦", "◈", "✧"}
			marker, color = active[(m.phase/2)%len(active)], cyan
			if m.ascii {
				marker = []string{">", ">", "*", ">"}[(m.phase/2)%4]
			}
		}
		lines = append(lines, m.paint(fmt.Sprintf("%s %02d %-12s", marker, i+1, name), color, pageID(i) == m.page))
	}
	lines = append(lines, "", m.paint(m.nekoNode(), magenta, true), m.paint("root@clipse:~# armed", dim, false))
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

func (m Model) renderProgress(width, current, total int) string {
	filled := width * current / max(1, total)
	if m.ascii {
		return m.paint("["+strings.Repeat("#", filled)+strings.Repeat(".", width-filled)+"]", green, true)
	}
	return m.paint(strings.Repeat("█", filled), green, true) + m.paint(strings.Repeat("░", width-filled), border, false)
}

func (m Model) nekoNode() string {
	if m.ascii {
		return "[>_<] N3K0 NODE"
	}
	return "ฅ^•ﻌ•^ฅ N3K0 NODE"
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
	step := fmt.Sprintf("[ %02d // %s ]", int(m.page)+1, title)
	ruleWidth := min(62, max(12, m.width-31))
	transmission := strings.Join(wrapWords(m.pageTransmission(), m.contentTextWidth()), "\n")
	return m.paint(step, cyan, true) + "\n" + m.renderRule(ruleWidth) + "\n" + m.paint(transmission, dim, false) + "\n\n" + body
}

func (m Model) pageTransmission() string {
	unicode := []string{
		"give the daemon a callsign. make it yours. ฅ^•ﻌ•^ฅ",
		"jack into the repo // point every branch toward home",
		"establish tracker uplink // choose who owns reality",
		"spin the remote shell habitat // Daytona recommended",
		"slot the machine minds // coder + docs + reviewer",
		"set the blast radius // cute operators verify boundaries",
		"tune the kernel clocks // deterministic, relentless, yours",
		"inspect every byte before we burn it to disk",
		"configuration sealed // release the cyber gremlin",
	}
	ascii := []string{
		"give the daemon a callsign. make it yours. [=^.^=]",
		"jack into the repo // point every branch toward home",
		"establish tracker uplink // choose who owns reality",
		"spin the remote shell habitat // Daytona recommended",
		"slot the machine minds // coder + docs + reviewer",
		"set the blast radius // cute operators verify boundaries",
		"tune the kernel clocks // deterministic, relentless, yours",
		"inspect every byte before we burn it to disk",
		"configuration sealed // release the cyber gremlin",
	}
	if m.ascii {
		return ascii[m.page]
	}
	return unicode[m.page]
}

func (m Model) renderFields() string {
	visible := m.visibleFields()
	if len(visible) == 0 {
		return "No fields on this page."
	}
	start, end := 0, len(visible)
	limit := max(3, m.height-14)
	if end-start > limit {
		start = max(0, m.cursor-limit/2)
		end = min(len(visible), start+limit)
		start = max(0, end-limit)
	}
	lines := make([]string, 0, (end-start)*3)
	for position := start; position < end; position++ {
		field := m.fields[visible[position]]
		selected := position == m.cursor
		label := "  " + field.label
		if field.advanced {
			label += "  <ADV>"
		}
		value := field.input.View()
		if len(field.options) > 0 {
			if m.ascii {
				value = "< " + strings.ToUpper(field.input.Value()) + " >"
			} else {
				value = "◀ " + strings.ToUpper(field.input.Value()) + " ▶"
			}
		} else if !selected {
			value = ellipsizeMiddle(field.input.Value(), max(10, m.contentTextWidth()-4), m.ascii)
		}
		if selected {
			node := fmt.Sprintf(" ACTIVE NODE %02d ", position+1)
			top, mid, bottom := "╭─[", "│ ", "╰─"
			if m.ascii {
				top, mid, bottom = "+-[", "| ", "+-"
			}
			pulse := "◆"
			if m.ascii {
				pulse = "*"
			} else if !m.noAnimation {
				pulse = []string{"◆", "✦", "◈", "✧"}[m.phase%4]
			}
			lines = append(lines,
				m.paint(top+node+"] "+pulse+" "+strings.TrimSpace(label), cyan, true),
				m.paint(mid+value, magenta, true),
			)
			helpLines := wrapWords(field.help, max(12, m.contentTextWidth()-3))
			for i, help := range helpLines {
				prefix := mid
				if i == len(helpLines)-1 {
					prefix = bottom + " "
				}
				lines = append(lines, m.paint(prefix+help, dim, false))
			}
		} else {
			lines = append(lines,
				m.paint(label, chooseColor(selected, cyan, dim), selected),
				"    "+value,
			)
		}
	}
	if len(m.teams) > 0 {
		return m.renderTeamPicker()
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderTeamPicker() string {
	lines := []string{m.paint("[ TRACKER UPLINK // SELECT TEAM ]", magenta, true), m.paint("credential is local // discovery is read-only", dim, false), ""}
	for i, team := range m.teams {
		marker := "  "
		if i == m.teamCursor {
			marker = "* "
			if !m.ascii {
				marker = "◆ "
			}
		}
		entry := fmt.Sprintf("%s%-8s %s", marker, team.Key, team.Name)
		lines = append(lines, m.paint(entry, chooseColor(i == m.teamCursor, cyan, dim), i == m.teamCursor))
	}
	help := "up/down select  enter jack in  esc ghost"
	if !m.ascii {
		help = "↑↓ select  enter jack in  esc ghost"
	}
	lines = append(lines, "", m.paint(help, dim, false))
	return strings.Join(lines, "\n")
}

func (m Model) renderReview() string {
	tabs := []string{"READINESS", "YAML"}
	if len(m.original) > 0 {
		tabs = append(tabs, "DIFF")
	}
	chips := make([]string, 0, len(tabs))
	for i, tab := range tabs {
		if i == m.reviewTab {
			marker := "*"
			if !m.ascii {
				marker = "◆"
			}
			chips = append(chips, m.paint("["+marker+" "+tab+" // ACTIVE]", magenta, true))
		} else {
			chips = append(chips, m.paint("[  "+tab+"  ]", dim, false))
		}
	}
	tabLine := strings.Join(chips, " ") + "\n\n"
	if m.reviewTab == 1 {
		return tabLine + m.renderDocument(string(m.raw))
	}
	if m.reviewTab == 2 && len(m.original) > 0 {
		return tabLine + m.renderDocument(simpleDiff(redactDocument(string(m.original)), string(m.raw)))
	}
	if m.busy {
		dots := "..."
		if !m.ascii {
			dots = "…"
		}
		return tabLine + m.paint("SCANNING HOST + REMOTES"+dots, magenta, true) + "\n" + m.renderSpectrum(min(48, max(8, m.width-35))) + "\n\n" + m.paint("spectral scan only // zero resources created", dim, false)
	}
	if !m.haveReport {
		return tabLine + m.paint("READINESS ORACLE // awaiting scan", amber, true)
	}
	outcome := strings.ToUpper(string(m.report.Outcome))
	lines := []string{
		m.paint("╭─ OUTCOME // "+outcome, outcomeColor(m.report.Outcome), true),
		m.paint("╰─ host + tracker + sandbox + model", dim, false),
		"",
	}
	if m.ascii {
		lines[0] = m.paint("+- OUTCOME // "+outcome, outcomeColor(m.report.Outcome), true)
		lines[1] = m.paint("+- host + tracker + sandbox + model", dim, false)
	}
	for _, result := range m.report.Results {
		glyph, icon, color := "PASS", "+", green
		if result.Severity == setup.SeverityWarning {
			glyph, icon, color = "WARN", "!", amber
		} else if result.Severity == setup.SeverityBlocked {
			glyph, icon, color = "BLOCK", "x", red
		}
		if !m.ascii {
			if result.Severity == setup.SeverityPass {
				icon = "✓"
			} else if result.Severity == setup.SeverityWarning {
				icon = "⚠"
			} else {
				icon = "✕"
			}
		}
		lines = append(lines, m.paint(fmt.Sprintf("[%s %s]", icon, glyph), color, true)+" "+result.Summary)
		if result.Detail != "" {
			lines = append(lines, "           "+m.paint(result.Detail, dim, false))
		}
	}
	lines = append(lines, "", m.paint("ENTER/W burn config  R rescan  TAB jack view  ESC edit", cyan, true))
	return tabLine + strings.Join(lines, "\n")
}

func (m Model) renderDocument(document string) string {
	lines := strings.Split(document, "\n")
	limit := max(4, m.height-12)
	maxOffset := max(0, len(lines)-limit)
	offset := min(m.reviewOffset, maxOffset)
	end := min(len(lines), offset+limit)
	visibleLines := make([]string, 0, end-offset)
	for _, line := range lines[offset:end] {
		visibleLines = append(visibleLines, m.paintDocumentLine(line))
	}
	position := fmt.Sprintf("lines %d-%d/%d  up/down scroll  TAB switch", offset+1, end, len(lines))
	if !m.ascii {
		position = fmt.Sprintf("lines %d-%d/%d  ↑↓ scroll  TAB switch", offset+1, end, len(lines))
	}
	return strings.Join(visibleLines, "\n") + "\n\n" + m.paint(position, dim, false)
}

func (m Model) paintDocumentLine(line string) string {
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, "+ "):
		return m.paint(line, green, false)
	case strings.HasPrefix(line, "- "):
		return m.paint(line, red, false)
	case strings.HasPrefix(trimmed, "#"):
		return m.paint(line, dim, false)
	case strings.Contains(trimmed, ":"):
		return m.paint(line, cyan, false)
	default:
		return line
	}
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
	seal := "[>_<]  CONFIGURATION LOCKED // CYBER GREMLIN DEPLOYED"
	if !m.ascii {
		seal = "ฅ^•ﻌ•^ฅ  CONFIGURATION LOCKED // CYBER GREMLIN DEPLOYED"
	}
	lines := []string{
		m.paint(seal, green, true),
		m.renderRule(min(58, max(12, m.width-30))),
		"",
		m.paint("CONFIG", cyan, true) + "  " + m.result.WrittenPath,
		m.paint("BOARD ", cyan, true) + "  " + m.draft.Config.BoardDir,
	}
	if m.result.BackupPath != "" {
		lines = append(lines, m.paint("BACKUP", amber, true)+"  "+m.result.BackupPath)
	}
	lines = append(lines, "", m.paint("PRESS ENTER // drop back to shellspace", magenta, true))
	return strings.Join(lines, "\n")
}

func (m Model) renderFooter() string {
	status := m.status
	if m.err != nil {
		status = m.paint("ERROR  "+m.err.Error(), red, true)
	}
	if m.width < 76 {
		if status == "" {
			keys := "up/down navigate // enter next // esc back // ctrl+c eject\nF2 deep // F3 audio"
			if !m.ascii {
				keys = "↑↓ navigate // enter next // esc back // ctrl+c eject\nF2 deep // F3 audio"
			}
			status = m.paint(keys, dim, false)
		} else {
			status = strings.Join(wrapWords(status, max(16, m.width-2)), "\n")
		}
		return "\n" + status
	}
	if status == "" {
		status = "SYSTEM ARMED // CONFIG CHANNEL OPEN // NO SECRETS HIT DISK // stay weird"
	}
	keys := "[UP/DN] NAV  [ENTER] NEXT  [ESC] BACK  [F2] DEEP  [F3] AUDIO  [F4] LINEAR  [F5] AUTH  [CTRL+C] EJECT"
	if !m.ascii {
		keys = "[↑↓] NAV  [ENTER] NEXT  [ESC] BACK  [F2] DEEP  [F3] AUDIO  [F4] LINEAR  [F5] AUTH  [CTRL+C] EJECT"
	}
	if m.width < 108 {
		keys = "[UP/DN] NAV  [ENTER] NEXT  [ESC] BACK  [F2] DEEP  [F3] AUDIO  [CTRL+C] EJECT"
		if !m.ascii {
			keys = "[↑↓] NAV  [ENTER] NEXT  [ESC] BACK  [F2] DEEP  [F3] AUDIO  [CTRL+C] EJECT"
		}
	}
	pulse := "HYPERDRIVE [--] // "
	if !m.noAnimation && m.phase%4 == 0 {
		pulse = "HYPERDRIVE [##] // "
		if !m.ascii {
			pulse = "HYPERDRIVE ▰▰ // "
		}
	}
	return "\n" + m.renderRule(min(90, m.width-1)) + "\n" + m.paint(keys, cyan, true) + "\n" + m.paint(pulse, magenta, true) + status
}

func (m Model) paint(value string, color lipgloss.TerminalColor, bold bool) string {
	if m.noColor {
		return lipgloss.NewStyle().Bold(bold).Render(value)
	}
	return lipgloss.NewStyle().Foreground(color).Bold(bold).Render(value)
}

func (m Model) contentTextWidth() int {
	if m.width < 76 {
		return max(16, m.width-2)
	}
	return max(20, m.width-32)
}

func wrapWords(value string, width int) []string {
	if width <= 0 || len(value) <= width {
		return []string{value}
	}
	words := strings.Fields(value)
	if len(words) == 0 {
		return []string{""}
	}
	lines := []string{words[0]}
	for _, word := range words[1:] {
		last := len(lines) - 1
		if len(lines[last])+1+len(word) <= width {
			lines[last] += " " + word
			continue
		}
		lines = append(lines, word)
	}
	return lines
}

func ellipsizeMiddle(value string, width int, ascii bool) string {
	runes := []rune(value)
	if width <= 0 || len(runes) <= width {
		return value
	}
	marker := "…"
	markerWidth := 1
	if ascii {
		marker = "..."
		markerWidth = 3
	}
	if width <= markerWidth+2 {
		return string(runes[:width])
	}
	keep := width - markerWidth
	left := keep / 2
	right := keep - left
	return string(runes[:left]) + marker + string(runes[len(runes)-right:])
}

func spectrum(width, phase int, ascii bool) string {
	glyphs := []rune("▁▂▃▄▅▆▇█")
	if ascii {
		glyphs = []rune("._-=+*#@")
	}
	count := min(max(8, width-1), 90)
	var b strings.Builder
	for i := 0; i < count; i++ {
		wave := int(3 * math.Abs(math.Sin(float64(i+phase)*0.31)))
		value := (i*i + phase*5 + i*phase + wave) % len(glyphs)
		b.WriteRune(glyphs[value])
	}
	return b.String()
}

func (m Model) renderSpectrum(width int) string {
	value := spectrum(width, m.phase, m.ascii)
	if m.noColor {
		return value
	}
	colors := []lipgloss.TerminalColor{cyan, magenta, green, amber, magenta}
	var b strings.Builder
	for i, glyph := range value {
		b.WriteString(m.paint(string(glyph), colors[(i+m.phase)%len(colors)], i%7 == 0))
	}
	return b.String()
}

func (m Model) renderRule(width int) string {
	if width <= 0 {
		return ""
	}
	segments := []string{"═", "━", "─", "━"}
	if m.ascii {
		segments = []string{"=", "-", "=", "#"}
	}
	var b strings.Builder
	for i := 0; i < width; i++ {
		glyph := segments[(i+m.phase/2)%len(segments)]
		if m.noColor {
			b.WriteString(glyph)
			continue
		}
		color := []lipgloss.TerminalColor{cyan, magenta, green, magenta}[(i/6+m.phase)%4]
		b.WriteString(m.paint(glyph, color, i%13 == 0))
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
