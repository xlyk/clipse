// Package linear provides the GraphQL client and issue normalization for Linear.
package linear

import "context"

// Issue is Clipse's normalized view of a Linear issue: the subset of Linear
// fields the dispatcher needs, mapped onto our own vocabulary (see
// status.go for the Status/Lane mapping rules).
type Issue struct {
	// ID is the Linear issue id (UUID).
	ID string

	// Identifier is the human-facing key, e.g. "CLP-12".
	Identifier string

	// Title is the issue's Linear title. Together with Description, this is
	// the task text a Coder-lane worker actually needs to do the work --
	// without it, the worker drives its agent with an empty prompt (see the
	// dispatcher's CLIPSE_ISSUE_TEXT env injection).
	Title string

	// Description is the issue's Linear description (may be empty).
	Description string

	// Status is the Linear workflow-state name mapped to our board
	// Column enum (contract.Column), e.g. "todo", "review".
	Status string

	// Lane is the bare agent lane (contract.Lane, e.g. "coder"), parsed
	// from an "agent:<lane>" label with the "agent:" prefix stripped.
	// Empty when no agent:<lane> label is present.
	Lane string

	// Deps holds the ids of issues this issue depends on (both "blocks"
	// and "blocked-by" relations are folded into this single blocker list).
	Deps []string

	// Priority is Linear's priority: 0=none, 1=urgent, 2=high, 3=medium,
	// 4=low, passed through unmodified.
	Priority int

	// BranchName is Linear's suggested git branch name, which auto-links
	// a PR pushed to it back to this issue.
	BranchName string

	// UpdatedAt is the issue's last-updated time, as a Unix timestamp
	// (seconds).
	UpdatedAt int64
}

// Comment is a single Linear issue comment: its body plus the ISO-8601
// createdAt timestamp Linear returns. CreatedAt is retained only for
// oldest-first ordering when the dispatcher assembles dependency notes; it
// sorts chronologically as a plain string. Comment bodies are untrusted user
// input threaded into the coder prompt and must never be logged.
type Comment struct {
	Body      string
	CreatedAt string
}

// Client is the seam the dispatcher depends on for all Linear interaction.
// The real implementation (HTTPClient) talks GraphQL over HTTP; tests use
// MockClient so Phase-1 dispatch logic never touches the network.
type Client interface {
	// CandidateIssues returns active-state issues the dispatcher should
	// consider for scheduling this tick.
	CandidateIssues(ctx context.Context) ([]Issue, error)

	// SetState moves the issue identified by issueID to the Linear
	// workflow state matching targetColumn (a contract.Column value).
	SetState(ctx context.Context, issueID, targetColumn string) error

	// Comment posts body as a comment on the issue identified by issueID
	// (used, for example, to record a Blocked reason).
	Comment(ctx context.Context, issueID, body string) error

	// IssueComments returns the comments on the issue identified by issueID,
	// as Linear returns them. Used at coder-spawn time to thread an issue's
	// and its blockers' comment history into the worker's prompt.
	IssueComments(ctx context.Context, issueID string) ([]Comment, error)
}
