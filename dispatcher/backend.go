package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xlyk/clipse/internal/backend"
	"github.com/xlyk/clipse/internal/store"
)

const workspaceReconcileNeedsInput = "workspace_reconcile_needs_input"
const workspaceReconcileNeedsInputAction = "reconcile_needs_input"

// drainWorkspaceCleanup retries each configured-provider cleanup_pending row
// once per tick. Remote Delete and local Remove failures are diagnostic state,
// not board transitions: the row stays pending with a sanitized error and the
// tick continues through its later phases, including outbox drain.
func (d *Dispatcher) drainWorkspaceCleanup(ctx context.Context) error {
	provider := d.cfg.AgentBackend.Type
	if provider != "daytona" && provider != "local" {
		return nil
	}
	pending, err := d.store.PendingWorkspaceCleanup(ctx)
	if err != nil {
		return fmt.Errorf("listing pending workspace cleanup: %w", err)
	}
	ownedPending := pending[:0]
	for _, workspace := range pending {
		if workspace.Provider == provider {
			ownedPending = append(ownedPending, workspace)
		}
	}
	if len(ownedPending) == 0 {
		return nil
	}
	var repoSlug string
	if provider == "daytona" {
		if d.backend == nil {
			return errors.New("remote workspace backend manager is not configured")
		}
		_, repoSlug, err = backend.CanonicalGitHubRemote(d.cfg.Repo.Remote)
		if err != nil {
			return fmt.Errorf("resolving repository for workspace cleanup: %w", err)
		}
	}
	for _, workspace := range ownedPending {
		var cleanupErr error
		var message string
		if provider == "local" {
			issue, err := d.store.GetIssue(ctx, workspace.IssueID)
			if err != nil {
				return fmt.Errorf("loading issue %s for local workspace cleanup: %w", workspace.IssueID, err)
			}
			cleanupErr = d.ws.Remove(*issue)
			message = "local workspace cleanup failed"
		} else {
			remote := backend.Workspace{
				Provider: workspace.Provider, OwnerKey: workspace.OwnerKey, ExternalID: workspace.ExternalID,
				WorkspacePath: workspace.WorkspacePath, State: backend.WorkspaceState(workspace.State),
				Role: workspace.Role, IssueID: workspace.IssueID, RunID: workspace.RunID,
				RepoSlug: repoSlug, Target: d.cfg.AgentBackend.Daytona.Target,
			}
			cleanupErr = d.backend.Delete(ctx, remote)
			message = sanitizedWorkspaceCleanupError(cleanupErr)
		}
		if cleanupErr != nil {
			if recordErr := d.store.RecordWorkspaceCleanupError(ctx, workspace.OwnerKey, message, d.now()); recordErr != nil {
				return fmt.Errorf("recording cleanup failure for %s: %w", workspace.OwnerKey, recordErr)
			}
			d.logger.Warn("agent workspace cleanup failed; retained for retry",
				"owner_key", workspace.OwnerKey, "issue_id", workspace.IssueID, "error", message)
			continue
		}
		if err := d.store.MarkWorkspaceDeleted(ctx, workspace.OwnerKey, d.now()); err != nil {
			return fmt.Errorf("marking cleaned workspace %s deleted: %w", workspace.OwnerKey, err)
		}
	}
	return nil
}

func sanitizedWorkspaceCleanupError(err error) string {
	var actionErr *backend.ActionError
	if errors.As(err, &actionErr) && actionErr.Msg != "" {
		return actionErr.Msg
	}
	return "workspace cleanup failed"
}

// reconcileAgentWorkspaces runs once before orphan-run recovery. It compares
// Clipse-scoped provider inventory with SQLite, restoring safe persistent
// coder rows and scheduling provable orphans for the normal durable cleanup
// drain. An open run always wins; duplicate coder sandboxes require a human
// decision and are never auto-deleted.
func (d *Dispatcher) reconcileAgentWorkspaces(ctx context.Context) error {
	if d.cfg.AgentBackend.Type != "daytona" {
		return nil
	}
	if d.backend == nil {
		return errors.New("remote workspace backend manager is not configured")
	}
	_, repoSlug, err := backend.CanonicalGitHubRemote(d.cfg.Repo.Remote)
	if err != nil {
		return fmt.Errorf("resolving repository for workspace reconciliation: %w", err)
	}
	remoteWorkspaces, err := d.backend.List(ctx, backend.ListRequest{
		Provider: "daytona", RepoSlug: repoSlug, Target: d.cfg.AgentBackend.Daytona.Target,
	})
	if err != nil {
		return fmt.Errorf("listing remote agent workspaces: %w", err)
	}
	localWorkspaces, err := d.store.ListAgentWorkspaces(ctx)
	if err != nil {
		return fmt.Errorf("listing durable agent workspaces: %w", err)
	}
	snapshot, err := d.store.ReadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("reading issues for workspace reconciliation: %w", err)
	}
	openRuns, err := d.store.ListOpenRuns(ctx)
	if err != nil {
		return fmt.Errorf("listing open runs for workspace reconciliation: %w", err)
	}

	issues := make(map[string]store.Issue, len(snapshot.Issues))
	for _, issue := range snapshot.Issues {
		issues[issue.ID] = issue.Issue
	}

	type observedWorkspace struct {
		remote backend.Workspace
		role   string
		issue  string
		run    string
		valid  bool
	}
	observed := make([]observedWorkspace, 0, len(remoteWorkspaces))
	remoteOwners := make(map[string]struct{}, len(remoteWorkspaces))
	coderCounts := make(map[string]int)
	coderExternalIDs := make(map[string][]string)
	for _, remote := range remoteWorkspaces {
		remoteOwners[remote.OwnerKey] = struct{}{}
		role, issueID, runID, ok := parseWorkspaceOwner(remote.OwnerKey, "daytona", repoSlug)
		observed = append(observed, observedWorkspace{remote: remote, role: role, issue: issueID, run: runID, valid: ok})
		if ok && role == "coder" {
			coderCounts[issueID]++
			coderExternalIDs[issueID] = append(coderExternalIDs[issueID], remote.ExternalID)
		}
	}

	now := d.now()
	duplicateEvents := make(map[string]bool)
	localByOwner := make(map[string]store.AgentWorkspace, len(localWorkspaces))
	for _, workspace := range localWorkspaces {
		if workspace.Provider == "daytona" {
			localByOwner[workspace.OwnerKey] = workspace
		}
	}
	for _, item := range observed {
		if !item.valid {
			if err := d.appendWorkspaceNeedsInput(ctx, "", "remote workspace has an unrecognized owner key; manual reconciliation required", now); err != nil {
				return err
			}
			continue
		}
		if item.role == "coder" && coderCounts[item.issue] > 1 {
			if !duplicateEvents[item.issue] {
				ids := append([]string(nil), coderExternalIDs[item.issue]...)
				sort.Strings(ids)
				detail := fmt.Sprintf("multiple coder workspaces found for issue %s (sandbox ids: %s); manual cleanup required", item.issue, strings.Join(ids, ", "))
				if local, exists := localByOwner[item.remote.OwnerKey]; exists {
					if err := d.store.MarkWorkspaceReconcileNeedsInput(ctx, local.OwnerKey, detail, now); err != nil {
						return fmt.Errorf("neutralizing duplicate workspace cleanup for %s: %w", local.OwnerKey, err)
					}
				}
				if err := d.appendWorkspaceNeedsInput(ctx, item.issue, detail, now); err != nil {
					return err
				}
				duplicateEvents[item.issue] = true
			}
			continue
		}

		remote := item.remote
		if remote.Provider == "" {
			remote.Provider = "daytona"
		}
		workspace := store.AgentWorkspace{
			OwnerKey: remote.OwnerKey, IssueID: item.issue, RunID: item.run, Provider: remote.Provider, Role: item.role,
			ExternalID: remote.ExternalID, WorkspacePath: remote.WorkspacePath, State: store.WorkspaceState(remote.State),
			LastAction: "reconcile", CreatedAt: now, UpdatedAt: now,
		}
		if err := d.store.UpsertAgentWorkspace(ctx, workspace); err != nil {
			return fmt.Errorf("restoring remote workspace %s: %w", remote.OwnerKey, err)
		}
		if workspace.State == store.WorkspaceDeleted {
			continue
		}
		if workspaceHasOpenRun(workspace, openRuns) {
			continue
		}
		issue, knownIssue := issues[item.issue]
		terminalIssue := knownIssue && terminalStatuses[issue.BoardStatus]
		providerAlreadyCleaning := workspace.State == store.WorkspaceCleanupPending || workspace.State == store.WorkspaceError
		if !knownIssue || terminalIssue || item.role == "reviewer" || providerAlreadyCleaning {
			if err := d.store.MarkWorkspaceCleanupPending(ctx, workspace.OwnerKey, now); err != nil {
				return fmt.Errorf("scheduling reconciled workspace %s cleanup: %w", workspace.OwnerKey, err)
			}
		}
	}

	for _, local := range localWorkspaces {
		if local.Provider != "daytona" || local.State == store.WorkspaceDeleted {
			continue
		}
		if _, present := remoteOwners[local.OwnerKey]; present {
			continue
		}
		if err := d.store.MarkWorkspaceDeleted(ctx, local.OwnerKey, now); err != nil {
			return fmt.Errorf("marking missing remote workspace %s deleted: %w", local.OwnerKey, err)
		}
	}
	return nil
}

// scheduleRecoveredWorkspaceCleanup closes the narrow gap between startup
// inventory and orphan recovery. The initial remote reconciliation must
// preserve a workspace while its run is still open; after RecoverOrphans has
// closed that debris, this local-state-only pass can safely queue a terminal
// coder workspace or the old run-scoped reviewer workspace without issuing a
// second provider List call. The next normal Tick drain performs deletion.
func (d *Dispatcher) scheduleRecoveredWorkspaceCleanup(ctx context.Context) error {
	provider := d.cfg.AgentBackend.Type
	if provider != "daytona" && provider != "local" {
		return nil
	}
	workspaces, err := d.store.ListAgentWorkspaces(ctx)
	if err != nil {
		return fmt.Errorf("listing workspaces after orphan recovery: %w", err)
	}
	snapshot, err := d.store.ReadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("reading issues after orphan recovery: %w", err)
	}
	openRuns, err := d.store.ListOpenRuns(ctx)
	if err != nil {
		return fmt.Errorf("listing open runs after orphan recovery: %w", err)
	}
	issues := make(map[string]store.Issue, len(snapshot.Issues))
	for _, issue := range snapshot.Issues {
		issues[issue.ID] = issue.Issue
	}
	coderCounts := make(map[string]int)
	for _, workspace := range workspaces {
		if workspace.Provider == provider && workspace.Role == "coder" && workspace.State != store.WorkspaceDeleted && workspace.LastAction != workspaceReconcileNeedsInputAction {
			coderCounts[workspace.IssueID]++
		}
	}
	now := d.now()
	duplicateEvents := make(map[string]bool)
	for _, workspace := range workspaces {
		if workspace.Provider != provider {
			continue
		}
		issue, known := issues[workspace.IssueID]
		if workspace.State == store.WorkspaceDeleted || workspace.State == store.WorkspaceCleanupPending || workspace.LastAction == workspaceReconcileNeedsInputAction {
			continue
		}
		if workspaceHasOpenRun(workspace, openRuns) {
			continue
		}
		if provider == "local" {
			if workspace.Role == "coder" && known && terminalStatuses[issue.BoardStatus] {
				if err := d.store.MarkWorkspaceCleanupPending(ctx, workspace.OwnerKey, now); err != nil {
					return fmt.Errorf("scheduling recovered local workspace %s cleanup: %w", workspace.OwnerKey, err)
				}
			}
			continue
		}
		if workspace.Role == "reviewer" {
			if err := d.store.MarkWorkspaceCleanupPending(ctx, workspace.OwnerKey, now); err != nil {
				return fmt.Errorf("scheduling recovered reviewer workspace %s cleanup: %w", workspace.OwnerKey, err)
			}
			continue
		}
		if workspace.Role != "coder" || !known || !terminalStatuses[issue.BoardStatus] {
			continue
		}
		if coderCounts[workspace.IssueID] != 1 {
			if !duplicateEvents[workspace.IssueID] {
				detail := fmt.Sprintf("multiple coder workspaces found for terminal issue %s after orphan recovery; manual cleanup required", workspace.IssueID)
				if err := d.appendWorkspaceNeedsInput(ctx, workspace.IssueID, detail, now); err != nil {
					return err
				}
				duplicateEvents[workspace.IssueID] = true
			}
			continue
		}
		if err := d.store.MarkWorkspaceCleanupPending(ctx, workspace.OwnerKey, now); err != nil {
			return fmt.Errorf("scheduling recovered terminal workspace %s cleanup: %w", workspace.OwnerKey, err)
		}
	}
	return nil
}

func parseWorkspaceOwner(ownerKey, provider, repoSlug string) (role, issueID, runID string, ok bool) {
	remainder, found := strings.CutPrefix(ownerKey, provider+":"+repoSlug+":")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(remainder, ":")
	switch {
	case len(parts) == 2 && parts[0] == "coder" && parts[1] != "":
		return parts[0], parts[1], "", true
	case len(parts) == 3 && parts[0] == "reviewer" && parts[1] != "" && parts[2] != "":
		return parts[0], parts[1], parts[2], true
	default:
		return "", "", "", false
	}
}

func workspaceHasOpenRun(workspace store.AgentWorkspace, openRuns []store.Run) bool {
	for _, run := range openRuns {
		if run.IssueID != workspace.IssueID {
			continue
		}
		if workspace.Role == "coder" && run.Lane == "coder" {
			return true
		}
		if workspace.Role == "reviewer" && run.Lane == "reviewer" && run.RunID == workspace.RunID {
			return true
		}
	}
	return false
}

func (d *Dispatcher) appendWorkspaceNeedsInput(ctx context.Context, issueID, detail string, now int64) error {
	event := store.Event{Ts: now, Kind: workspaceReconcileNeedsInput, Detail: detail}
	if issueID != "" {
		event.IssueID = nullString(issueID)
	}
	if err := d.store.AppendEvent(ctx, event); err != nil {
		return fmt.Errorf("recording workspace reconciliation needs-input event: %w", err)
	}
	return nil
}

// cleanupCoderWorkspaceRequested reports whether a terminal transition has
// exactly one durable, non-deleted remote coder workspace to schedule. A
// duplicate is deliberately not selected: startup reconciliation emits the
// visible needs-input event and leaves both provider objects untouched.
func (d *Dispatcher) cleanupCoderWorkspaceRequested(ctx context.Context, issueID, terminalStatus string) (bool, error) {
	provider := d.cfg.AgentBackend.Type
	if (provider != "daytona" && provider != "local") || !terminalStatuses[terminalStatus] {
		return false, nil
	}
	workspaces, err := d.store.AgentWorkspacesByIssue(ctx, issueID)
	if err != nil {
		return false, fmt.Errorf("loading coder workspace for terminal cleanup: %w", err)
	}
	count := 0
	for _, workspace := range workspaces {
		if workspace.Provider == provider && workspace.Role == "coder" && workspace.State != store.WorkspaceDeleted && workspace.LastAction != workspaceReconcileNeedsInputAction {
			count++
		}
	}
	return count == 1, nil
}

func localWorkspaceOwnerKey(issueID string) string {
	return "local:coder:" + issueID
}

func (d *Dispatcher) recordLocalWorkspace(ctx context.Context, issue store.Issue, workspacePath string) error {
	if d.cfg.AgentBackend.Type != "local" {
		return nil
	}
	now := d.now()
	if err := d.store.UpsertAgentWorkspace(ctx, store.AgentWorkspace{
		OwnerKey:      localWorkspaceOwnerKey(issue.ID),
		IssueID:       issue.ID,
		Provider:      "local",
		Role:          "coder",
		WorkspacePath: workspacePath,
		State:         store.WorkspaceActive,
		LastAction:    "ensure",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		return fmt.Errorf("persisting local workspace: %w", err)
	}
	return nil
}
