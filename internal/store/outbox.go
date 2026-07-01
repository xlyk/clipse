package store

import (
	"context"
	"database/sql"
	"fmt"
)

// TransitionReq describes one atomic board transition: the SQLite state
// change (issues.board_status, optionally clearing the claim), the run it
// closes out (if any), the audit event to append, and the Linear outbox
// rows to enqueue (A2) — all applied in a single transaction by Transition.
type TransitionReq struct {
	IssueID    string
	NewStatus  string // target board column
	ClearClaim bool   // null out claim_lock/claim_expires (terminal/blocked/requeue)

	CloseRunID string // if non-empty, close this run
	RunStatus  string // status to set on the closed run (e.g. "done","blocked","stale","orphaned")
	RunError   string // optional; stored NULL if empty
	ResultJSON string // optional; stored NULL if empty
	TokensIn   int
	TokensOut  int

	Event Event // audit event to append (ts/kind/detail; issue_id/run_id as given)

	EnqueueSetState bool   // enqueue a linear_writes setstate mirror to NewStatus
	Comment         string // if non-empty, enqueue a linear_writes comment row
}

// Transition applies a board.Next result atomically: the issues state
// change, the run close-out, the audit event, and the outbound Linear
// mirror enqueue all commit together in one transaction, or none of them
// do. This is what keeps the outbox (A2) consistent with the transition it
// mirrors — a crash between "commit the transition" and "enqueue the
// mirror" can't happen because they're the same commit.
func (s *Store) Transition(ctx context.Context, req TransitionReq) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("transitioning issue %s: beginning tx: %w", req.IssueID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if already committed

	if err := applyIssueTransition(ctx, tx, req); err != nil {
		return err
	}
	if req.CloseRunID != "" {
		if err := applyRunClose(ctx, tx, req); err != nil {
			return err
		}
	}
	if err := appendEventTx(ctx, tx, req.Event); err != nil {
		return fmt.Errorf("transitioning issue %s: appending event: %w", req.IssueID, err)
	}
	if req.EnqueueSetState {
		if err := enqueueLinearWrite(ctx, tx, req.IssueID, "setstate", req.NewStatus, "", req.Event.Ts); err != nil {
			return fmt.Errorf("transitioning issue %s: enqueueing setstate: %w", req.IssueID, err)
		}
	}
	if req.Comment != "" {
		if err := enqueueLinearWrite(ctx, tx, req.IssueID, "comment", "", req.Comment, req.Event.Ts); err != nil {
			return fmt.Errorf("transitioning issue %s: enqueueing comment: %w", req.IssueID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("transitioning issue %s: committing: %w", req.IssueID, err)
	}
	return nil
}

func applyIssueTransition(ctx context.Context, tx *sql.Tx, req TransitionReq) error {
	q := `UPDATE issues SET board_status = ?`
	args := []any{req.NewStatus}
	if req.ClearClaim {
		q += `, claim_lock = NULL, claim_expires = NULL`
	}
	q += ` WHERE id = ?`
	args = append(args, req.IssueID)

	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("transitioning issue %s: updating issue: %w", req.IssueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transitioning issue %s: reading rows affected: %w", req.IssueID, err)
	}
	if n == 0 {
		return fmt.Errorf("transitioning issue %s: no such issue", req.IssueID)
	}
	return nil
}

func applyRunClose(ctx context.Context, tx *sql.Tx, req TransitionReq) error {
	const q = `
		UPDATE runs SET
			status      = ?,
			result_json = ?,
			error       = ?,
			tokens_in   = ?,
			tokens_out  = ?
		WHERE run_id = ?
	`
	result := sql.NullString{String: req.ResultJSON, Valid: req.ResultJSON != ""}
	runErr := sql.NullString{String: req.RunError, Valid: req.RunError != ""}

	res, err := tx.ExecContext(ctx, q, req.RunStatus, result, runErr, req.TokensIn, req.TokensOut, req.CloseRunID)
	if err != nil {
		return fmt.Errorf("transitioning issue %s: closing run %s: %w", req.IssueID, req.CloseRunID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transitioning issue %s: closing run %s: reading rows affected: %w", req.IssueID, req.CloseRunID, err)
	}
	if n == 0 {
		return fmt.Errorf("transitioning issue %s: closing run %s: no such run", req.IssueID, req.CloseRunID)
	}
	return nil
}

func appendEventTx(ctx context.Context, tx *sql.Tx, event Event) error {
	const q = `
		INSERT INTO events (ts, issue_id, run_id, kind, detail)
		VALUES (?, ?, ?, ?, ?)
	`
	_, err := tx.ExecContext(ctx, q, event.Ts, event.IssueID, event.RunID, event.Kind, event.Detail)
	return err
}

func enqueueLinearWrite(ctx context.Context, tx *sql.Tx, issueID, kind, target, body string, now int64) error {
	const q = `
		INSERT INTO linear_writes (issue_id, kind, target, body, status, attempts, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', 0, ?, ?)
	`
	_, err := tx.ExecContext(ctx, q, issueID, kind, target, body, now, now)
	return err
}

// EnqueueLinearSetState enqueues a standalone pending 'setstate' linear_writes
// row mirroring issueID's board state to column, outside of a Transition.
// This exists because ClaimReady's CAS win (ready -> running) is not itself a
// Transition call, so nothing else enqueues the outbox row that mirrors a
// fresh claim to Linear.
func (s *Store) EnqueueLinearSetState(ctx context.Context, issueID, column string, now int64) error {
	const q = `
		INSERT INTO linear_writes (issue_id, kind, target, body, status, attempts, created_at, updated_at)
		VALUES (?, 'setstate', ?, '', 'pending', 0, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, q, issueID, column, now, now)
	if err != nil {
		return fmt.Errorf("enqueueing setstate for issue %s: %w", issueID, err)
	}
	return nil
}

// DrainPendingLinearWrites returns up to limit pending linear_writes rows,
// ordered by id (oldest first), so the dispatcher processes the outbox in
// enqueue order.
func (s *Store) DrainPendingLinearWrites(ctx context.Context, limit int) ([]LinearWrite, error) {
	const q = `
		SELECT id, issue_id, kind, target, body, status, attempts, last_error, created_at, updated_at
		FROM linear_writes
		WHERE status = 'pending'
		ORDER BY id
		LIMIT ?
	`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("draining pending linear writes: %w", err)
	}
	defer rows.Close()

	var writes []LinearWrite
	for rows.Next() {
		var w LinearWrite
		if err := rows.Scan(
			&w.ID, &w.IssueID, &w.Kind, &w.Target, &w.Body, &w.Status, &w.Attempts, &w.LastError, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning pending linear write row: %w", err)
		}
		writes = append(writes, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pending linear write rows: %w", err)
	}
	return writes, nil
}

// MarkLinearWriteDone marks id as successfully mirrored to Linear, removing
// it from future DrainPendingLinearWrites results. now is the caller-supplied
// timestamp for updated_at, matching the rest of the store's convention
// (ClaimReady/Heartbeat/ReleaseStaleClaims/Transition all take a caller now
// rather than relying on SQLite's unixepoch()).
func (s *Store) MarkLinearWriteDone(ctx context.Context, id int64, now int64) error {
	const q = `UPDATE linear_writes SET status = 'done', updated_at = ? WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, now, id)
	if err != nil {
		return fmt.Errorf("marking linear write %d done: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("marking linear write %d done: reading rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("marking linear write %d done: no such row", id)
	}
	return nil
}

// MarkLinearWriteFailed records a failed mirror attempt: it increments
// attempts and stores errStr + updated_at (using the caller-supplied now, per
// the store's convention — see MarkLinearWriteDone), but leaves
// status='pending' so the dispatcher retries it on a later tick.
func (s *Store) MarkLinearWriteFailed(ctx context.Context, id int64, errStr string, now int64) error {
	const q = `
		UPDATE linear_writes SET
			attempts   = attempts + 1,
			last_error = ?,
			status     = 'pending',
			updated_at = ?
		WHERE id = ?
	`
	res, err := s.db.ExecContext(ctx, q, errStr, now, id)
	if err != nil {
		return fmt.Errorf("marking linear write %d failed: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("marking linear write %d failed: reading rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("marking linear write %d failed: no such row", id)
	}
	return nil
}
