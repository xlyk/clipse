package store

import (
	"context"
	"database/sql"
	"fmt"
)

// UpsertIssue inserts or updates the cache row for a normalized Linear
// issue.
//
// Conflict behavior (on a matching id): only the Linear-sourced columns
// (identifier, lane_label, board_status, deps, priority, branch_name,
// updated_at) plus last_seen are overwritten. claim_lock and claim_expires
// are dispatcher-owned (set by the CAS claim path added in a later task) and
// are never touched here, so a re-poll of Linear can never clobber an
// in-flight claim. created_at is preserved from the original insert.
//
// board_status is updated straight from the Linear-sourced value on every
// call, including conflicts. That's intentionally permissive for this task:
// CAS/ownership of board_status transitions (e.g. refusing to overwrite a
// dispatcher-driven Running/Review/etc. status from a stale poll) is
// enforced by the state machine added in a later task, not here.
func (s *Store) UpsertIssue(ctx context.Context, issue Issue) error {
	const q = `
		INSERT INTO issues (
			id, identifier, lane_label, board_status, deps, priority,
			branch_name, claim_lock, claim_expires, updated_at, last_seen, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			identifier   = excluded.identifier,
			lane_label   = excluded.lane_label,
			board_status = excluded.board_status,
			deps         = excluded.deps,
			priority     = excluded.priority,
			branch_name  = excluded.branch_name,
			updated_at   = excluded.updated_at,
			last_seen    = excluded.last_seen
	`
	_, err := s.db.ExecContext(ctx, q,
		issue.ID, issue.Identifier, issue.LaneLabel, issue.BoardStatus, issue.Deps, issue.Priority,
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
			run_id, issue_id, lane, worker_pid, status, started_at, heartbeat_at,
			attempt, turn_count, thread_id, result_json, error, tokens_in, tokens_out
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, q,
		run.RunID, run.IssueID, run.Lane, run.WorkerPID, run.Status, run.StartedAt, run.HeartbeatAt,
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

// ReadSnapshot returns every issue with its board_status and latest run
// state, plus per-status issue counts, enough to render `clipse status` /
// `clipse tui`.
func (s *Store) ReadSnapshot(ctx context.Context) (Snapshot, error) {
	const issuesQ = `
		SELECT id, identifier, lane_label, board_status, deps, priority,
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
				&is.ID, &is.Identifier, &is.LaneLabel, &is.BoardStatus, &is.Deps, &is.Priority,
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

	for i := range snap.Issues {
		latest, err := s.latestRun(ctx, snap.Issues[i].ID)
		if err != nil {
			return Snapshot{}, err
		}
		snap.Issues[i].LatestRun = latest
	}

	return snap, nil
}

// latestRun returns the most recently started run for issueID, or nil if
// the issue has never had a run.
func (s *Store) latestRun(ctx context.Context, issueID string) (*Run, error) {
	const q = `
		SELECT run_id, issue_id, lane, worker_pid, status, started_at, heartbeat_at,
			attempt, turn_count, thread_id, result_json, error, tokens_in, tokens_out
		FROM runs
		WHERE issue_id = ?
		ORDER BY started_at DESC, run_id DESC
		LIMIT 1
	`
	var r Run
	err := s.db.QueryRowContext(ctx, q, issueID).Scan(
		&r.RunID, &r.IssueID, &r.Lane, &r.WorkerPID, &r.Status, &r.StartedAt, &r.HeartbeatAt,
		&r.Attempt, &r.TurnCount, &r.ThreadID, &r.ResultJSON, &r.Error, &r.TokensIn, &r.TokensOut,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading latest run for issue %s: %w", issueID, err)
	}
	return &r, nil
}
