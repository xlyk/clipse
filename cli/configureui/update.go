package configureui

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/internal/setup"
)

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		m.phase++
		if m.noAnimation || m.page == pageFinish {
			return m, nil
		}
		return m, tickCmd()
	case checkMsg:
		m.busy = false
		m.err = nil
		m.report = msg.report
		m.haveReport = true
		m.status = "Readiness scan complete: " + string(msg.report.Outcome)
		return m, nil
	case teamsMsg:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Linear team discovery failed"
			return m, nil
		}
		m.teams = msg.teams
		m.teamCursor = 0
		if len(m.teams) == 0 {
			m.status = "No Linear teams are visible to this credential"
		} else if len(m.teams) == 1 {
			m.chooseTeam(0)
			m.teams = nil
		}
		return m, nil
	case writeMsg:
		m.busy = false
		if msg.needsReplace {
			m.replace = true
			m.status = "File exists. Press y to back it up and replace it, or Esc to cancel."
			return m, nil
		}
		if msg.err != nil {
			m.err = msg.err
			m.result.Err = msg.err
			m.status = "Write failed"
			return m, nil
		}
		m.result.WrittenPath = msg.result.Path
		m.result.BackupPath = msg.result.BackupPath
		m.result.BoardDir = m.draft.Config.BoardDir
		m.result.Report = m.report
		m.result.Err = nil
		m.err = nil
		m.page = pageFinish
		m.status = "Configuration written"
		return m, nil
	case audioMsg:
		if msg.err != nil {
			m.musicOn = false
			m.status = "Soundtrack unavailable: " + msg.err.Error()
			return m, nil
		}
		m.musicOn = msg.active
		if msg.active {
			m.status = "Soundtrack online — 125 BPM"
		} else {
			m.status = "Soundtrack muted"
		}
		return m, nil
	case authMsg:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Codex authentication command failed"
		} else {
			m.err = nil
			m.status = "Authentication command finished; readiness will recheck on Review"
		}
		return m, nil
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if key.Type == tea.KeyCtrlC {
		m.result.Canceled = true
		return m, m.quitCmd()
	}
	if key.String() == "f3" || ((m.page == pageReview || m.page == pageFinish) && strings.ToLower(key.String()) == "m") {
		return m, m.toggleAudioCmd()
	}

	if len(m.teams) > 0 {
		return m.updateTeamPicker(key)
	}
	if m.replace {
		switch strings.ToLower(key.String()) {
		case "y":
			m.replace = false
			m.busy = true
			return m, m.writeCmd(true)
		case "esc", "n":
			m.replace = false
			m.status = "Existing file left untouched"
		}
		return m, nil
	}
	if m.page == pageFinish {
		if key.String() == "enter" || key.String() == "q" || key.String() == "esc" {
			return m, m.quitCmd()
		}
		return m, nil
	}
	if m.page == pageReview {
		return m.updateReview(key)
	}

	switch key.String() {
	case "esc":
		if m.page > pageInstance {
			m.page--
			m.cursor = 0
			m.err = nil
			m.status = ""
			m.syncFocus()
			return m, nil
		}
		m.result.Canceled = true
		return m, m.quitCmd()
	case "up", "shift+tab":
		if m.cursor > 0 {
			m.cursor--
			m.syncFocus()
		}
		return m, nil
	case "down", "tab":
		visible := m.visibleFields()
		if m.cursor+1 < len(visible) {
			m.cursor++
			m.syncFocus()
		}
		return m, nil
	case "f2":
		m.advanced = !m.advanced
		m.cursor = 0
		m.syncFocus()
		return m, nil
	case "f4":
		if m.page == pageLinear && !m.busy {
			m.busy = true
			m.status = "Discovering Linear teams…"
			return m, m.discoverTeamsCmd()
		}
	case "f5":
		if m.page == pageModels && !m.busy {
			command, err := m.codexAuthCommand()
			if err != nil {
				m.err = err
				m.status = "Codex authentication is unavailable"
				return m, nil
			}
			m.busy = true
			m.status = "Opening dcode; run /auth and choose openai_codex"
			return m, tea.ExecProcess(command, func(err error) tea.Msg { return authMsg{err: err} })
		}
	case "enter":
		if err := m.applyFields(); err != nil {
			m.err = err
			m.status = "Fix the highlighted input before continuing"
			return m, nil
		}
		visible := m.visibleFields()
		if m.cursor+1 < len(visible) {
			m.cursor++
			m.syncFocus()
			return m, nil
		}
		m.page++
		m.cursor = 0
		m.err = nil
		m.status = ""
		if m.page == pageReview {
			raw, err := setup.Render(m.draft.Config)
			if err != nil {
				m.err = err
				m.status = "Configuration is not valid yet"
				return m, nil
			}
			m.raw = raw
			m.busy = true
			m.status = "Running read-only readiness checks…"
			return m, m.checkCmd()
		}
		m.syncFocus()
		return m, nil
	}

	visible := m.visibleFields()
	if len(visible) == 0 {
		return m, nil
	}
	index := visible[m.cursor]
	if len(m.fields[index].options) > 0 && (key.String() == "left" || key.String() == "right") {
		m.cycleOption(index, key.String() == "right")
		return m, nil
	}
	var cmd tea.Cmd
	m.fields[index].input, cmd = m.fields[index].input.Update(key)
	m.haveReport = false
	return m, cmd
}

func (m Model) startAudioCmd() tea.Cmd {
	return func() tea.Msg {
		if m.audio == nil {
			return audioMsg{err: errors.New("no audio controller configured")}
		}
		err := m.audio.Start(m.ctx)
		return audioMsg{active: err == nil, err: err}
	}
}

func (m Model) toggleAudioCmd() tea.Cmd {
	return func() tea.Msg {
		if m.audio == nil {
			return audioMsg{err: errors.New("no supported audio controller")}
		}
		if m.audio.Active() {
			err := m.audio.Stop()
			return audioMsg{active: false, err: err}
		}
		err := m.audio.Start(m.ctx)
		return audioMsg{active: err == nil, err: err}
	}
}

func (m Model) quitCmd() tea.Cmd {
	return func() tea.Msg {
		if m.audio != nil {
			_ = m.audio.Stop()
		}
		return tea.Quit()
	}
}

func (m Model) updateReview(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch strings.ToLower(key.String()) {
	case "esc":
		m.page = pageRuntime
		m.cursor = 0
		m.syncFocus()
		return m, nil
	case "r":
		if !m.busy {
			m.busy = true
			m.status = "Running read-only readiness checks…"
			return m, m.checkCmd()
		}
	case "enter", "w":
		if !m.busy && m.haveReport {
			m.busy = true
			m.status = "Writing private config atomically…"
			return m, m.writeCmd(false)
		}
	case "tab", "right":
		tabs := 2
		if len(m.original) > 0 {
			tabs = 3
		}
		m.reviewTab = (m.reviewTab + 1) % tabs
		m.reviewOffset = 0
	case "left":
		tabs := 2
		if len(m.original) > 0 {
			tabs = 3
		}
		m.reviewTab = (m.reviewTab - 1 + tabs) % tabs
		m.reviewOffset = 0
	case "up":
		if m.reviewOffset > 0 {
			m.reviewOffset--
		}
	case "down":
		m.reviewOffset++
	}
	return m, nil
}

func (m Model) codexAuthCommand() (*exec.Cmd, error) {
	worker := m.draft.Config.Worker.Command
	project := ""
	for i := 0; i+1 < len(worker); i++ {
		if worker[i] == "--project" {
			project = worker[i+1]
			break
		}
	}
	if project == "" {
		return nil, errors.New("worker.command has no --project path for dcode")
	}
	return exec.Command("uv", "--project", project, "run", "dcode"), nil
}

func (m Model) updateTeamPicker(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up":
		if m.teamCursor > 0 {
			m.teamCursor--
		}
	case "down":
		if m.teamCursor+1 < len(m.teams) {
			m.teamCursor++
		}
	case "enter":
		m.chooseTeam(m.teamCursor)
		m.teams = nil
	case "esc":
		m.teams = nil
	}
	return m, nil
}

func (m *Model) chooseTeam(index int) {
	if index < 0 || index >= len(m.teams) {
		return
	}
	team := m.teams[index]
	for i := range m.fields {
		switch m.fields[i].key {
		case "team_key":
			m.fields[i].input.SetValue(team.Key)
		case "team_id":
			m.fields[i].input.SetValue(team.ID)
		}
	}
	m.status = "Selected Linear team " + team.Key + " — " + team.Name
}

func (m *Model) cycleOption(index int, forward bool) {
	field := &m.fields[index]
	current := field.input.Value()
	position := 0
	for i, option := range field.options {
		if option == current {
			position = i
			break
		}
	}
	if forward {
		position = (position + 1) % len(field.options)
	} else {
		position = (position - 1 + len(field.options)) % len(field.options)
	}
	field.input.SetValue(field.options[position])
}

func (m Model) checkCmd() tea.Cmd {
	return func() tea.Msg { return checkMsg{report: m.services.Check(m.ctx, m.draft.Config)} }
}

func (m Model) discoverTeamsCmd() tea.Cmd {
	return func() tea.Msg {
		teams, err := m.services.DiscoverTeams(m.ctx)
		return teamsMsg{teams: teams, err: err}
	}
}

func (m Model) writeCmd(replace bool) tea.Cmd {
	return func() tea.Msg {
		if !replace {
			if _, err := os.Stat(m.outputPath); err == nil {
				return writeMsg{needsReplace: true}
			} else if !errors.Is(err, os.ErrNotExist) {
				return writeMsg{err: err}
			}
		}
		result, err := m.services.Write(m.outputPath, m.raw, setup.WriteOptions{Replace: replace})
		return writeMsg{result: result, err: err}
	}
}
