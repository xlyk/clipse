package store

import "database/sql"

// Issue mirrors a row in the issues table: a cache of Linear issue state
// plus dispatcher-owned claim fields. Deps is a JSON array (of issue ids)
// encoded as TEXT.
type Issue struct {
	ID           string
	Identifier   string
	LaneLabel    string
	BoardStatus  string
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

// IssueSnapshot pairs an Issue with its most recent Run, if any.
type IssueSnapshot struct {
	Issue
	LatestRun *Run
}

// Snapshot is a point-in-time read of the kernel store, shaped for
// rendering `clipse status` / `clipse tui`.
type Snapshot struct {
	Issues         []IssueSnapshot
	CountsByStatus map[string]int
}
