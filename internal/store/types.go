package store

import "database/sql"

// SchedulingMode is the durable operator intent consulted atomically by
// every claim transaction.
type SchedulingMode string

const (
	SchedulingRunning SchedulingMode = "running"
	SchedulingPaused  SchedulingMode = "paused"
)

// ObservedMode is the active dispatcher's last acknowledged control state.
type ObservedMode string

const (
	ObservedRunning  ObservedMode = "running"
	ObservedPaused   ObservedMode = "paused"
	ObservedDraining ObservedMode = "draining"
)

// DispatcherControl mirrors the singleton board-scoped scheduling row.
// Empty strings and zero timestamps represent absent optional values.
type DispatcherControl struct {
	DesiredMode           SchedulingMode
	ObservedMode          ObservedMode
	RequestID             string
	RequestedAt           int64
	AcknowledgedAt        int64
	ActiveInstanceID      string
	ActivePID             int
	InstanceStartedAt     int64
	HeartbeatAt           int64
	DrainTargetInstanceID string
	DrainStrict           bool
	DrainedAt             int64
}

// DispatcherRegistration reports the state observed by a newly registered
// daemon and whether it repaired a drain targeting a prior dead instance.
type DispatcherRegistration struct {
	Control          DispatcherControl
	DrainInterrupted bool
}

// Issue mirrors a row in the issues table: a cache of Linear issue state
// plus dispatcher-owned claim fields. Deps is a JSON array (of issue ids)
// encoded as TEXT.
type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	LaneLabel   string
	BoardStatus string

	// ReworkCount is dispatcher-owned runtime state, like BoardStatus and the
	// claim fields: it counts how many times this issue has landed in the
	// rework column (amendment C1) -- the Reviewer lane's changes_requested
	// from review, and the Git-operator lane's stale-base-conflict route from
	// merging both count, since both mean "the Coder lane gets another
	// attempt". It resets to 0 once the issue reaches done, or once a human
	// requeues it out of Blocked back to ready/todo (see
	// dispatcher.adoptLinearMove / TransitionReq.ResetReworkCount). A claim
	// released without a genuine rework edge (dispatcher.requeueOrphan's
	// re-assert of a column the card was already in — see
	// TransitionReq.SkipReworkBump) never bumps it either way. A Linear
	// re-poll (UpsertIssue's conflict path) never touches it.
	ReworkCount int

	// RecoverAttempts is dispatcher-owned runtime state (like ReworkCount and
	// the claim fields): it counts how many times auto-unblock layer 1 has
	// deterministically re-queued this issue after a *transient* failure (a
	// worker block_kind=transient, a run-level crash/malformed/timeout, or a
	// spawn/workspace failure -- see dispatcher.parkOrRetry). Once it reaches
	// cfg.RecoverCap the issue parks in Blocked for good. It resets to 0 the
	// next time the card advances on a normal (non-block) terminal transition
	// (TransitionReq.ResetRecoverAttempts), or once a human requeues it out of
	// Blocked back to ready/todo (dispatcher.adoptLinearMove), mirroring
	// ReworkCount's own reset there. A Linear re-poll (UpsertIssue's conflict
	// path) never touches it.
	RecoverAttempts int

	// BlockedUntil is the unix time (0 = not blocked) before which this issue
	// is NOT claimable: an auto-retry re-queue sets it to now+RecoverBackoffS
	// so the retried card sits out a backoff window rather than being
	// re-claimed on the very next tick. Every claim/peek candidate query
	// filters it (blocked_until <= now), which is what makes the retry budget
	// a real anti-hot-loop guard. Cleared back to 0 when RecoverAttempts
	// resets. Like RecoverAttempts, dispatcher-owned and preserved across a
	// Linear re-poll.
	BlockedUntil int64

	Deps         string
	Priority     int
	BranchName   string
	ClaimLock    sql.NullString
	ClaimExpires sql.NullInt64
	UpdatedAt    int64
	LastSeen     int64
	CreatedAt    int64
}

// Run mirrors a row in the runs table: one row per dispatch attempt/turn.
type Run struct {
	RunID         string
	IssueID       string
	Lane          string
	WorkerPID     sql.NullInt64
	ProcStartedAt sql.NullInt64
	Status        string
	StartedAt     int64
	HeartbeatAt   int64
	Attempt       int
	TurnCount     int
	ThreadID      string
	ResultJSON    sql.NullString
	Error         sql.NullString
	TokensIn      int
	TokensOut     int
}

// Event mirrors a row in the events table: the append-only audit stream.
type Event struct {
	ID      int64
	Ts      int64
	IssueID sql.NullString
	RunID   sql.NullString
	Kind    string
	Detail  string
}

// LinearWrite mirrors a row in the linear_writes table: a pending or
// completed outbound mirror write to Linear (A2's at-least-once outbox).
// Kind is "setstate" (mirror a board_status change via Target) or "comment"
// (post Body as an issue comment).
type LinearWrite struct {
	ID        int64
	IssueID   string
	Kind      string
	Target    string
	Body      string
	Status    string
	Attempts  int
	LastError sql.NullString
	CreatedAt int64
	UpdatedAt int64
}

// WorkspaceState is the provider-neutral lifecycle state of an agent's
// execution workspace. Provider-specific states are normalized before they
// enter the store.
type WorkspaceState string

const (
	WorkspaceActive         WorkspaceState = "active"
	WorkspaceStopped        WorkspaceState = "stopped"
	WorkspaceCleanupPending WorkspaceState = "cleanup_pending"
	WorkspaceDeleted        WorkspaceState = "deleted"
	WorkspaceError          WorkspaceState = "error"
)

// AgentWorkspace records the durable provider-neutral identity and lifecycle
// state of one remote agent workspace. Coder workspaces are issue-scoped and
// persistent; reviewer workspaces also carry their run id.
type AgentWorkspace struct {
	OwnerKey      string
	IssueID       string
	RunID         string
	Provider      string
	Role          string
	ExternalID    string
	WorkspacePath string
	State         WorkspaceState
	LastAction    string
	LastError     string
	CreatedAt     int64
	UpdatedAt     int64
}

// IssueSnapshot pairs an Issue with its most recent Run, if any.
type IssueSnapshot struct {
	Issue
	LatestRun *Run
	Workspace *AgentWorkspace

	// Runs is every run for this issue in chronological order (oldest first:
	// coder, then reviewer, then git_operator as the card
	// advances). LatestRun is retained as the convenience "current lane"
	// pointer; Runs is what the TUI's per-issue detail view walks to show the
	// full lane history. A card with no runs yet has an empty (nil) slice.
	Runs []Run

	// TokensInTotal / TokensOutTotal sum tokens across ALL of this issue's
	// runs (every lane it has passed through — coder, reviewer, git_operator, ...),
	// not just LatestRun. Displaying LatestRun's tokens alone dropped every
	// earlier lane's usage the moment a card advanced, which read as the
	// counters "not updating".
	TokensInTotal  int
	TokensOutTotal int

	// Unmirrored is true iff this issue has at least one linear_writes row
	// with status='pending' (A2's outbox) — i.e. a board transition
	// committed locally but hasn't yet been mirrored to Linear, typically
	// because Linear was unreachable when the dispatcher tried to drain it.
	Unmirrored bool
}

// Snapshot is a point-in-time read of the kernel store, shaped for
// rendering `clipse status` / `clipse tui`.
type Snapshot struct {
	Issues         []IssueSnapshot
	CountsByStatus map[string]int

	// TotalTokensIn / TotalTokensOut sum tokens across every run of every
	// issue — the board-wide cumulative spend the dashboard header shows.
	TotalTokensIn  int
	TotalTokensOut int

	// UnmirroredCount is the number of issues with Unmirrored=true, i.e. how
	// many issues currently have a pending Linear mirror write outstanding.
	UnmirroredCount int

	// RecentEvents is the tail of the append-only events stream, newest-first
	// (highest id first), capped at a small limit for the TUI's activity feed.
	RecentEvents []Event

	// LastEventAt is the maximum ts across RecentEvents (0 when there are no
	// events). The TUI derives a "last activity Ns ago" liveness reading from
	// it. It is a wall-clock-free datum: View, not the pure Update, turns it
	// into an age against time.Now.
	LastEventAt int64
}
