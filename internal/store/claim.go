package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNoReady is returned by ClaimReady when no issue in the requested lane
// is currently claimable: either none are 'ready', or a concurrent claimer
// won the race for the only candidate.
var ErrNoReady = errors.New("no ready issue to claim")

// Claim is the result of a successful ClaimReady: the issue that was won
// (now board_status='running', claim_lock=runID) plus the run row created
// to track the attempt.
type Claim struct {
	Issue Issue
	Run   Run
}

// ClaimReady atomically claims the single best 'ready' issue in laneLabel
// for runID.
//
// Candidate selection orders by (priority, created_at, identifier): Linear
// priority is 0=none,1=urgent,2=high,3=medium,4=low, so the most urgent
// actionable issue is claimed first and 0=none (no priority set) is treated
// as the lowest urgency, claimed last. Ties break on created_at ascending
// (oldest first), then identifier ascending, so claim order is deterministic
// even when two issues share priority and created_at.
//
// The claim itself is a compare-and-swap: an UPDATE guarded by
// `board_status='ready' AND claim_lock IS NULL` on the selected id. Under
// concurrent callers racing for the same issue, SQLite's BEGIN IMMEDIATE
// (configured via the store's "_txlock=immediate" DSN option) serializes
// writers at the reserved-lock level, so only one caller's UPDATE can affect
// a row; every other concurrent caller either sees a different (already
// claimed) row or an UPDATE that affects zero rows, and returns ErrNoReady.
// 'running' is entered ONLY through this path.
//
// On a win, ClaimReady inserts a fresh Run (attempt=1, turn_count=1,
// status="running") and appends a "claimed" event, all in the same
// transaction, then returns the resulting Claim.
func (s *Store) ClaimReady(ctx context.Context, laneLabel, runID string, now, ttl int64) (*Claim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claiming ready issue: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if already committed

	const selectQ = `
		SELECT id, identifier, lane_label, board_status, deps, priority,
			branch_name, claim_lock, claim_expires, updated_at, last_seen, created_at
		FROM issues
		WHERE board_status = 'ready' AND claim_lock IS NULL AND lane_label = ?
		ORDER BY
			CASE priority WHEN 0 THEN 999999999 ELSE priority END ASC,
			created_at ASC,
			identifier ASC
		LIMIT 1
	`
	var issue Issue
	err = tx.QueryRowContext(ctx, selectQ, laneLabel).Scan(
		&issue.ID, &issue.Identifier, &issue.LaneLabel, &issue.BoardStatus, &issue.Deps, &issue.Priority,
		&issue.BranchName, &issue.ClaimLock, &issue.ClaimExpires, &issue.UpdatedAt, &issue.LastSeen, &issue.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoReady
	}
	if err != nil {
		return nil, fmt.Errorf("claiming ready issue: selecting candidate: %w", err)
	}

	claimExpires := now + ttl
	const casQ = `
		UPDATE issues SET board_status = 'running', claim_lock = ?, claim_expires = ?
		WHERE id = ? AND board_status = 'ready' AND claim_lock IS NULL
	`
	res, err := tx.ExecContext(ctx, casQ, runID, claimExpires, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("claiming ready issue %s: cas update: %w", issue.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("claiming ready issue %s: reading rows affected: %w", issue.ID, err)
	}
	if n != 1 {
		// Lost the race: another transaction claimed this row (or the wider
		// snapshot) between our SELECT and our UPDATE.
		return nil, ErrNoReady
	}

	var nextAttempt int
	const attemptQ = `SELECT COALESCE(MAX(attempt), 0) + 1 FROM runs WHERE issue_id = ?`
	if err := tx.QueryRowContext(ctx, attemptQ, issue.ID).Scan(&nextAttempt); err != nil {
		return nil, fmt.Errorf("claiming ready issue %s: computing next attempt: %w", issue.ID, err)
	}

	run := Run{
		RunID:       runID,
		IssueID:     issue.ID,
		Lane:        laneLabel,
		Status:      "running",
		StartedAt:   now,
		HeartbeatAt: now,
		Attempt:     nextAttempt,
		TurnCount:   1,
		ThreadID:    "",
	}
	const insertRunQ = `
		INSERT INTO runs (
			run_id, issue_id, lane, worker_pid, proc_started_at, status, started_at, heartbeat_at,
			attempt, turn_count, thread_id, result_json, error, tokens_in, tokens_out
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if _, err := tx.ExecContext(ctx, insertRunQ,
		run.RunID, run.IssueID, run.Lane, run.WorkerPID, run.ProcStartedAt, run.Status, run.StartedAt, run.HeartbeatAt,
		run.Attempt, run.TurnCount, run.ThreadID, run.ResultJSON, run.Error, run.TokensIn, run.TokensOut,
	); err != nil {
		return nil, fmt.Errorf("claiming ready issue %s: inserting run %s: %w", issue.ID, run.RunID, err)
	}

	const insertEventQ = `
		INSERT INTO events (ts, issue_id, run_id, kind, detail)
		VALUES (?, ?, ?, 'claimed', ?)
	`
	detail := fmt.Sprintf("claimed by run %s", runID)
	if _, err := tx.ExecContext(ctx, insertEventQ,
		now, sql.NullString{String: issue.ID, Valid: true}, sql.NullString{String: runID, Valid: true}, detail,
	); err != nil {
		return nil, fmt.Errorf("claiming ready issue %s: appending claimed event: %w", issue.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claiming ready issue %s: committing: %w", issue.ID, err)
	}

	issue.BoardStatus = "running"
	issue.ClaimLock = sql.NullString{String: runID, Valid: true}
	issue.ClaimExpires = sql.NullInt64{Int64: claimExpires, Valid: true}

	return &Claim{Issue: issue, Run: run}, nil
}

// Heartbeat extends the TTL of runID's claim: it pushes the owning issue's
// claim_expires to now+ttl and records the run's heartbeat_at. It errors if
// runID has no active claim (i.e. no issue currently locked by it).
func (s *Store) Heartbeat(ctx context.Context, runID string, now, ttl int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("heartbeat for run %s: beginning tx: %w", runID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if already committed

	claimExpires := now + ttl
	const issueQ = `UPDATE issues SET claim_expires = ? WHERE claim_lock = ?`
	res, err := tx.ExecContext(ctx, issueQ, claimExpires, runID)
	if err != nil {
		return fmt.Errorf("heartbeat for run %s: extending claim_expires: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("heartbeat for run %s: reading rows affected: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("heartbeat for run %s: no active claim", runID)
	}

	const runQ = `UPDATE runs SET heartbeat_at = ? WHERE run_id = ?`
	runRes, err := tx.ExecContext(ctx, runQ, now, runID)
	if err != nil {
		return fmt.Errorf("heartbeat for run %s: updating heartbeat_at: %w", runID, err)
	}
	runN, err := runRes.RowsAffected()
	if err != nil {
		return fmt.Errorf("heartbeat for run %s: reading rows affected: %w", runID, err)
	}
	if runN == 0 {
		return fmt.Errorf("heartbeat for run %s: no such run", runID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("heartbeat for run %s: committing: %w", runID, err)
	}
	return nil
}

// ReleaseStaleClaims requeues every 'running' issue whose claim_expires has
// passed now: it resets the issue to board_status='ready' with claim_lock
// and claim_expires cleared, marks its still-'running' run 'stale', and
// appends a "stale_release" event per released issue. It returns the number
// of issues released.
func (s *Store) ReleaseStaleClaims(ctx context.Context, now int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("releasing stale claims: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if already committed

	const selectQ = `
		SELECT id, claim_lock FROM issues
		WHERE board_status = 'running' AND claim_expires IS NOT NULL AND claim_expires < ?
	`
	rows, err := tx.QueryContext(ctx, selectQ, now)
	if err != nil {
		return 0, fmt.Errorf("releasing stale claims: selecting candidates: %w", err)
	}

	type staleIssue struct {
		id        string
		claimLock sql.NullString
	}
	var stale []staleIssue
	for rows.Next() {
		var si staleIssue
		if err := rows.Scan(&si.id, &si.claimLock); err != nil {
			rows.Close()
			return 0, fmt.Errorf("releasing stale claims: scanning candidate row: %w", err)
		}
		stale = append(stale, si)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("releasing stale claims: iterating candidate rows: %w", err)
	}
	rows.Close()

	const requeueQ = `
		UPDATE issues SET board_status = 'ready', claim_lock = NULL, claim_expires = NULL
		WHERE id = ?
	`
	const staleRunQ = `UPDATE runs SET status = 'stale' WHERE issue_id = ? AND status = 'running'`
	const eventQ = `
		INSERT INTO events (ts, issue_id, run_id, kind, detail)
		VALUES (?, ?, ?, 'stale_release', ?)
	`

	for _, si := range stale {
		if _, err := tx.ExecContext(ctx, requeueQ, si.id); err != nil {
			return 0, fmt.Errorf("releasing stale claims: requeuing issue %s: %w", si.id, err)
		}
		if _, err := tx.ExecContext(ctx, staleRunQ, si.id); err != nil {
			return 0, fmt.Errorf("releasing stale claims: marking runs stale for issue %s: %w", si.id, err)
		}
		detail := fmt.Sprintf("released stale claim %s", si.claimLock.String)
		if _, err := tx.ExecContext(ctx, eventQ, now, sql.NullString{String: si.id, Valid: true}, si.claimLock, detail); err != nil {
			return 0, fmt.Errorf("releasing stale claims: appending stale_release event for issue %s: %w", si.id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("releasing stale claims: committing: %w", err)
	}

	return len(stale), nil
}
