// Package backend defines the provider-neutral lifecycle boundary used by the
// dispatcher to provision remote agent workspaces.
package backend

import (
	"context"
	"strings"
)

// ErrorKind classifies a lifecycle failure for deterministic retry/park
// handling in the dispatcher.
type ErrorKind string

const (
	ErrorKindTransient  ErrorKind = "transient"
	ErrorKindCapability ErrorKind = "capability"
	ErrorKindNeedsInput ErrorKind = "needs_input"
)

// WorkspaceState is the provider-neutral state emitted by lifecycle actions.
type WorkspaceState string

const (
	WorkspaceActive         WorkspaceState = "active"
	WorkspaceStopped        WorkspaceState = "stopped"
	WorkspaceCleanupPending WorkspaceState = "cleanup_pending"
	WorkspaceDeleted        WorkspaceState = "deleted"
	WorkspaceError          WorkspaceState = "error"
)

// EnsureRequest carries the issue, repository, and lifecycle policy needed to
// create or resume one role-scoped workspace.
type EnsureRequest struct {
	Provider                  string
	Role                      string
	IssueID                   string
	RunID                     string
	RepoURL                   string
	RepoSlug                  string
	BaseBranch                string
	Branch                    string
	AutoStopMinutes           int
	ReviewerAutoDeleteMinutes int
	Snapshot                  string
	Target                    string
}

// ListRequest scopes a provider preflight/list operation to one repository.
type ListRequest struct {
	Provider string
	RepoSlug string
	Target   string
}

// Workspace is the provider-neutral identity returned by lifecycle actions.
// The request metadata is retained so Delete can reconstruct the typed worker
// command without parsing the provider-specific owner key.
type Workspace struct {
	Provider      string
	OwnerKey      string
	ExternalID    string
	WorkspacePath string
	State         WorkspaceState
	Role          string
	IssueID       string
	RunID         string
	RepoSlug      string
	Target        string
}

// Manager owns remote workspace lifecycle operations.
type Manager interface {
	Ensure(context.Context, EnsureRequest) (Workspace, error)
	Delete(context.Context, Workspace) error
	List(context.Context, ListRequest) ([]Workspace, error)
}

// RepoSlug normalizes a GitHub HTTPS or SCP-style remote to owner/repo for
// provider labels and lifecycle scoping.
func RepoSlug(remote string) string {
	value := strings.TrimSuffix(strings.TrimSpace(remote), "/")
	value = strings.TrimSuffix(value, ".git")
	if scheme := strings.Index(value, "://"); scheme >= 0 {
		afterHost := value[scheme+3:]
		if slash := strings.IndexByte(afterHost, '/'); slash >= 0 {
			value = afterHost[slash+1:]
		}
	} else if at := strings.IndexByte(value, '@'); at >= 0 {
		if colon := strings.IndexByte(value[at:], ':'); colon >= 0 {
			value = value[at+colon+1:]
		}
	}
	value = strings.Trim(value, "/")
	parts := strings.Split(value, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return value
}

// ActionError is safe to expose to the kernel. It contains only the typed
// provider classification and a sanitized message, never stderr or a raw
// provider response.
type ActionError struct {
	Kind ErrorKind
	Op   string
	Msg  string
}

func (e *ActionError) Error() string {
	if e.Op == "" {
		return e.Msg
	}
	return e.Op + ": " + e.Msg
}
