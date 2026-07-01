package linear_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
)

func TestMockClient_CandidateIssues_ReturnsScriptedIssues(t *testing.T) {
	want := []linear.Issue{
		{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder"},
		{ID: "issue-2", Identifier: "CLP-2", Status: "todo", Lane: ""},
	}
	m := &linear.MockClient{Issues: want}

	got, err := m.CandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("CandidateIssues: unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Identifier != want[i].Identifier ||
			got[i].Status != want[i].Status || got[i].Lane != want[i].Lane {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMockClient_CandidateIssues_ReturnsScriptedError(t *testing.T) {
	wantErr := errors.New("boom")
	m := &linear.MockClient{Err: wantErr}

	_, err := m.CandidateIssues(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("CandidateIssues: err = %v, want wrapping %v", err, wantErr)
	}
}

func TestMockClient_SetState_RecordsCall(t *testing.T) {
	m := &linear.MockClient{}

	if err := m.SetState(context.Background(), "issue-1", "running"); err != nil {
		t.Fatalf("SetState: unexpected error: %v", err)
	}
	if err := m.SetState(context.Background(), "issue-2", "review"); err != nil {
		t.Fatalf("SetState: unexpected error: %v", err)
	}

	want := []linear.SetStateCall{
		{IssueID: "issue-1", TargetColumn: "running"},
		{IssueID: "issue-2", TargetColumn: "review"},
	}
	if len(m.SetStateCalls) != len(want) {
		t.Fatalf("len(SetStateCalls) = %d, want %d", len(m.SetStateCalls), len(want))
	}
	for i := range want {
		if m.SetStateCalls[i] != want[i] {
			t.Errorf("SetStateCalls[%d] = %+v, want %+v", i, m.SetStateCalls[i], want[i])
		}
	}
}

func TestMockClient_Comment_RecordsCall(t *testing.T) {
	m := &linear.MockClient{}

	if err := m.Comment(context.Background(), "issue-1", "blocked: needs input"); err != nil {
		t.Fatalf("Comment: unexpected error: %v", err)
	}

	want := []linear.CommentCall{
		{IssueID: "issue-1", Body: "blocked: needs input"},
	}
	if len(m.CommentCalls) != len(want) {
		t.Fatalf("len(CommentCalls) = %d, want %d", len(m.CommentCalls), len(want))
	}
	if m.CommentCalls[0] != want[0] {
		t.Errorf("CommentCalls[0] = %+v, want %+v", m.CommentCalls[0], want[0])
	}
}

func TestMockClient_SetStateErr_ReturnsScriptedError(t *testing.T) {
	wantErr := errors.New("state update failed")
	m := &linear.MockClient{SetStateErr: wantErr}

	err := m.SetState(context.Background(), "issue-1", "running")
	if !errors.Is(err, wantErr) {
		t.Fatalf("SetState: err = %v, want wrapping %v", err, wantErr)
	}
	// The call is still recorded even though it errors, so tests can assert
	// dispatch attempted the transition.
	if len(m.SetStateCalls) != 1 {
		t.Errorf("len(SetStateCalls) = %d, want 1", len(m.SetStateCalls))
	}
}

func TestMockClient_CommentErr_ReturnsScriptedError(t *testing.T) {
	wantErr := errors.New("comment failed")
	m := &linear.MockClient{CommentErr: wantErr}

	err := m.Comment(context.Background(), "issue-1", "body")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Comment: err = %v, want wrapping %v", err, wantErr)
	}
	if len(m.CommentCalls) != 1 {
		t.Errorf("len(CommentCalls) = %d, want 1", len(m.CommentCalls))
	}
}

// Compile-time assertion that MockClient satisfies the Client interface.
var _ linear.Client = &linear.MockClient{}
