package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/internal/store"
)

// tickInterval is how often the refresh command re-fetches a snapshot and
// reschedules itself.
const tickInterval = 2 * time.Second

// SnapshotMsg carries a freshly read store.Snapshot into Update. It is the
// only way new board state enters the Model.
type SnapshotMsg struct {
	Snap store.Snapshot
}

// TickMsg is the refresh trigger sent by tea.Tick. Update responds to it by
// returning a command that re-fetches a snapshot and schedules the next
// tick; Update itself never touches the store.
type TickMsg struct{}

// ErrMsg carries a fetch/refresh error into Update. It does not stop the
// refresh loop: the next tick still schedules another fetch attempt.
type ErrMsg struct {
	Err error
}

// Row is one issue's display-ready state: identifier/lane plus its latest
// run info, flattened out of store.IssueSnapshot so View doesn't need to
// know about sql.Null* field wrangling.
type Row struct {
	Identifier string
	LaneLabel  string
	Status     string
	Run        *store.Run
}

// Model is the bubbletea model for `clipse tui`. It holds only
// snapshot-derived display state (plus the last error, if any) — never a
// live store handle — so Update stays pure and unit-testable without a DB
// or TTY.
type Model struct {
	running  []Row
	blocked  []Row
	queued   []Row // ready + todo, in that order
	inFlight []Row // review + rework + merging + documentation, in that order

	tokensIn  int
	tokensOut int

	lastErr error

	// refreshCmd is injected (see WithRefreshCmd) rather than hardcoded, so
	// Update never imports internal/store's *Store directly: the command
	// layer (the `clipse tui` RunE) supplies the func that actually reads
	// the store, and Update just wires it into tea.Tick's schedule.
	refreshCmd func() tea.Msg
}

// Option configures a Model constructed via NewModel.
type Option func(*Model)

// WithRefreshCmd sets the func Update uses, on each TickMsg, to fetch the
// next snapshot. Its result is wrapped in a tea.Cmd; it should return a
// SnapshotMsg on success or an ErrMsg on failure. Tests can inject a fake
// to assert scheduling behavior without a DB; the real `clipse tui` command
// wires in one that calls store.ReadSnapshot.
func WithRefreshCmd(f func() tea.Msg) Option {
	return func(m *Model) { m.refreshCmd = f }
}

// NewModel builds an empty Model, ready to receive its first SnapshotMsg.
func NewModel(opts ...Option) Model {
	m := Model{}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// Running, Blocked, Queued, and InFlight expose the current display-ready
// rows for each dashboard section. Queued folds "ready" and "todo" issues
// together; InFlight folds every active downstream lane-entry column
// (review/rework/merging/documentation) together, labeled per-row by its
// own column (Row.Status) since — unlike the other three sections — it
// spans more than one board_status value.
func (m Model) Running() []Row  { return m.running }
func (m Model) Blocked() []Row  { return m.blocked }
func (m Model) Queued() []Row   { return m.queued }
func (m Model) InFlight() []Row { return m.inFlight }

// TotalTokensIn and TotalTokensOut sum LatestRun token counts across every
// issue in the snapshot, for the dashboard's header line.
func (m Model) TotalTokensIn() int  { return m.tokensIn }
func (m Model) TotalTokensOut() int { return m.tokensOut }

// Err returns the last refresh error, if any. A subsequent successful
// SnapshotMsg clears it.
func (m Model) Err() error { return m.lastErr }

// Init starts the refresh loop by scheduling the first tick.
func (m Model) Init() tea.Cmd {
	return scheduleTick()
}

// Update is the bubbletea state transition: pure and deterministic, no DB
// or I/O. SnapshotMsg folds new board state in; TickMsg returns a command
// that performs the actual fetch (via the injected refreshCmd) and
// reschedules the next tick; ErrMsg records a fetch failure; 'q'/ctrl+c
// quit.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case SnapshotMsg:
		m.fold(msg.Snap)
		m.lastErr = nil
		return m, nil

	case TickMsg:
		return m, tea.Batch(m.refresh(), scheduleTick())

	case ErrMsg:
		m.lastErr = msg.Err
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyRunes:
			if string(msg.Runes) == "q" {
				return m, tea.Quit
			}
		}
		return m, nil

	default:
		return m, nil
	}
}

// refresh returns the tea.Cmd that invokes the injected refreshCmd, if any.
// Kept as its own tea.Cmd (rather than called inline) so it composes with
// scheduleTick() via tea.Batch.
func (m Model) refresh() tea.Cmd {
	if m.refreshCmd == nil {
		return nil
	}
	return tea.Cmd(m.refreshCmd)
}

// scheduleTick returns a tea.Cmd that fires a TickMsg after tickInterval,
// driving the next refresh/reschedule cycle.
func scheduleTick() tea.Cmd {
	return tea.Tick(tickInterval, func(time.Time) tea.Msg {
		return TickMsg{}
	})
}

// fold recomputes the Model's grouped rows and token totals from snap. It
// is deterministic: same snapshot in, same grouping/totals out, regardless
// of the snapshot's underlying slice/map iteration order.
func (m *Model) fold(snap store.Snapshot) {
	m.running = m.running[:0]
	m.blocked = m.blocked[:0]
	m.queued = m.queued[:0]
	m.inFlight = m.inFlight[:0]
	m.tokensIn = 0
	m.tokensOut = 0

	for _, is := range sortedIssueSnapshots(snap.Issues) {
		row := Row{
			Identifier: is.Identifier,
			LaneLabel:  is.LaneLabel,
			Status:     is.BoardStatus,
			Run:        is.LatestRun,
		}
		if is.LatestRun != nil {
			m.tokensIn += is.LatestRun.TokensIn
			m.tokensOut += is.LatestRun.TokensOut
		}

		switch is.BoardStatus {
		case "running":
			m.running = append(m.running, row)
		case "blocked":
			m.blocked = append(m.blocked, row)
		case "ready", "todo":
			m.queued = append(m.queued, row)
		case "review", "rework", "merging", "documentation":
			// Active downstream lane-entry columns: a card here is either
			// currently claimed (a Reviewer/Git-operator/Scribe run in
			// flight) or waiting its turn to be claimed — either way it is
			// still "in play", not invisible the way an unhandled
			// board_status previously left it. "done" deliberately has no
			// case here (and this is why the switch stays an explicit list
			// rather than a catch-all default): it's terminal, with
			// nothing left to watch.
			m.inFlight = append(m.inFlight, row)
		}
	}
}
