package store

import (
	"context"
	"database/sql"
	"fmt"
)

const agentWorkspaceColumns = `
	owner_key, issue_id, run_id, provider, role, external_id,
	workspace_path, state, last_action, last_error, created_at, updated_at
`

// UpsertAgentWorkspace inserts the current provider-neutral lifecycle state
// for owner key. A repeated observation updates the workspace metadata and
// lifecycle fields while retaining when Clipse first recorded the owner.
func (s *Store) UpsertAgentWorkspace(ctx context.Context, workspace AgentWorkspace) error {
	const q = `
		INSERT INTO agent_workspaces (
			owner_key, issue_id, run_id, provider, role, external_id,
			workspace_path, state, last_action, last_error, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_key) DO UPDATE SET
			issue_id       = excluded.issue_id,
			run_id         = excluded.run_id,
			provider       = excluded.provider,
			role           = excluded.role,
			external_id    = excluded.external_id,
			workspace_path = excluded.workspace_path,
			state          = excluded.state,
			last_action    = excluded.last_action,
			last_error     = excluded.last_error,
			updated_at     = excluded.updated_at
	`
	_, err := s.db.ExecContext(ctx, q,
		workspace.OwnerKey,
		workspace.IssueID,
		workspace.RunID,
		workspace.Provider,
		workspace.Role,
		workspace.ExternalID,
		workspace.WorkspacePath,
		workspace.State,
		workspace.LastAction,
		workspace.LastError,
		workspace.CreatedAt,
		workspace.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upserting agent workspace %s: %w", workspace.OwnerKey, err)
	}
	return nil
}

// MarkWorkspaceCleanupPending durably schedules an owned workspace for
// provider deletion. The dispatcher drains these rows independently of the
// issue transition that requested cleanup.
func (s *Store) MarkWorkspaceCleanupPending(ctx context.Context, ownerKey string, now int64) error {
	const q = `
		UPDATE agent_workspaces
		SET state = ?, last_error = '', updated_at = ?
		WHERE owner_key = ?
	`
	return s.updateAgentWorkspace(ctx, q, ownerKey, "marking cleanup pending", WorkspaceCleanupPending, now, ownerKey)
}

// PendingWorkspaceCleanup returns all workspaces awaiting provider deletion.
// Cleanup failures stay in this state so later dispatcher ticks retry them.
func (s *Store) PendingWorkspaceCleanup(ctx context.Context) ([]AgentWorkspace, error) {
	const q = `SELECT ` + agentWorkspaceColumns + `
		FROM agent_workspaces
		WHERE state = ?
		ORDER BY updated_at, owner_key
	`
	rows, err := s.db.QueryContext(ctx, q, WorkspaceCleanupPending)
	if err != nil {
		return nil, fmt.Errorf("listing agent workspaces pending cleanup: %w", err)
	}
	return scanAgentWorkspaces(rows, "pending cleanup")
}

// RecordWorkspaceCleanupError records a failed provider delete without
// removing the workspace from the cleanup queue.
func (s *Store) RecordWorkspaceCleanupError(ctx context.Context, ownerKey, lastError string, now int64) error {
	const q = `
		UPDATE agent_workspaces
		SET state = ?, last_error = ?, updated_at = ?
		WHERE owner_key = ?
	`
	return s.updateAgentWorkspace(ctx, q, ownerKey, "recording cleanup error", WorkspaceCleanupPending, lastError, now, ownerKey)
}

// MarkWorkspaceDeleted records a successful idempotent provider deletion.
func (s *Store) MarkWorkspaceDeleted(ctx context.Context, ownerKey string, now int64) error {
	const q = `
		UPDATE agent_workspaces
		SET state = ?, last_action = 'delete', last_error = '', updated_at = ?
		WHERE owner_key = ?
	`
	return s.updateAgentWorkspace(ctx, q, ownerKey, "marking deleted", WorkspaceDeleted, now, ownerKey)
}

// MarkWorkspaceReconcileNeedsInput durably removes an ambiguous workspace
// identity from automatic cleanup. Reconciliation uses this when the provider
// reports duplicate coder sandboxes for one owner key; state=error keeps the
// row out of the delete queue while the event/detail tells an operator which
// provider objects require manual resolution.
func (s *Store) MarkWorkspaceReconcileNeedsInput(ctx context.Context, ownerKey, detail string, now int64) error {
	const q = `
		UPDATE agent_workspaces
		SET state = ?, last_action = 'reconcile_needs_input', last_error = ?, updated_at = ?
		WHERE owner_key = ?
	`
	return s.updateAgentWorkspace(ctx, q, ownerKey, "marking reconciliation needs input", WorkspaceError, detail, now, ownerKey)
}

// AgentWorkspacesByIssue returns every recorded workspace for issueID in
// stable creation order.
func (s *Store) AgentWorkspacesByIssue(ctx context.Context, issueID string) ([]AgentWorkspace, error) {
	const q = `SELECT ` + agentWorkspaceColumns + `
		FROM agent_workspaces
		WHERE issue_id = ?
		ORDER BY created_at, owner_key
	`
	rows, err := s.db.QueryContext(ctx, q, issueID)
	if err != nil {
		return nil, fmt.Errorf("listing agent workspaces for issue %s: %w", issueID, err)
	}
	return scanAgentWorkspaces(rows, "by issue")
}

// ListAgentWorkspaces returns every durable workspace row. Startup
// reconciliation compares this inventory with the provider's scoped List
// result so a remote workspace that disappeared while Clipse was stopped is
// recorded as deleted rather than left active forever.
func (s *Store) ListAgentWorkspaces(ctx context.Context) ([]AgentWorkspace, error) {
	const q = `SELECT ` + agentWorkspaceColumns + `
		FROM agent_workspaces
		ORDER BY created_at, owner_key
	`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing agent workspaces: %w", err)
	}
	return scanAgentWorkspaces(rows, "all")
}

func (s *Store) updateAgentWorkspace(ctx context.Context, query, ownerKey, action string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s for agent workspace %s: %w", action, ownerKey, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s for agent workspace %s: reading rows affected: %w", action, ownerKey, err)
	}
	if rows == 0 {
		return fmt.Errorf("%s for agent workspace %s: no such workspace", action, ownerKey)
	}
	return nil
}

func scanAgentWorkspaces(rows *sql.Rows, scope string) ([]AgentWorkspace, error) {
	defer rows.Close()

	var workspaces []AgentWorkspace
	for rows.Next() {
		var workspace AgentWorkspace
		if err := rows.Scan(
			&workspace.OwnerKey,
			&workspace.IssueID,
			&workspace.RunID,
			&workspace.Provider,
			&workspace.Role,
			&workspace.ExternalID,
			&workspace.WorkspacePath,
			&workspace.State,
			&workspace.LastAction,
			&workspace.LastError,
			&workspace.CreatedAt,
			&workspace.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning agent workspace row (%s): %w", scope, err)
		}
		workspaces = append(workspaces, workspace)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating agent workspace rows (%s): %w", scope, err)
	}
	return workspaces, nil
}
