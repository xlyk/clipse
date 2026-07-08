package tui

import (
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/internal/store"
)

// tickInterval is how often the refresh command re-fetches a snapshot and
// reschedules itself.
const tickInterval = 2 * time.Second

// SnapshotMsg carries a freshly read store.Snapshot into Update, plus a
// liveness reading (Live) the refresh command computes out-of-band (it tests
// the dispatcher singleton lock, which is I/O and so must not happen inside
// the pure Update). It is the only way new board state enters the Model.
type SnapshotMsg struct {
	Snap store.Snapshot
	// Live is true when a dispatcher is actively holding the board's
	// singleton lock at the moment the snapshot was read. Defaults to false
	// (unknown/none) for callers — e.g. tests — that don't supply it.
	Live bool
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

// viewMode selects which screen the dashboard renders: the stacked section
// list, a single issue's detail, or the kanban board. The help overlay is a
// separate boolean toggle layered over whichever mode is active.
type viewMode int

const (
	modeDashboard viewMode = iota
	modeDetail
	modeKanban
)

// Row is one issue's display-ready state: identifier/lane plus its latest
// run info and cumulative token usage, flattened out of store.IssueSnapshot so
// View doesn't need to know about sql.Null* field wrangling.
type Row struct {
	// ID is the issue's Linear UUID, retained (unlike the original
	// display-only Row) so selection can be tracked stably and so events /
	// dependencies referring to issues by id can be resolved back to a row.
	ID         string
	Identifier string
	LaneLabel  string
	Status     string

	// Deps is the raw JSON array of dependency issue-ids, carried so QUEUED
	// rows can render a "waiting on …" hint and the detail view can list
	// blockers without a second store read.
	Deps string

	Run *store.Run

	// TokensIn/TokensOut are cumulative across every run of this issue (not
	// just Run, the latest) — the honest per-card usage.
	TokensIn  int
	TokensOut int

	// Live is true iff the dispatcher currently holds a claim on this issue —
	// i.e. a worker is actively working it RIGHT NOW, in whatever lane. It is
	// keyed off the claim, not board_status, so it lights up for the
	// reviewer/git_operator agents (review/merging) just
	// as it does for the coder ("running"), and it goes dark for a card merely
	// parked in a downstream column waiting to be claimed. Live rows render
	// with the spinner + elapsed, whatever section they fall in.
	Live bool

	// ActiveLane is the bare lane of the run currently working this issue
	// (Run.Lane) when Live, else "". It differs from LaneLabel (the issue's
	// coder "home" label) once a card reaches a downstream lane — a review
	// card being reviewed has ActiveLane "reviewer" but LaneLabel "coder" — so
	// the row badge can name the agent that is actually working, not the one
	// that opened the issue.
	ActiveLane string

	// ReworkCount / RecoverAttempts / BlockedUntil / Priority / Unmirrored
	// surface the kernel's per-issue bookkeeping the TUI previously dropped
	// (P4): how many times the card bounced back to the coder, how many
	// transient-failure auto-retries it has burned, the unix time before
	// which it is invisible to every claim (its retry backoff window), its
	// Linear priority (the kernel's claim-order key), and whether a board
	// transition is still waiting to be mirrored to Linear (a pending
	// outbox write).
	ReworkCount     int
	RecoverAttempts int
	BlockedUntil    int64
	Priority        int
	Unmirrored      bool
}

// Model is the bubbletea model for `clipse tui`. It holds only
// snapshot-derived display state (plus view/selection state and the last
// error) — never a live store handle — so Update stays pure and unit-testable
// without a DB or TTY.
type Model struct {
	// active is every issue in play right now — the working columns
	// running/review/rework/merging folded into one section (P2): live-claim
	// rows first (a worker is on them at this moment), then unclaimed rows
	// waiting for their next pickup.
	active  []Row
	blocked []Row
	queued  []Row // ready + todo, in that order

	// ordered is every visible row flattened in section order
	// (active→blocked→queued), the list the selection cursor walks.
	ordered []Row
	// byStatus groups rows by exact board_status, the source for the kanban
	// columns (including terminal "done", which the stacked view omits).
	byStatus map[string][]Row
	// issuesByIdent maps a row identifier to its full IssueSnapshot, so the
	// detail view can reach run history / branch / deps without another read.
	issuesByIdent map[string]store.IssueSnapshot
	// identByID / statusByID resolve an issue UUID to its identifier and
	// board_status — used to turn dependency ids and event issue-ids into
	// something human-readable.
	identByID  map[string]string
	statusByID map[string]string
	// laneByRunID resolves a run id to its bare lane, built from every
	// issue's full run history — the activity feed uses it to badge each
	// event with the lane that produced it (P3).
	laneByRunID map[string]string

	tokensIn  int
	tokensOut int

	// counts is the board-wide status histogram (every board_status, incl.
	// terminal "done"), for the header's stat chips — not just the four
	// displayed sections.
	counts map[string]int
	// totalIssues / doneCount drive the header progress bar (done / total).
	totalIssues int
	doneCount   int
	// unmirroredCount is how many issues have a pending (unmirrored) Linear
	// outbox write — surfaced as an amber header chip when > 0.
	unmirroredCount int

	// recentEvents / lastEventAt back the activity feed and the "updated Ns
	// ago" liveness readout (age is computed in View, never in Update).
	recentEvents []store.Event
	lastEventAt  int64
	// live reflects the most recent SnapshotMsg.Live (dispatcher lock held).
	live bool

	// selected is the identifier of the highlighted row. Tracking by
	// identifier (not slice index) keeps the cursor pinned to the same issue
	// across a refresh that reorders or resizes the sections.
	selected string
	mode     viewMode
	showHelp bool

	// width/height track the terminal size (tea.WindowSizeMsg); frame drives
	// the running-row spinner animation, advanced by a fast spinner tick
	// independent of the slower snapshot refresh.
	width  int
	height int
	frame  int

	// bubbles widgets. keys/help render the keybinding overlay; progress is
	// the header completion bar (rendered statically via ViewAs, so Update
	// stays free of its animation cmds); the three viewports scroll the body
	// sections, the activity feed, and the detail pane.
	keys       keyMap
	help       help.Model
	progress   progress.Model
	bodyVp     viewport.Model
	activityVp viewport.Model
	detailVp   viewport.Model

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
	m := Model{
		mode:     modeDashboard,
		keys:     defaultKeyMap(),
		help:     help.New(),
		progress: progress.New(progress.WithGradient(progressGradientColors()), progress.WithoutPercentage()),
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// Active, Blocked, and Queued expose the current display-ready rows for each
// dashboard section. Active folds every working column
// (running/review/rework/merging) together — live-claim rows first, then
// unclaimed rows awaiting pickup — labeled per-row by its own column
// (Row.Status) since it spans more than one board_status value. Queued folds
// "ready" and "todo" issues together.
func (m Model) Active() []Row  { return m.active }
func (m Model) Blocked() []Row { return m.blocked }
func (m Model) Queued() []Row  { return m.queued }

// TotalTokensIn and TotalTokensOut sum LatestRun token counts across every
// issue in the snapshot, for the dashboard's header line.
func (m Model) TotalTokensIn() int  { return m.tokensIn }
func (m Model) TotalTokensOut() int { return m.tokensOut }

// Err returns the last refresh error, if any. A subsequent successful
// SnapshotMsg clears it.
func (m Model) Err() error { return m.lastErr }

// Selected returns the identifier of the currently highlighted row (empty
// when there are no rows). Exposed for tests asserting cursor navigation.
func (m Model) Selected() string { return m.selected }

// ViewMode returns the active screen as a stable string ("dashboard",
// "detail", or "kanban"). Exposed for tests asserting view-mode toggling.
func (m Model) ViewMode() string {
	switch m.mode {
	case modeDetail:
		return "detail"
	case modeKanban:
		return "kanban"
	default:
		return "dashboard"
	}
}

// HelpVisible reports whether the help overlay is toggled open. Exposed for
// tests asserting the '?' toggle.
func (m Model) HelpVisible() bool { return m.showHelp }

// spinnerInterval is how often the running-row spinner advances. It is much
// faster than tickInterval (the snapshot refresh) so the animation is smooth
// without hammering the store: a spinnerTickMsg only bumps a frame counter, it
// never reads the DB.
const spinnerInterval = 120 * time.Millisecond

// spinnerTickMsg advances the spinner animation frame. Distinct from TickMsg
// (which triggers a snapshot refresh) so animation and data refresh run at
// independent cadences.
type spinnerTickMsg struct{}

// Init starts the refresh loop and the faster spinner animation tick, and
// fires one immediate refresh so the dashboard populates without waiting a
// full tickInterval.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), scheduleTick(), scheduleSpinner())
}

// Update is the bubbletea state transition: pure and deterministic, no DB
// or wall-clock (all time.Now lives in View or the injected refresh cmd).
// SnapshotMsg folds new board state in; TickMsg returns a command that
// performs the actual fetch (via the injected refreshCmd) and reschedules the
// next tick; ErrMsg records a fetch failure; key messages drive
// navigation/mode/scroll; q/ctrl+c quit.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case SnapshotMsg:
		m.fold(msg.Snap)
		m.live = msg.Live
		m.lastErr = nil
		m.layout()
		return m, nil

	case TickMsg:
		return m, tea.Batch(m.refresh(), scheduleTick())

	case spinnerTickMsg:
		m.frame++
		return m, scheduleSpinner()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case ErrMsg:
		m.lastErr = msg.Err
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)

	default:
		return m, nil
	}
}

// handleMouse forwards a mouse (wheel) event to whichever viewport the pointer
// is over, so the panes scroll under the wheel: the detail pane in detail mode,
// and the pipeline vs. activity pane on the dashboard by pointer position. It
// is pure — viewport.Update on a wheel event only adjusts the scroll offset —
// so it upholds the "no I/O in Update" invariant.
func (m Model) handleMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	switch m.mode {
	case modeDetail:
		m.detailVp, _ = m.detailVp.Update(msg)
	case modeDashboard:
		if m.pointerOverActivity(msg.X, msg.Y) {
			m.activityVp, _ = m.activityVp.Update(msg)
		} else {
			m.bodyVp, _ = m.bodyVp.Update(msg)
		}
	}
	return m, nil
}

// pointerOverActivity reports whether y lands in the activity pane rather than
// the pipeline pane, to route a dashboard wheel-scroll. The panels stack, so
// the split is vertical: the activity band sits below the pipeline panel.
func (m Model) pointerOverActivity(_, y int) bool {
	d := m.dims()
	return y >= d.headerH+d.tabsH+d.pipeH
}

// handleKey applies a key press against the current view mode. It never
// performs I/O: it only mutates selection/mode/scroll state (plus the pure
// re-layout) and, for quit, returns tea.Quit.
func (m Model) handleKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, m.keys.Help):
		m.showHelp = !m.showHelp
		return m, nil

	case key.Matches(msg, m.keys.Back):
		// esc unwinds one layer: close help first, then leave a sub-view.
		switch {
		case m.showHelp:
			m.showHelp = false
		case m.mode != modeDashboard:
			m.mode = modeDashboard
		}
		return m, nil

	case key.Matches(msg, m.keys.Kanban):
		// tab/v flips between the stacked list and the kanban board; it is a
		// no-op inside the detail view (esc/enter own that transition).
		if m.mode == modeKanban {
			m.mode = modeDashboard
		} else if m.mode == modeDashboard {
			m.mode = modeKanban
		}
		return m, nil

	case key.Matches(msg, m.keys.Enter):
		if m.mode != modeDetail && m.selected != "" {
			m.mode = modeDetail
			m.detailVp.SetYOffset(0)
			m.layout()
		}
		return m, nil

	case key.Matches(msg, m.keys.Up):
		if m.mode == modeDetail {
			m.detailVp.ScrollUp(1)
		} else {
			m.moveSelection(-1)
			m.layout()
			m.ensureSelectionVisible()
		}
		return m, nil

	case key.Matches(msg, m.keys.Down):
		if m.mode == modeDetail {
			m.detailVp.ScrollDown(1)
		} else {
			m.moveSelection(1)
			m.layout()
			m.ensureSelectionVisible()
		}
		return m, nil

	case key.Matches(msg, m.keys.ScrollUp):
		m.scrollActive(-1)
		return m, nil

	case key.Matches(msg, m.keys.ScrollDown):
		m.scrollActive(1)
		return m, nil
	}
	return m, nil
}

// scrollActive scrolls whichever viewport the active mode owns by half a
// page in dir (-1 up, +1 down). The viewports clamp to their own content, so
// an over-scroll is a harmless no-op.
func (m *Model) scrollActive(dir int) {
	vp := &m.bodyVp
	if m.mode == modeDetail {
		vp = &m.detailVp
	}
	if dir < 0 {
		vp.HalfViewUp()
	} else {
		vp.HalfViewDown()
	}
}

// moveSelection moves the cursor by delta over the flattened ordered rows,
// clamped at both ends (no wraparound), tracking the target by identifier so
// the selection survives the next refresh's re-fold.
func (m *Model) moveSelection(delta int) {
	if len(m.ordered) == 0 {
		m.selected = ""
		return
	}
	idx := m.selectedIndex()
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(m.ordered) {
		idx = len(m.ordered) - 1
	}
	m.selected = m.ordered[idx].Identifier
}

// selectedIndex returns the position of the selected identifier within
// ordered, or 0 when the selection is unset or no longer present.
func (m Model) selectedIndex() int {
	for i, r := range m.ordered {
		if r.Identifier == m.selected {
			return i
		}
	}
	return 0
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

// scheduleSpinner returns a tea.Cmd that fires a spinnerTickMsg after
// spinnerInterval, driving the running-row spinner animation.
func scheduleSpinner() tea.Cmd {
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// fold recomputes the Model's grouped rows, lookup maps, and totals from
// snap. It is deterministic: same snapshot in, same grouping/totals out,
// regardless of the snapshot's underlying slice/map iteration order.
func (m *Model) fold(snap store.Snapshot) {
	m.active = m.active[:0]
	m.blocked = m.blocked[:0]
	m.queued = m.queued[:0]
	m.ordered = m.ordered[:0]

	// Board-wide cumulative token spend comes straight from the snapshot's
	// SUM over every run (see store.ReadSnapshot); it is NOT re-derived from
	// LatestRun here, which would drop every non-latest lane's usage.
	m.tokensIn = snap.TotalTokensIn
	m.tokensOut = snap.TotalTokensOut
	m.counts = snap.CountsByStatus
	m.totalIssues = len(snap.Issues)
	m.doneCount = snap.CountsByStatus["done"]
	m.unmirroredCount = snap.UnmirroredCount
	m.recentEvents = snap.RecentEvents
	m.lastEventAt = snap.LastEventAt

	m.byStatus = make(map[string][]Row, len(snap.CountsByStatus))
	m.issuesByIdent = make(map[string]store.IssueSnapshot, len(snap.Issues))
	m.identByID = make(map[string]string, len(snap.Issues))
	m.statusByID = make(map[string]string, len(snap.Issues))
	m.laneByRunID = make(map[string]string)

	for _, is := range sortedIssueSnapshots(snap.Issues) {
		// A held claim means a worker is actively on this card now, in whatever
		// lane — the honest "an agent is working" signal, independent of the
		// board_status bucket the row lands in below.
		live := is.ClaimLock.Valid && is.LatestRun != nil
		activeLane := ""
		if live {
			activeLane = is.LatestRun.Lane
		}
		row := Row{
			ID:              is.ID,
			Identifier:      is.Identifier,
			LaneLabel:       is.LaneLabel,
			Status:          is.BoardStatus,
			Deps:            is.Deps,
			Run:             is.LatestRun,
			TokensIn:        is.TokensInTotal,
			TokensOut:       is.TokensOutTotal,
			Live:            live,
			ActiveLane:      activeLane,
			ReworkCount:     is.ReworkCount,
			RecoverAttempts: is.RecoverAttempts,
			BlockedUntil:    is.BlockedUntil,
			Priority:        is.Priority,
			Unmirrored:      is.Unmirrored,
		}

		m.byStatus[is.BoardStatus] = append(m.byStatus[is.BoardStatus], row)
		m.issuesByIdent[is.Identifier] = is
		m.identByID[is.ID] = is.Identifier
		m.statusByID[is.ID] = is.BoardStatus
		for _, r := range is.Runs {
			m.laneByRunID[r.RunID] = r.Lane
		}

		switch is.BoardStatus {
		case "running", "review", "rework", "merging":
			// The working columns fold into one ACTIVE section (P2): a card
			// here is either currently claimed (a worker in some lane is on
			// it right now) or waiting its turn to be claimed — either way it
			// is in play. "done" deliberately has no case here (and this is
			// why the switch stays an explicit list rather than a catch-all
			// default): it's terminal, with nothing left to watch.
			m.active = append(m.active, row)
		case "blocked":
			m.blocked = append(m.blocked, row)
		case "ready", "todo":
			m.queued = append(m.queued, row)
		}
	}

	// Live rows lead the ACTIVE section (spinner/lane/elapsed), unclaimed
	// rows trail dim; the stable sort preserves identifier order within each
	// half (rows arrive identifier-sorted from sortedIssueSnapshots).
	sort.SliceStable(m.active, func(i, j int) bool {
		return m.active[i].Live && !m.active[j].Live
	})

	// QUEUED sorts by claim priority — the order the kernel will actually
	// take them (store.selectClaimCandidate's ORDER BY): Linear priority 0
	// means "none" and sorts last, 1 (urgent) … 4 (low) ascending, ties by
	// identifier (rows arrive identifier-sorted; the stable sort keeps that).
	sort.SliceStable(m.queued, func(i, j int) bool {
		return queuedRank(m.queued[i].Priority) < queuedRank(m.queued[j].Priority)
	})

	// ordered is the section-order concatenation the cursor walks; it must
	// match View's stacked render order (active, blocked, queued).
	m.ordered = append(m.ordered, m.active...)
	m.ordered = append(m.ordered, m.blocked...)
	m.ordered = append(m.ordered, m.queued...)

	m.reconcileSelection()
}

// reconcileSelection keeps m.selected pointing at a still-present row: it is
// left untouched when the selected identifier survives the refresh, and
// otherwise snaps to the first ordered row (or clears when nothing is
// visible).
func (m *Model) reconcileSelection() {
	if len(m.ordered) == 0 {
		m.selected = ""
		return
	}
	for _, r := range m.ordered {
		if r.Identifier == m.selected {
			return
		}
	}
	m.selected = m.ordered[0].Identifier
}
