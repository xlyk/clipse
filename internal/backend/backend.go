// Package backend defines the provider-neutral lifecycle boundary used by the
// dispatcher to provision remote agent workspaces.
package backend

import (
	"context"
	"errors"
	"net/url"
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

// CanonicalGitHubRemote validates a credential-free GitHub HTTPS or SCP-style
// SSH remote and returns the credential-free HTTPS clone URL plus owner/repo.
// Errors deliberately omit the rejected input because URL userinfo may carry
// a token.
func CanonicalGitHubRemote(remote string) (canonicalURL, slug string, err error) {
	value := strings.TrimSpace(remote)
	var repoPath string
	if strings.HasPrefix(value, "git@github.com:") {
		repoPath = strings.TrimPrefix(value, "git@github.com:")
	} else {
		parsed, parseErr := url.Parse(value)
		if parseErr != nil {
			return "", "", errors.New("remote is not a valid GitHub URL")
		}
		if parsed.User != nil {
			return "", "", errors.New("remote must not contain credentials")
		}
		if parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", "", errors.New("remote must be credential-free GitHub HTTPS or SCP-style SSH")
		}
		repoPath = parsed.Path
	}

	repoPath = strings.TrimSuffix(strings.Trim(repoPath, "/"), ".git")
	parts := strings.Split(repoPath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("remote must identify one GitHub owner and repository")
	}
	slug = parts[0] + "/" + parts[1]
	return "https://github.com/" + slug + ".git", slug, nil
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
