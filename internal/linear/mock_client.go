package linear

import (
	"context"
	"fmt"
)

// SetStateCall records one MockClient.SetState invocation, so tests can
// assert exactly which transitions the dispatcher attempted.
type SetStateCall struct {
	IssueID      string
	TargetColumn string
}

// CommentCall records one MockClient.Comment invocation.
type CommentCall struct {
	IssueID string
	Body    string
}

// MockClient is an in-memory Client for tests: CandidateIssues returns a
// scripted slice (or a scripted error), and SetState/Comment record every
// call they receive instead of doing anything. It never touches the
// network, so dispatch tests can drive it deterministically.
type MockClient struct {
	// Issues is returned by CandidateIssues when Err is nil.
	Issues []Issue
	// Err, if set, is returned (wrapped) by CandidateIssues instead of Issues.
	Err error

	// SetStateErr, if set, is returned (wrapped) by every SetState call.
	// The call is still recorded even when this is set.
	SetStateErr error
	// SetStateCalls accumulates every SetState call, in order.
	SetStateCalls []SetStateCall

	// CommentErr, if set, is returned (wrapped) by every Comment call.
	// The call is still recorded even when this is set.
	CommentErr error
	// CommentCalls accumulates every Comment call, in order.
	CommentCalls []CommentCall

	// Comments maps an issueID to the comments IssueComments returns for it
	// (nil/absent yields an empty slice, matching HTTPClient's behavior).
	Comments map[string][]Comment
	// IssueCommentsErr, if set, is returned (wrapped) by every IssueComments
	// call. The call is still recorded even when this is set.
	IssueCommentsErr error
	// IssueCommentsCalls accumulates every IssueComments call's issueID, in order.
	IssueCommentsCalls []string
}

// CandidateIssues returns m.Issues, or a wrapped m.Err if set.
func (m *MockClient) CandidateIssues(ctx context.Context) ([]Issue, error) {
	if m.Err != nil {
		return nil, fmt.Errorf("mock candidate issues: %w", m.Err)
	}
	return m.Issues, nil
}

// SetState records the call and returns a wrapped m.SetStateErr if set.
func (m *MockClient) SetState(ctx context.Context, issueID, targetColumn string) error {
	m.SetStateCalls = append(m.SetStateCalls, SetStateCall{IssueID: issueID, TargetColumn: targetColumn})
	if m.SetStateErr != nil {
		return fmt.Errorf("mock set state: %w", m.SetStateErr)
	}
	return nil
}

// Comment records the call and returns a wrapped m.CommentErr if set.
func (m *MockClient) Comment(ctx context.Context, issueID, body string) error {
	m.CommentCalls = append(m.CommentCalls, CommentCall{IssueID: issueID, Body: body})
	if m.CommentErr != nil {
		return fmt.Errorf("mock comment: %w", m.CommentErr)
	}
	return nil
}

// IssueComments records the call and returns the scripted comments for
// issueID, or a wrapped m.IssueCommentsErr if set.
func (m *MockClient) IssueComments(ctx context.Context, issueID string) ([]Comment, error) {
	m.IssueCommentsCalls = append(m.IssueCommentsCalls, issueID)
	if m.IssueCommentsErr != nil {
		return nil, fmt.Errorf("mock issue comments: %w", m.IssueCommentsErr)
	}
	return m.Comments[issueID], nil
}
