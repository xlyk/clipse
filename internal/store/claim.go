package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNoReady is returned by ClaimReady/ClaimColumn when no issue in the
// requested lane/column is currently claimable: either none are candidates,
// or a concurrent claimer won the race for the only one.
var ErrNoReady = errors.New("no ready issue to claim")

// ErrSchedulingPaused is returned before candidate selection when the
// board's durable control row forbids new claims.
var ErrSchedulingPaused = errors.New("dispatcher scheduling is paused")

func ensureSchedulingRunning(ctx context.Context, tx *sql.Tx) error {
	var desired SchedulingMode
	if err := tx.QueryRowContext(ctx, `SELECT desired_mode FROM dispatcher_control WHERE id = 1`).Scan(&desired); err != nil {
		return fmt.Errorf("reading dispatcher scheduling mode: %w", err)
	}
	if desired != SchedulingRunning {
		return ErrSchedulingPaused
	}
	return nil
}

// Claim is the result of a successful ClaimReady or ClaimColumn: the issue
// that was won plus the run row created to track the attempt.
type Claim struct {
	Issue Issue
	Run   Run
}

// claimCandidateColumns lists exactly the issues columns
// selectClaimCandidate scans, in order, so the SELECT and the Scan
// destinations underneath it can't drift apart.
const claimCandidateColumns = `
	id, identifier, title, description, lane_label, board_status, rework_count, recover_attempts, blocked_until, deps, priority,
	branch_name, claim_lock, claim_expires, updated_at, last_seen, created_at
`

// queryRowContexter is satisfied by both *sql.Tx and *sql.DB, letting
// selectClaimCandidate run either inside a claim's transaction (ClaimReady/
// ClaimColumn, guarding the subsequent CAS update) or directly against the
// database for a read-only peek (PeekReadyCandidate/PeekColumnCandidate)
// that commits to nothing.
type queryRowContexter interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// selectClaimCandidate scans the single best UNCLAIMED, non-backed-off issue
// row matching whereExtra (a boolean SQL fragment ANDed after "claim_lock IS
// NULL AND blocked_until <= now", with args bound in order AFTER now), against
// q. Candidates are ordered by (priority, created_at, identifier): Linear
// priority is 0=none,1=urgent,2=high,3=medium,4=low, so the most urgent
// actionable candidate sorts first and 0=none (no priority set) sorts last.
// Ties break on created_at ascending (oldest first), then identifier
// ascending, so candidate order is deterministic even when two rows tie on
// both.
//
// The "blocked_until <= now" predicate is auto-unblock layer 1's backoff gate:
// an issue re-queued after a transient failure carries blocked_until =
// retry-time + RecoverBackoffS, so it is invisible to every claim/peek until
// that window passes. This is the one candidate-selection rule shared by
// ClaimReady (ready -> running, filtered by lane_label), ClaimColumn (any
// downstream lane-entry column, not filtered by lane_label), and their
// read-only Peek* counterparts, so none of the four can drift apart from each
// other — the backoff gate applies uniformly to all of them.
func selectClaimCandidate(ctx context.Context, q queryRowContexter, now int64, whereExtra string, args ...any) (Issue, error) {
	query := `
		SELECT ` + claimCandidateColumns + `
		FROM issues
		WHERE claim_lock IS NULL AND blocked_until <= ? AND ` + whereExtra + `
		ORDER BY
			CASE priority WHEN 0 THEN 999999999 ELSE priority END ASC,
			created_at ASC,
			identifier ASC
		LIMIT 1
	`
	var issue Issue
	err := q.QueryRowContext(ctx, query, append([]any{now}, args...)...).Scan(
		&issue.ID, &issue.Identifier, &issue.Title, &issue.Description, &issue.LaneLabel, &issue.BoardStatus, &issue.ReworkCount, &issue.RecoverAttempts, &issue.BlockedUntil, &issue.Deps, &issue.Priority,
		&issue.BranchName, &issue.ClaimLock, &issue.ClaimExpires, &issue.UpdatedAt, &issue.LastSeen, &issue.CreatedAt,
	)
	return issue, err
}

// nextAttempt computes the next runs.attempt for issueID inside tx: the
// prior max (0 if none) plus one. attempt is global per issue, not per-lane
// (R5): a coder rework re-run continues the same attempt sequence as its
// original ready-claim run, and a later reviewer claim on the same
// issue shares it too, so cfg.MaxAttempts counts real dispatch attempts
// across every lane, restart, and retry -- shared by ClaimReady and
// ClaimColumn.
func nextAttempt(ctx context.Context, tx *sql.Tx, issueID string) (int, error) {
	var n int
	const q = `SELECT COALESCE(MAX(attempt), 0) + 1 FROM runs WHERE issue_id = ?`
	if err := tx.QueryRowContext(ctx, q, issueID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// insertClaimRun inserts run (already fully populated by the caller) inside
// tx for a freshly won claim -- shared by ClaimReady and ClaimColumn so both
// claim paths write runs identically.
func insertClaimRun(ctx context.Context, tx *sql.Tx, run Run) error {
	const q = `
		INSERT INTO runs (
			run_id, issue_id, lane, worker_pid, proc_started_at, status, started_at, heartbeat_at,
			attempt, turn_count, thread_id, result_json, error, tokens_in, tokens_out
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := tx.ExecContext(ctx, q,
		run.RunID, run.IssueID, run.Lane, run.WorkerPID, run.ProcStartedAt, run.Status, run.StartedAt, run.HeartbeatAt,
		run.Attempt, run.TurnCount, run.ThreadID, run.ResultJSON, run.Error, run.TokensIn, run.TokensOut,
	)
	return err
}

// insertClaimedEvent appends the "claimed" audit event for issueID/runID
// inside tx -- shared by ClaimReady and ClaimColumn.
func insertClaimedEvent(ctx context.Context, tx *sql.Tx, issueID, runID string, now int64) error {
	const q = `
		INSERT INTO events (ts, issue_id, run_id, kind, detail)
		VALUES (?, ?, ?, 'claimed', ?)
	`
	detail := fmt.Sprintf("claimed by run %s", runID)
	_, err := tx.ExecContext(ctx, q, now, sql.NullString{String: issueID, Valid: true}, sql.NullString{String: runID, Valid: true}, detail)
	return err
}

// ClaimReady atomically claims the single best 'ready' issue in laneLabel
// for runID.
//
// Candidate selection is selectClaimCandidate's rule: the most urgent
// actionable issue is claimed first, ties break oldest-created first then
// identifier ascending -- see its doc comment.
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
// On a win, ClaimReady inserts a fresh Run (attempt=prior max+1, turn_count=1,
// status="running") and appends a "claimed" event, all in the same
// transaction, then returns the resulting Claim.
func (s *Store) ClaimReady(ctx context.Context, laneLabel, runID string, now, ttl int64) (*Claim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claiming ready issue: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if already committed
	if err := ensureSchedulingRunning(ctx, tx); err != nil {
		if errors.Is(err, ErrSchedulingPaused) {
			return nil, err
		}
		return nil, fmt.Errorf("claiming ready issue: checking scheduling control: %w", err)
	}

	issue, err := selectClaimCandidate(ctx, tx, now, "board_status = 'ready' AND lane_label = ?", laneLabel)
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

	attempt, err := nextAttempt(ctx, tx, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("claiming ready issue %s: computing next attempt: %w", issue.ID, err)
	}

	run := Run{
		RunID:       runID,
		IssueID:     issue.ID,
		Lane:        laneLabel,
		Status:      "running",
		StartedAt:   now,
		HeartbeatAt: now,
		Attempt:     attempt,
		TurnCount:   1,
		ThreadID:    "",
	}
	if err := insertClaimRun(ctx, tx, run); err != nil {
		return nil, fmt.Errorf("claiming ready issue %s: inserting run %s: %w", issue.ID, run.RunID, err)
	}
	if err := insertClaimedEvent(ctx, tx, issue.ID, runID, now); err != nil {
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

// ClaimColumn atomically claims the single best UNCLAIMED card currently
// sitting in column (e.g. "review", "rework", "merging")
// for runID, and records the run as dispatched to lane.
//
// This is cross-lane claiming (design doc decision O amendment, Phase 3):
// the claim is the source of truth for what's in flight, not just a
// handoff-spawn, so restart recovery (dispatcher.RecoverOrphans) can find
// and reconcile every claimed card the same way regardless of which column
// it's claimed in. Unlike ClaimReady, the candidate pool is NOT filtered by
// lane_label: which lane runs is decided by which COLUMN the card is
// sitting in, not by the issue's own agent:<lane> label (which stays
// "coder" for the issue's entire lifetime -- see AGENTS.md's "bare lane"
// invariant). Candidate selection otherwise follows selectClaimCandidate's
// rule (most urgent, then oldest, then identifier).
//
// The claim is the same compare-and-swap discipline as ClaimReady, guarded
// by `board_status = column AND claim_lock IS NULL` -- but critically,
// board_status is NOT part of the SET clause: the card stays in column
// while the claimed lane runs. Only ClaimReady's ready->running claim moves
// a card's column on claim; every downstream lane-entry column is entered
// by a Transition (board.Next's action) before ClaimColumn ever sees it,
// and left by a later Transition once the claimed lane's worker resolves.
//
// attempt is computed the same issue-global way as ClaimReady (R5): a
// reviewer/git-operator claim on an issue continues the same attempt
// sequence as its coder run(s), not a separate per-lane counter.
func (s *Store) ClaimColumn(ctx context.Context, column, lane, runID string, now, ttl int64) (*Claim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claiming column %s: beginning tx: %w", column, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if already committed
	if err := ensureSchedulingRunning(ctx, tx); err != nil {
		if errors.Is(err, ErrSchedulingPaused) {
			return nil, err
		}
		return nil, fmt.Errorf("claiming column %s: checking scheduling control: %w", column, err)
	}

	issue, err := selectClaimCandidate(ctx, tx, now, "board_status = ?", column)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoReady
	}
	if err != nil {
		return nil, fmt.Errorf("claiming column %s: selecting candidate: %w", column, err)
	}

	claimExpires := now + ttl
	const casQ = `
		UPDATE issues SET claim_lock = ?, claim_expires = ?
		WHERE id = ? AND board_status = ? AND claim_lock IS NULL
	`
	res, err := tx.ExecContext(ctx, casQ, runID, claimExpires, issue.ID, column)
	if err != nil {
		return nil, fmt.Errorf("claiming column %s issue %s: cas update: %w", column, issue.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("claiming column %s issue %s: reading rows affected: %w", column, issue.ID, err)
	}
	if n != 1 {
		// Lost the race: another transaction claimed this row between our
		// SELECT and our UPDATE.
		return nil, ErrNoReady
	}

	attempt, err := nextAttempt(ctx, tx, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("claiming column %s issue %s: computing next attempt: %w", column, issue.ID, err)
	}

	run := Run{
		RunID:       runID,
		IssueID:     issue.ID,
		Lane:        lane,
		Status:      "running",
		StartedAt:   now,
		HeartbeatAt: now,
		Attempt:     attempt,
		TurnCount:   1,
		ThreadID:    "",
	}
	if err := insertClaimRun(ctx, tx, run); err != nil {
		return nil, fmt.Errorf("claiming column %s issue %s: inserting run %s: %w", column, issue.ID, run.RunID, err)
	}
	if err := insertClaimedEvent(ctx, tx, issue.ID, runID, now); err != nil {
		return nil, fmt.Errorf("claiming column %s issue %s: appending claimed event: %w", column, issue.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claiming column %s issue %s: committing: %w", column, issue.ID, err)
	}

	// issue.BoardStatus already equals column (selectClaimCandidate scanned
	// it straight off the matching row) and is intentionally left alone: the
	// card stays put while the claimed lane runs.
	issue.ClaimLock = sql.NullString{String: runID, Valid: true}
	issue.ClaimExpires = sql.NullInt64{Int64: claimExpires, Valid: true}

	return &Claim{Issue: issue, Run: run}, nil
}

// PeekReadyCandidate returns (read-only; claims nothing) the single best
// UNCLAIMED 'ready' issue in laneLabel that ClaimReady would claim right
// now, or ErrNoReady if none exist. It exists for the dispatcher's
// coder-pool fairness rule (R4): choosing whether to claim next from ready
// or from rework requires comparing their top candidates without
// committing to either first.
//
// This is safe without a transaction because Tick runs on a single
// goroutine with no concurrent writer inside one process (see AGENTS.md's
// "Tick is single-goroutine and race-free" invariant) and the dispatcher's
// singleton lock means no other process mutates the database underneath a
// running dispatcher either. Even if that ever changed, nothing unsafe
// happens: the caller's subsequent ClaimReady still CAS-guards the actual
// claim, so a peeked-then-stolen row just yields ErrNoReady there instead
// of a double-claim.
//
// now gates the same backoff window ClaimReady enforces (blocked_until <= now),
// so a peek never proposes a card the subsequent claim would then skip.
func (s *Store) PeekReadyCandidate(ctx context.Context, laneLabel string, now int64) (Issue, error) {
	issue, err := selectClaimCandidate(ctx, s.db, now, "board_status = 'ready' AND lane_label = ?", laneLabel)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, ErrNoReady
	}
	if err != nil {
		return Issue{}, fmt.Errorf("peeking ready candidate in lane %s: %w", laneLabel, err)
	}
	return issue, nil
}

// PeekColumnCandidate returns (read-only; claims nothing) the single best
// UNCLAIMED card currently sitting in column that ClaimColumn would claim
// right now, or ErrNoReady if none exist -- the downstream analogue of
// PeekReadyCandidate; see its doc comment for why this is safe without a
// transaction. now gates the backoff window exactly as PeekReadyCandidate's.
func (s *Store) PeekColumnCandidate(ctx context.Context, column string, now int64) (Issue, error) {
	issue, err := selectClaimCandidate(ctx, s.db, now, "board_status = ?", column)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, ErrNoReady
	}
	if err != nil {
		return Issue{}, fmt.Errorf("peeking candidate in column %s: %w", column, err)
	}
	return issue, nil
}

// Heartbeat extends the TTL of runID's claim: it pushes the owning issue's
// claim_expires to now+ttl and records the run's heartbeat_at. It errors if
// runID has no active claim (i.e. no issue currently locked by it). This
// works identically for a ClaimReady claim (running) or a ClaimColumn claim
// (review/rework/merging): both key off claim_lock, not
// board_status.
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

// ReleaseTargetColumn reports the board_status a claimed card returns to
// when its claim is released without a normal terminal outcome: a stale
// claim (ReleaseStaleClaims, below) or an orphaned run recovered at
// dispatcher restart (dispatcher.requeueOrphan). A card claimed while
// running -- the Coder lane's sole working column, entered only via
// ClaimReady -- returns to ready to be re-claimed. A card claimed in any
// downstream lane-entry column (review, rework, merging, all
// claimed via ClaimColumn) never left that column to begin with; only
// claim_lock changed, so releasing the claim leaves board_status unchanged.
// Both release paths call this one function so they cannot drift apart
// (design amendment: cross-lane claiming, R2).
func ReleaseTargetColumn(current string) string {
	if current == "running" {
		return "ready"
	}
	return current
}

// ReleaseStaleClaims requeues every claimed issue whose claim_expires has
// passed now, regardless of which column it's claimed in (a ClaimReady
// 'running' claim, or a ClaimColumn claim on review/rework/merging): it
// resets the issue's board_status per ReleaseTargetColumn
// (running -> ready; every downstream column stays put) with claim_lock and
// claim_expires cleared, marks its still-'running' run 'stale', and appends
// a "stale_release" event per released issue. It returns the number of
// issues released.
func (s *Store) ReleaseStaleClaims(ctx context.Context, now int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("releasing stale claims: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if already committed

	const selectQ = `
		SELECT id, claim_lock, board_status FROM issues
		WHERE claim_expires IS NOT NULL AND claim_expires < ?
	`
	rows, err := tx.QueryContext(ctx, selectQ, now)
	if err != nil {
		return 0, fmt.Errorf("releasing stale claims: selecting candidates: %w", err)
	}

	type staleIssue struct {
		id          string
		claimLock   sql.NullString
		boardStatus string
	}
	var stale []staleIssue
	for rows.Next() {
		var si staleIssue
		if err := rows.Scan(&si.id, &si.claimLock, &si.boardStatus); err != nil {
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
		UPDATE issues SET board_status = ?, claim_lock = NULL, claim_expires = NULL
		WHERE id = ?
	`
	const staleRunQ = `UPDATE runs SET status = 'stale' WHERE issue_id = ? AND status = 'running'`
	const eventQ = `
		INSERT INTO events (ts, issue_id, run_id, kind, detail)
		VALUES (?, ?, ?, 'stale_release', ?)
	`

	for _, si := range stale {
		target := ReleaseTargetColumn(si.boardStatus)
		if _, err := tx.ExecContext(ctx, requeueQ, target, si.id); err != nil {
			return 0, fmt.Errorf("releasing stale claims: requeuing issue %s: %w", si.id, err)
		}
		if _, err := tx.ExecContext(ctx, staleRunQ, si.id); err != nil {
			return 0, fmt.Errorf("releasing stale claims: marking runs stale for issue %s: %w", si.id, err)
		}
		detail := fmt.Sprintf("released stale claim %s (column %s -> %s)", si.claimLock.String, si.boardStatus, target)
		if _, err := tx.ExecContext(ctx, eventQ, now, sql.NullString{String: si.id, Valid: true}, si.claimLock, detail); err != nil {
			return 0, fmt.Errorf("releasing stale claims: appending stale_release event for issue %s: %w", si.id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("releasing stale claims: committing: %w", err)
	}

	return len(stale), nil
}
