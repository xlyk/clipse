package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// UpsertIssue inserts or updates the cache row for a normalized Linear
// issue.
//
// Conflict behavior (on a matching id): only the Linear-sourced intent
// columns (identifier, title, description, lane_label, deps, priority,
// branch_name, updated_at) plus last_seen are overwritten. board_status,
// rework_count, recover_attempts, blocked_until, claim_lock, and claim_expires
// are dispatcher-owned runtime state: they are set on the initial insert and
// never touched on conflict, so a re-poll of Linear can neither clobber an
// in-flight claim nor reset a dispatcher-driven status (e.g. running/review),
// its rework_count, or an active auto-retry backoff (recover_attempts /
// blocked_until) -- see amendment C1 / Transition, the only writer. created_at
// is preserved from the original insert.
//
// title/description round-trip here purely as cached Linear content (the
// dispatcher's CLIPSE_ISSUE_TEXT env injection reads them off a claimed
// issue) -- they carry no special claim/board semantics of their own, so an
// edited Linear title/description simply updates like identifier/priority
// on every re-poll, even while the issue is running under an active claim.
//
// board_status transitions after insert are made only by dispatcher-owned
// paths (the CAS claim + the state machine, added in later tasks). Reflecting
// an out-of-band human requeue in Linear (Blocked -> Ready) is a separate
// reconciliation concern, deferred beyond Phase 1.
func (s *Store) UpsertIssue(ctx context.Context, issue Issue) error {
	const q = `
		INSERT INTO issues (
			id, identifier, title, description, lane_label, board_status, rework_count, recover_attempts, blocked_until, deps, priority,
			branch_name, claim_lock, claim_expires, updated_at, last_seen, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			identifier   = excluded.identifier,
			title        = excluded.title,
			description  = excluded.description,
			lane_label   = excluded.lane_label,
			deps         = excluded.deps,
			priority     = excluded.priority,
			branch_name  = excluded.branch_name,
			updated_at   = excluded.updated_at,
			last_seen    = excluded.last_seen
	`
	_, err := s.db.ExecContext(ctx, q,
		issue.ID, issue.Identifier, issue.Title, issue.Description, issue.LaneLabel, issue.BoardStatus, issue.ReworkCount, issue.RecoverAttempts, issue.BlockedUntil, issue.Deps, issue.Priority,
		issue.BranchName, issue.ClaimLock, issue.ClaimExpires, issue.UpdatedAt, issue.LastSeen, issue.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("upserting issue %s: %w", issue.ID, err)
	}
	return nil
}

// AppendEvent appends a row to the append-only events audit stream.
func (s *Store) AppendEvent(ctx context.Context, event Event) error {
	const q = `
		INSERT INTO events (ts, issue_id, run_id, kind, detail)
		VALUES (?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, q, event.Ts, event.IssueID, event.RunID, event.Kind, event.Detail)
	if err != nil {
		return fmt.Errorf("appending event kind=%s: %w", event.Kind, err)
	}
	return nil
}

// ListEvents returns every row in the events table ordered by id. It exists
// primarily to make AppendEvent testable; higher-level TUI/status filtering
// lands in a later task.
func (s *Store) ListEvents(ctx context.Context) ([]Event, error) {
	const q = `SELECT id, ts, issue_id, run_id, kind, detail FROM events ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Ts, &e.IssueID, &e.RunID, &e.Kind, &e.Detail); err != nil {
			return nil, fmt.Errorf("scanning event row: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating event rows: %w", err)
	}
	return events, nil
}

// InsertRun inserts a new row in the runs table for a claimed issue attempt.
func (s *Store) InsertRun(ctx context.Context, run Run) error {
	const q = `
		INSERT INTO runs (
			run_id, issue_id, lane, worker_pid, proc_started_at, status, started_at, heartbeat_at,
			attempt, turn_count, thread_id, result_json, error, tokens_in, tokens_out
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, q,
		run.RunID, run.IssueID, run.Lane, run.WorkerPID, run.ProcStartedAt, run.Status, run.StartedAt, run.HeartbeatAt,
		run.Attempt, run.TurnCount, run.ThreadID, run.ResultJSON, run.Error, run.TokensIn, run.TokensOut,
	)
	if err != nil {
		return fmt.Errorf("inserting run %s: %w", run.RunID, err)
	}
	return nil
}

// CloseRun records the terminal outcome of a run: its final status, the
// worker's typed result (resultJSON) or error, and token usage. An empty
// resultJSON or errStr is stored as NULL rather than an empty string, so
// downstream readers can distinguish "no result" from "empty result".
func (s *Store) CloseRun(ctx context.Context, runID, status, resultJSON, errStr string, tokensIn, tokensOut int) error {
	const q = `
		UPDATE runs SET
			status      = ?,
			result_json = ?,
			error       = ?,
			tokens_in   = ?,
			tokens_out  = ?
		WHERE run_id = ?
	`
	result := sql.NullString{String: resultJSON, Valid: resultJSON != ""}
	runErr := sql.NullString{String: errStr, Valid: errStr != ""}

	res, err := s.db.ExecContext(ctx, q, status, result, runErr, tokensIn, tokensOut, runID)
	if err != nil {
		return fmt.Errorf("closing run %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("closing run %s: reading rows affected: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("closing run %s: no such run", runID)
	}
	return nil
}

// SetRunProcess records the worker process identity for runID: its OS pid
// and process start time. The dispatcher calls this immediately after
// spawning the worker so a later restart can verify (via proc_started_at)
// that a live PID is actually the same process it spawned, rather than an
// unrelated process the OS reused the PID for (A1's PID-reuse guard).
func (s *Store) SetRunProcess(ctx context.Context, runID string, pid int, procStartedAt int64) error {
	const q = `UPDATE runs SET worker_pid = ?, proc_started_at = ? WHERE run_id = ?`
	res, err := s.db.ExecContext(ctx, q, pid, procStartedAt, runID)
	if err != nil {
		return fmt.Errorf("setting run process for %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("setting run process for %s: reading rows affected: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("setting run process for %s: no such run", runID)
	}
	return nil
}

// ListOpenRuns returns every run row with status='running'. On dispatcher
// startup (A1), these are the runs that may be orphaned by a previous
// process's death: each is checked for a live, matching worker process,
// killed if still running, and closed/requeued before any stale-claim
// release happens.
func (s *Store) ListOpenRuns(ctx context.Context) ([]Run, error) {
	const q = `
		SELECT run_id, issue_id, lane, worker_pid, proc_started_at, status, started_at, heartbeat_at,
			attempt, turn_count, thread_id, result_json, error, tokens_in, tokens_out
		FROM runs
		WHERE status = 'running'
		ORDER BY run_id
	`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing open runs: %w", err)
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(
			&r.RunID, &r.IssueID, &r.Lane, &r.WorkerPID, &r.ProcStartedAt, &r.Status, &r.StartedAt, &r.HeartbeatAt,
			&r.Attempt, &r.TurnCount, &r.ThreadID, &r.ResultJSON, &r.Error, &r.TokensIn, &r.TokensOut,
		); err != nil {
			return nil, fmt.Errorf("scanning open run row: %w", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating open run rows: %w", err)
	}
	return runs, nil
}

// ReadSnapshot returns every issue with its board_status and latest run
// state, plus per-status issue counts, enough to render `clipse status` /
// `clipse tui`.
func (s *Store) ReadSnapshot(ctx context.Context) (Snapshot, error) {
	const issuesQ = `
		SELECT id, identifier, lane_label, board_status, rework_count, recover_attempts, blocked_until, deps, priority,
			branch_name, claim_lock, claim_expires, updated_at, last_seen, created_at
		FROM issues
		ORDER BY id
	`
	rows, err := s.db.QueryContext(ctx, issuesQ)
	if err != nil {
		return Snapshot{}, fmt.Errorf("reading issues snapshot: %w", err)
	}

	var snap Snapshot
	snap.CountsByStatus = make(map[string]int)

	func() {
		defer rows.Close()
		for rows.Next() {
			var is IssueSnapshot
			if err = rows.Scan(
				&is.ID, &is.Identifier, &is.LaneLabel, &is.BoardStatus, &is.ReworkCount, &is.RecoverAttempts, &is.BlockedUntil, &is.Deps, &is.Priority,
				&is.BranchName, &is.ClaimLock, &is.ClaimExpires, &is.UpdatedAt, &is.LastSeen, &is.CreatedAt,
			); err != nil {
				return
			}
			snap.Issues = append(snap.Issues, is)
			snap.CountsByStatus[is.BoardStatus]++
		}
	}()
	if err != nil {
		return Snapshot{}, fmt.Errorf("scanning issue row: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterating issue rows: %w", err)
	}

	// A single runsByIssue query loads every run for every issue, already
	// ordered oldest-first per issue (ORDER BY issue_id, started_at, run_id).
	// LatestRun is just that per-issue slice's last element, so deriving it
	// here replaces what used to be a separate latestRun(ctx, id) query PER
	// ISSUE -- an N+1 on top of data this function was already loading -- with
	// zero extra queries.
	runs, err := s.runsByIssue(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	for i := range snap.Issues {
		issueRuns := runs[snap.Issues[i].ID]
		snap.Issues[i].Runs = issueRuns
		if n := len(issueRuns); n > 0 {
			latest := issueRuns[n-1]
			snap.Issues[i].LatestRun = &latest
		}
	}

	tokensIn, tokensOut, err := s.tokenTotalsByIssue(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	for i := range snap.Issues {
		id := snap.Issues[i].ID
		snap.Issues[i].TokensInTotal = tokensIn[id]
		snap.Issues[i].TokensOutTotal = tokensOut[id]
		snap.TotalTokensIn += tokensIn[id]
		snap.TotalTokensOut += tokensOut[id]
	}

	unmirrored, err := s.unmirroredIssueIDs(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	for i := range snap.Issues {
		if unmirrored[snap.Issues[i].ID] {
			snap.Issues[i].Unmirrored = true
			snap.UnmirroredCount++
		}
	}

	events, err := s.recentEvents(ctx, recentEventLimit)
	if err != nil {
		return Snapshot{}, err
	}
	snap.RecentEvents = events
	for _, e := range events {
		if e.Ts > snap.LastEventAt {
			snap.LastEventAt = e.Ts
		}
	}

	return snap, nil
}

// recentEventLimit caps how many trailing events ReadSnapshot loads for the
// TUI activity feed. Small enough to keep the read cheap on a busy board, big
// enough to fill a feed panel.
const recentEventLimit = 15

// recentEvents returns the last `limit` events, newest-first (highest id
// first). Unlike ListEvents (ascending, whole table) this is the tail read the
// TUI activity feed wants; ordering by id — a monotonic AUTOINCREMENT — is a
// stable proxy for append order even if two events share a ts.
func (s *Store) recentEvents(ctx context.Context, limit int) ([]Event, error) {
	const q = `
		SELECT id, ts, issue_id, run_id, kind, detail
		FROM events
		ORDER BY id DESC
		LIMIT ?
	`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("reading recent events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Ts, &e.IssueID, &e.RunID, &e.Kind, &e.Detail); err != nil {
			return nil, fmt.Errorf("scanning recent event row: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recent event rows: %w", err)
	}
	return events, nil
}

// runsByIssue returns every run grouped by issue id, each issue's slice in
// chronological order (oldest first, by started_at then run_id). One grouped
// query rather than a per-issue lookup in ReadSnapshot's loop; an issue with
// no runs is simply absent from the map (nil slice on lookup).
func (s *Store) runsByIssue(ctx context.Context) (map[string][]Run, error) {
	const q = `
		SELECT run_id, issue_id, lane, worker_pid, proc_started_at, status, started_at, heartbeat_at,
			attempt, turn_count, thread_id, result_json, error, tokens_in, tokens_out
		FROM runs
		ORDER BY issue_id, started_at, run_id
	`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("reading runs by issue: %w", err)
	}
	defer rows.Close()

	byIssue := make(map[string][]Run)
	for rows.Next() {
		var r Run
		if err := rows.Scan(
			&r.RunID, &r.IssueID, &r.Lane, &r.WorkerPID, &r.ProcStartedAt, &r.Status, &r.StartedAt, &r.HeartbeatAt,
			&r.Attempt, &r.TurnCount, &r.ThreadID, &r.ResultJSON, &r.Error, &r.TokensIn, &r.TokensOut,
		); err != nil {
			return nil, fmt.Errorf("scanning run-by-issue row: %w", err)
		}
		byIssue[r.IssueID] = append(byIssue[r.IssueID], r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating run-by-issue rows: %w", err)
	}
	return byIssue, nil
}

// unmirroredIssueIDs returns the set of issue ids that have at least one
// pending linear_writes row (A2's outbox), via a single grouped query rather
// than a per-issue lookup in ReadSnapshot's issue loop.
func (s *Store) unmirroredIssueIDs(ctx context.Context) (map[string]bool, error) {
	const q = `SELECT DISTINCT issue_id FROM linear_writes WHERE status = 'pending'`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("reading unmirrored issue ids: %w", err)
	}
	defer rows.Close()

	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning unmirrored issue id row: %w", err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating unmirrored issue id rows: %w", err)
	}
	return ids, nil
}

// tokenTotalsByIssue returns per-issue cumulative token sums across ALL runs
// (every lane the issue has passed through), keyed by issue id, via one
// grouped query rather than a per-issue lookup in ReadSnapshot's loop. A card
// with no runs yet is simply absent from both maps (zero on lookup).
func (s *Store) tokenTotalsByIssue(ctx context.Context) (in, out map[string]int, err error) {
	const q = `SELECT issue_id, COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0) FROM runs GROUP BY issue_id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, fmt.Errorf("reading token totals: %w", err)
	}
	defer rows.Close()

	in = make(map[string]int)
	out = make(map[string]int)
	for rows.Next() {
		var id string
		var ti, to int
		if err := rows.Scan(&id, &ti, &to); err != nil {
			return nil, nil, fmt.Errorf("scanning token totals row: %w", err)
		}
		in[id] = ti
		out[id] = to
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating token totals rows: %w", err)
	}
	return in, out, nil
}

// GetIssue fetches the single issue row for id, e.g. so the dispatcher can
// read an issue's current board_status before computing a board.Next
// transition without paying for a full ReadSnapshot.
func (s *Store) GetIssue(ctx context.Context, id string) (*Issue, error) {
	const q = `
		SELECT id, identifier, title, description, lane_label, board_status, rework_count, recover_attempts, blocked_until, deps, priority,
			branch_name, claim_lock, claim_expires, updated_at, last_seen, created_at
		FROM issues
		WHERE id = ?
	`
	var issue Issue
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&issue.ID, &issue.Identifier, &issue.Title, &issue.Description, &issue.LaneLabel, &issue.BoardStatus, &issue.ReworkCount, &issue.RecoverAttempts, &issue.BlockedUntil, &issue.Deps, &issue.Priority,
		&issue.BranchName, &issue.ClaimLock, &issue.ClaimExpires, &issue.UpdatedAt, &issue.LastSeen, &issue.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("getting issue %s: no such issue", id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting issue %s: %w", id, err)
	}
	return &issue, nil
}

// BumpRunTurn increments runs.turn_count for runID and returns the new
// value, so the dispatcher can advance a "continue" outcome's turn count
// (against cfg.TurnCap) in one round-trip.
func (s *Store) BumpRunTurn(ctx context.Context, runID string) (int, error) {
	const q = `UPDATE runs SET turn_count = turn_count + 1 WHERE run_id = ?`
	res, err := s.db.ExecContext(ctx, q, runID)
	if err != nil {
		return 0, fmt.Errorf("bumping turn for run %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("bumping turn for run %s: reading rows affected: %w", runID, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("bumping turn for run %s: no such run", runID)
	}

	var newTurn int
	const selectQ = `SELECT turn_count FROM runs WHERE run_id = ?`
	if err := s.db.QueryRowContext(ctx, selectQ, runID).Scan(&newTurn); err != nil {
		return 0, fmt.Errorf("bumping turn for run %s: reading new turn_count: %w", runID, err)
	}
	return newTurn, nil
}

// LatestReworkFeedback returns the human-readable feedback that most
// recently routed issueID to the rework column: the summary of the newest
// run whose status is "changes_requested". That covers both edges the board
// state machine treats identically (see dispatcher.applyTerminalWorkerOutcome):
// the Reviewer lane's changes_requested from review, and the Git-operator
// lane's stale-base-conflict route from merging (dispatcher.applyGitopsResult
// builds an equivalent changes_requested WorkerResult) -- both close their run
// with status "changes_requested" and their summary in runs.result_json.
//
// The summary is read out of result_json's "summary" field; when result_json
// is absent or carries an empty summary, runs.error is used as a fallback. It
// returns "" (and no error) when the issue has never had a changes_requested
// run, so a fresh Coder claim from ready threads no feedback -- only a
// re-run claimed out of the rework column does.
func (s *Store) LatestReworkFeedback(ctx context.Context, issueID string) (string, error) {
	const q = `
		SELECT result_json, error
		FROM runs
		WHERE issue_id = ? AND status = 'changes_requested'
		ORDER BY started_at DESC, run_id DESC
		LIMIT 1
	`
	var resultJSON, runErr sql.NullString
	err := s.db.QueryRowContext(ctx, q, issueID).Scan(&resultJSON, &runErr)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading latest rework feedback for issue %s: %w", issueID, err)
	}

	if resultJSON.Valid && resultJSON.String != "" {
		var payload struct {
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(resultJSON.String), &payload); err != nil {
			return "", fmt.Errorf("parsing rework feedback result_json for issue %s: %w", issueID, err)
		}
		if payload.Summary != "" {
			return payload.Summary, nil
		}
	}
	if runErr.Valid {
		return runErr.String, nil
	}
	return "", nil
}
