package store_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/store"
)

func TestAgentWorkspace_CleanupRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws := store.AgentWorkspace{
		OwnerKey:      "daytona:xlyk/clipse:coder:issue-1",
		IssueID:       "issue-1",
		Provider:      "daytona",
		Role:          "coder",
		ExternalID:    "sb-1",
		WorkspacePath: "/home/daytona/workspace/clipse",
		State:         store.WorkspaceActive,
		LastAction:    "create",
		CreatedAt:     10,
		UpdatedAt:     10,
	}
	if err := s.UpsertAgentWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorkspaceCleanupPending(ctx, ws.OwnerKey, 20); err != nil {
		t.Fatal(err)
	}
	rows, err := s.PendingWorkspaceCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ExternalID != "sb-1" {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].State != store.WorkspaceCleanupPending || rows[0].UpdatedAt != 20 {
		t.Fatalf("pending row = %#v", rows[0])
	}
}

func TestAgentWorkspace_CleanupErrorRemainsPending(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws := agentWorkspaceFixture("daytona:xlyk/clipse:reviewer:issue-1:run-1", "issue-1", "run-1", 10)
	if err := s.UpsertAgentWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorkspaceCleanupPending(ctx, ws.OwnerKey, 20); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordWorkspaceCleanupError(ctx, ws.OwnerKey, "provider unavailable", 30); err != nil {
		t.Fatal(err)
	}

	rows, err := s.PendingWorkspaceCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("pending rows = %#v, want one", rows)
	}
	if rows[0].State != store.WorkspaceCleanupPending || rows[0].LastAction != "create" || rows[0].LastError != "provider unavailable" || rows[0].UpdatedAt != 30 {
		t.Fatalf("pending row after cleanup error = %#v", rows[0])
	}
}

func TestAgentWorkspace_UpsertPreservesCreatedAtAndListsByIssue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	coder := agentWorkspaceFixture("daytona:xlyk/clipse:coder:issue-1", "issue-1", "", 10)
	reviewer := agentWorkspaceFixture("daytona:xlyk/clipse:reviewer:issue-1:run-1", "issue-1", "run-1", 11)
	other := agentWorkspaceFixture("daytona:xlyk/clipse:coder:issue-2", "issue-2", "", 12)
	for _, ws := range []store.AgentWorkspace{reviewer, other, coder} {
		if err := s.UpsertAgentWorkspace(ctx, ws); err != nil {
			t.Fatalf("UpsertAgentWorkspace(%q): %v", ws.OwnerKey, err)
		}
	}

	coder.RunID = "run-2"
	coder.ExternalID = "sb-recreated"
	coder.WorkspacePath = "/workspace/recreated"
	coder.State = store.WorkspaceStopped
	coder.LastAction = "stop"
	coder.LastError = "idle timeout"
	coder.CreatedAt = 99
	coder.UpdatedAt = 20
	if err := s.UpsertAgentWorkspace(ctx, coder); err != nil {
		t.Fatal(err)
	}

	rows, err := s.AgentWorkspacesByIssue(ctx, "issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %#v, want two", rows)
	}
	if rows[0].OwnerKey != coder.OwnerKey || rows[1].OwnerKey != reviewer.OwnerKey {
		t.Fatalf("owner order = [%q, %q], want coder then reviewer", rows[0].OwnerKey, rows[1].OwnerKey)
	}
	got := rows[0]
	if got.RunID != "run-2" || got.ExternalID != "sb-recreated" || got.WorkspacePath != "/workspace/recreated" || got.State != store.WorkspaceStopped || got.LastAction != "stop" || got.LastError != "idle timeout" || got.UpdatedAt != 20 {
		t.Fatalf("updated coder row = %#v", got)
	}
	if got.CreatedAt != 10 {
		t.Fatalf("created_at = %d, want original 10", got.CreatedAt)
	}
}

func TestAgentWorkspace_MarkDeletedRemovesFromPending(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws := agentWorkspaceFixture("daytona:xlyk/clipse:coder:issue-1", "issue-1", "", 10)
	if err := s.UpsertAgentWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorkspaceCleanupPending(ctx, ws.OwnerKey, 20); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordWorkspaceCleanupError(ctx, ws.OwnerKey, "temporary", 30); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorkspaceDeleted(ctx, ws.OwnerKey, 40); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingWorkspaceCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %#v, want none", pending)
	}
	rows, err := s.AgentWorkspacesByIssue(ctx, ws.IssueID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != store.WorkspaceDeleted || rows[0].LastError != "" || rows[0].UpdatedAt != 40 {
		t.Fatalf("deleted row = %#v", rows)
	}
}

func TestAgentWorkspace_LifecycleStateValues(t *testing.T) {
	got := []store.WorkspaceState{
		store.WorkspaceActive,
		store.WorkspaceStopped,
		store.WorkspaceCleanupPending,
		store.WorkspaceDeleted,
		store.WorkspaceError,
	}
	want := []store.WorkspaceState{"active", "stopped", "cleanup_pending", "deleted", "error"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("state[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func agentWorkspaceFixture(ownerKey, issueID, runID string, now int64) store.AgentWorkspace {
	return store.AgentWorkspace{
		OwnerKey:      ownerKey,
		IssueID:       issueID,
		RunID:         runID,
		Provider:      "daytona",
		Role:          "coder",
		ExternalID:    "sb-1",
		WorkspacePath: "/home/daytona/workspace/clipse",
		State:         store.WorkspaceActive,
		LastAction:    "create",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}
