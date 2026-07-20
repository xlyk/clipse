// Package configureui implements the Bubble Tea configuration wizard. The
// reducer owns only display/draft state; filesystem, network, subprocess, and
// write operations enter through typed tea.Cmd results.
package configureui

import (
	"context"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/setup"
)

type Services struct {
	Check         func(context.Context, config.Config) setup.Report
	Write         func(string, []byte, setup.WriteOptions) (setup.WriteResult, error)
	DiscoverTeams func(context.Context) ([]linear.Team, error)
}

type AudioController interface {
	Start(context.Context) error
	Stop() error
	Active() bool
}

type Options struct {
	Draft       setup.Draft
	OutputPath  string
	Advanced    bool
	NoColor     bool
	NoAnimation bool
	ASCII       bool
	Music       string
	Context     context.Context
	Services    Services
	Audio       AudioController
	Original    []byte
}

type Result struct {
	Canceled    bool
	WrittenPath string
	BackupPath  string
	BoardDir    string
	Report      setup.Report
	Err         error
}

type pageID int

const (
	pageInstance pageID = iota
	pageRepository
	pageLinear
	pageBackend
	pageModels
	pageSafety
	pageRuntime
	pageReview
	pageFinish
)

var pageNames = []string{
	"INSTANCE", "REPOSITORY", "LINEAR LINK", "BACKEND", "MODELS", "SAFETY", "RUNTIME", "REVIEW", "FINISH",
}

type Model struct {
	draft        setup.Draft
	outputPath   string
	fields       []field
	page         pageID
	cursor       int
	advanced     bool
	width        int
	height       int
	noColor      bool
	noAnimation  bool
	ascii        bool
	phase        int
	busy         bool
	status       string
	err          error
	report       setup.Report
	haveReport   bool
	raw          []byte
	original     []byte
	reviewTab    int
	reviewOffset int
	replace      bool
	teams        []linear.Team
	teamCursor   int
	ctx          context.Context
	services     Services
	audio        AudioController
	musicOn      bool
	autoMusic    bool
	result       Result
}

type tickMsg time.Time
type checkMsg struct{ report setup.Report }
type teamsMsg struct {
	teams []linear.Team
	err   error
}
type writeMsg struct {
	result       setup.WriteResult
	err          error
	needsReplace bool
}
type audioMsg struct {
	active bool
	err    error
}
type authMsg struct{ err error }

func NewModel(opts Options) Model {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	services := opts.Services
	if services.Check == nil {
		services.Check = func(ctx context.Context, cfg config.Config) setup.Report {
			return setup.RunChecks(ctx, cfg, setup.CheckOptions{})
		}
	}
	if services.Write == nil {
		services.Write = setup.WriteConfig
	}
	if services.DiscoverTeams == nil {
		services.DiscoverTeams = linear.DiscoverTeams
	}
	noColor := opts.NoColor || os.Getenv("NO_COLOR") != ""
	ascii := opts.ASCII || os.Getenv("TERM") == "dumb"
	m := Model{
		draft:       opts.Draft,
		outputPath:  opts.OutputPath,
		advanced:    opts.Advanced,
		width:       100,
		height:      30,
		noColor:     noColor,
		noAnimation: opts.NoAnimation,
		ascii:       ascii,
		ctx:         ctx,
		services:    services,
		audio:       opts.Audio,
		autoMusic:   opts.Music == "on",
		original:    append([]byte(nil), opts.Original...),
	}
	m.fields = newFields(opts.Draft, opts.OutputPath)
	m.syncFocus()
	return m
}

func (m Model) Init() tea.Cmd {
	var commands []tea.Cmd
	if !m.noAnimation {
		commands = append(commands, tickCmd())
	}
	if m.autoMusic && m.audio != nil {
		commands = append(commands, m.startAudioCmd())
	}
	return tea.Batch(commands...)
}

func tickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Result() Result { return m.result }

func (m Model) PageName() string { return pageNames[m.page] }

func (m Model) ConfigFieldKeys() []string {
	keys := make([]string, 0, len(m.fields))
	for _, field := range m.fields {
		if field.configKey {
			keys = append(keys, field.key)
		}
	}
	return keys
}
