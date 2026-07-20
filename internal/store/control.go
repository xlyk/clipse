package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrDrainInProgress         = errors.New("dispatcher drain is in progress")
	ErrDrainNotTarget          = errors.New("dispatcher instance is not the drain target")
	ErrDispatcherInstanceStale = errors.New("dispatcher instance is not active")
)

func validSchedulingMode(mode SchedulingMode) bool {
	return mode == SchedulingRunning || mode == SchedulingPaused
}

func validObservedMode(mode ObservedMode) bool {
	return mode == ObservedRunning || mode == ObservedPaused || mode == ObservedDraining
}

func readDispatcherControl(ctx context.Context, q queryRowContexter) (DispatcherControl, error) {
	const query = `
		SELECT desired_mode, observed_mode, request_id, requested_at, acknowledged_at,
			active_instance_id, active_pid, instance_started_at, heartbeat_at,
			drain_target_instance_id, drain_strict, drained_at
		FROM dispatcher_control WHERE id = 1
	`
	var control DispatcherControl
	if err := q.QueryRowContext(ctx, query).Scan(
		&control.DesiredMode,
		&control.ObservedMode,
		&control.RequestID,
		&control.RequestedAt,
		&control.AcknowledgedAt,
		&control.ActiveInstanceID,
		&control.ActivePID,
		&control.InstanceStartedAt,
		&control.HeartbeatAt,
		&control.DrainTargetInstanceID,
		&control.DrainStrict,
		&control.DrainedAt,
	); err != nil {
		return DispatcherControl{}, fmt.Errorf("reading dispatcher control: %w", err)
	}
	if !validSchedulingMode(control.DesiredMode) {
		return DispatcherControl{}, fmt.Errorf("reading dispatcher control: invalid desired mode %q", control.DesiredMode)
	}
	if !validObservedMode(control.ObservedMode) {
		return DispatcherControl{}, fmt.Errorf("reading dispatcher control: invalid observed mode %q", control.ObservedMode)
	}
	return control, nil
}

// ReadDispatcherControl returns the board's authoritative scheduling state.
func (s *Store) ReadDispatcherControl(ctx context.Context) (DispatcherControl, error) {
	return readDispatcherControl(ctx, s.db)
}

func insertControlEvent(ctx context.Context, tx *sql.Tx, now int64, kind, detail string) error {
	const query = `INSERT INTO events (ts, issue_id, run_id, kind, detail) VALUES (?, NULL, NULL, ?, ?)`
	if _, err := tx.ExecContext(ctx, query, now, kind, detail); err != nil {
		return fmt.Errorf("appending %s event: %w", kind, err)
	}
	return nil
}

// RequestPause commits the hard scheduling barrier. Because this write and
// every claim use BEGIN IMMEDIATE, SQLite establishes one exact winner.
func (s *Store) RequestPause(ctx context.Context, requestID string, now int64) error {
	if requestID == "" {
		return fmt.Errorf("requesting dispatcher pause: request id is empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("requesting dispatcher pause: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `
		UPDATE dispatcher_control
		SET desired_mode = ?, request_id = ?, requested_at = ?, acknowledged_at = 0
		WHERE id = 1
	`, SchedulingPaused, requestID, now); err != nil {
		return fmt.Errorf("requesting dispatcher pause: updating control: %w", err)
	}
	if err := insertControlEvent(ctx, tx, now, "dispatcher_pause_requested", fmt.Sprintf("request_id=%s", requestID)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("requesting dispatcher pause: committing: %w", err)
	}
	return nil
}

// RequestDrain pauses claims and targets the dispatcher instance registered
// at the same serialized point. An empty target means no daemon was active.
func (s *Store) RequestDrain(ctx context.Context, requestID string, now int64, strict bool) error {
	if requestID == "" {
		return fmt.Errorf("requesting dispatcher drain: request id is empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("requesting dispatcher drain: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	control, err := readDispatcherControl(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE dispatcher_control
		SET desired_mode = ?, request_id = ?, requested_at = ?, acknowledged_at = 0,
			drain_target_instance_id = ?, drain_strict = ?, drained_at = 0
		WHERE id = 1
	`, SchedulingPaused, requestID, now, control.ActiveInstanceID, strict); err != nil {
		return fmt.Errorf("requesting dispatcher drain: updating control: %w", err)
	}
	detail := fmt.Sprintf("request_id=%s target_instance_id=%s strict=%t", requestID, control.ActiveInstanceID, strict)
	if err := insertControlEvent(ctx, tx, now, "dispatcher_drain_requested", detail); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("requesting dispatcher drain: committing: %w", err)
	}
	return nil
}

// RequestResume reopens scheduling. An unfinished targeted drain requires an
// explicit cancellation so an accidental resume cannot recreate the race.
func (s *Store) RequestResume(ctx context.Context, requestID string, now int64, cancelDrain bool) error {
	if requestID == "" {
		return fmt.Errorf("requesting dispatcher resume: request id is empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("requesting dispatcher resume: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	control, err := readDispatcherControl(ctx, tx)
	if err != nil {
		return err
	}
	if control.DrainTargetInstanceID != "" && control.DrainedAt == 0 && !cancelDrain {
		return ErrDrainInProgress
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE dispatcher_control
		SET desired_mode = ?, request_id = ?, requested_at = ?, acknowledged_at = 0,
			drain_target_instance_id = '', drain_strict = 0, drained_at = 0
		WHERE id = 1
	`, SchedulingRunning, requestID, now); err != nil {
		return fmt.Errorf("requesting dispatcher resume: updating control: %w", err)
	}
	if err := insertControlEvent(ctx, tx, now, "dispatcher_resumed", fmt.Sprintf("request_id=%s cancel_drain=%t", requestID, cancelDrain)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("requesting dispatcher resume: committing: %w", err)
	}
	return nil
}

// RegisterDispatcher establishes the current daemon identity without
// changing desired_mode. A replacement repairs a stale targeted drain and
// deliberately remains paused.
func (s *Store) RegisterDispatcher(ctx context.Context, instanceID string, pid int, now int64) (DispatcherRegistration, error) {
	if instanceID == "" || pid <= 0 {
		return DispatcherRegistration{}, fmt.Errorf("registering dispatcher: invalid identity %q pid=%d", instanceID, pid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DispatcherRegistration{}, fmt.Errorf("registering dispatcher: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	control, err := readDispatcherControl(ctx, tx)
	if err != nil {
		return DispatcherRegistration{}, err
	}
	interrupted := control.DrainTargetInstanceID != "" && control.DrainTargetInstanceID != instanceID && control.DrainedAt == 0
	if interrupted {
		priorTarget := control.DrainTargetInstanceID
		control.DrainTargetInstanceID = ""
		control.DesiredMode = SchedulingPaused
		if err := insertControlEvent(ctx, tx, now, "dispatcher_drain_interrupted", fmt.Sprintf("request_id=%s prior_target=%s replacement=%s", control.RequestID, priorTarget, instanceID)); err != nil {
			return DispatcherRegistration{}, err
		}
	}
	observed := ObservedRunning
	if control.DesiredMode == SchedulingPaused {
		observed = ObservedPaused
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE dispatcher_control
		SET desired_mode = ?, observed_mode = ?, active_instance_id = ?, active_pid = ?,
			instance_started_at = ?, heartbeat_at = ?, drain_target_instance_id = ?,
			acknowledged_at = CASE WHEN requested_at > 0 THEN ? ELSE acknowledged_at END
		WHERE id = 1
	`, control.DesiredMode, observed, instanceID, pid, now, now, control.DrainTargetInstanceID, now); err != nil {
		return DispatcherRegistration{}, fmt.Errorf("registering dispatcher: updating control: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DispatcherRegistration{}, fmt.Errorf("registering dispatcher: committing: %w", err)
	}
	registered, err := s.ReadDispatcherControl(ctx)
	if err != nil {
		return DispatcherRegistration{}, err
	}
	return DispatcherRegistration{Control: registered, DrainInterrupted: interrupted}, nil
}

// HeartbeatDispatcher acknowledges a control request and refreshes daemon
// liveness. Only the currently registered instance may write it.
func (s *Store) HeartbeatDispatcher(ctx context.Context, instanceID string, observed ObservedMode, now int64) error {
	if !validObservedMode(observed) {
		return fmt.Errorf("heartbeating dispatcher: invalid observed mode %q", observed)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("heartbeating dispatcher: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	control, err := readDispatcherControl(ctx, tx)
	if err != nil {
		return err
	}
	if control.ActiveInstanceID != instanceID {
		return ErrDispatcherInstanceStale
	}
	firstPauseAck := control.DesiredMode == SchedulingPaused && control.AcknowledgedAt == 0
	if _, err := tx.ExecContext(ctx, `
		UPDATE dispatcher_control
		SET observed_mode = ?, heartbeat_at = ?,
			acknowledged_at = CASE WHEN acknowledged_at = 0 THEN ? ELSE acknowledged_at END
		WHERE id = 1 AND active_instance_id = ?
	`, observed, now, now, instanceID); err != nil {
		return fmt.Errorf("heartbeating dispatcher: updating control: %w", err)
	}
	if firstPauseAck {
		if err := insertControlEvent(ctx, tx, now, "dispatcher_paused", fmt.Sprintf("request_id=%s instance_id=%s", control.RequestID, instanceID)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("heartbeating dispatcher: committing: %w", err)
	}
	return nil
}

// CompleteDrain records execution quiescence and clears the active identity
// before the targeted daemon exits normally.
func (s *Store) CompleteDrain(ctx context.Context, instanceID string, now int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("completing dispatcher drain: beginning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	control, err := readDispatcherControl(ctx, tx)
	if err != nil {
		return err
	}
	if control.DrainTargetInstanceID != instanceID || control.ActiveInstanceID != instanceID {
		return ErrDrainNotTarget
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE dispatcher_control
		SET desired_mode = ?, observed_mode = ?, acknowledged_at = ?, drained_at = ?,
			drain_target_instance_id = '', active_instance_id = '', active_pid = 0,
			instance_started_at = 0, heartbeat_at = ?
		WHERE id = 1
	`, SchedulingPaused, ObservedPaused, now, now, now); err != nil {
		return fmt.Errorf("completing dispatcher drain: updating control: %w", err)
	}
	if err := insertControlEvent(ctx, tx, now, "dispatcher_drained", fmt.Sprintf("request_id=%s instance_id=%s", control.RequestID, instanceID)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("completing dispatcher drain: committing: %w", err)
	}
	return nil
}

// UnregisterDispatcher clears liveness metadata on an ordinary clean exit.
// It deliberately leaves an unfinished drain target intact so a replacement
// can record dispatcher_drain_interrupted before orphan recovery.
func (s *Store) UnregisterDispatcher(ctx context.Context, instanceID string, now int64) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE dispatcher_control
		SET active_instance_id = '', active_pid = 0, instance_started_at = 0, heartbeat_at = ?
		WHERE id = 1 AND active_instance_id = ?
	`, now, instanceID); err != nil {
		return fmt.Errorf("unregistering dispatcher %s: %w", instanceID, err)
	}
	return nil
}

// DispatcherRuntimeCounts reads the durable execution and side-effect
// backlog used by drain completion and status rendering.
func (s *Store) DispatcherRuntimeCounts(ctx context.Context) (DispatcherRuntimeCounts, error) {
	var counts DispatcherRuntimeCounts
	queries := []struct {
		name  string
		query string
		dest  *int
	}{
		{name: "active runs", query: `SELECT COUNT(*) FROM runs WHERE status = 'running'`, dest: &counts.ActiveRuns},
		{name: "pending outbox", query: `SELECT COUNT(*) FROM linear_writes WHERE status = 'pending'`, dest: &counts.PendingOutbox},
		{name: "pending cleanup", query: `SELECT COUNT(*) FROM agent_workspaces WHERE state = 'cleanup_pending'`, dest: &counts.PendingCleanup},
	}
	for _, item := range queries {
		if err := s.db.QueryRowContext(ctx, item.query).Scan(item.dest); err != nil {
			return DispatcherRuntimeCounts{}, fmt.Errorf("counting dispatcher %s: %w", item.name, err)
		}
	}
	return counts, nil
}
