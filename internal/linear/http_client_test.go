package linear_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
)

// gqlRequest mirrors the wire shape of a GraphQL POST body: {query, variables}.
type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func TestBuildCandidateIssuesRequest(t *testing.T) {
	body, err := linear.BuildCandidateIssuesRequest("CLI")
	if err != nil {
		t.Fatalf("BuildCandidateIssuesRequest: unexpected error: %v", err)
	}

	var req gqlRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}

	if req.Query != linear.CandidateIssuesQuery {
		t.Errorf("Query = %q, want the exact candidate-issues query", req.Query)
	}
	// Deps must come from inverseRelations (the issues that block this one),
	// not the source-side `relations` (the issues this one blocks) — reading
	// the latter inverts the dependency graph on the live board.
	if !strings.Contains(linear.CandidateIssuesQuery, "inverseRelations") {
		t.Errorf("candidate query must fetch inverseRelations for dependency edges")
	}
	wantVars := map[string]any{"teamKey": "CLI"}
	if len(req.Variables) != len(wantVars) || req.Variables["teamKey"] != wantVars["teamKey"] {
		t.Errorf("Variables = %v, want %v (query filters to the configured team)", req.Variables, wantVars)
	}
}

func TestBuildTeamWorkflowStatesRequest_ExactPayload(t *testing.T) {
	body, err := linear.BuildTeamWorkflowStatesRequest("team-123")
	if err != nil {
		t.Fatalf("BuildTeamWorkflowStatesRequest: unexpected error: %v", err)
	}

	want := mustMarshal(t, gqlRequest{
		Query: linear.TeamWorkflowStatesQuery,
		Variables: map[string]any{
			"teamId": "team-123",
		},
	})

	assertJSONEqual(t, body, want)
}

func TestBuildIssueCommentsRequest_ExactPayload(t *testing.T) {
	body, err := linear.BuildIssueCommentsRequest("issue-123")
	if err != nil {
		t.Fatalf("BuildIssueCommentsRequest: unexpected error: %v", err)
	}

	want := mustMarshal(t, gqlRequest{
		Query: linear.IssueCommentsQuery,
		Variables: map[string]any{
			"id": "issue-123",
		},
	})

	assertJSONEqual(t, body, want)
}

func TestBuildSetStateRequest_ExactPayload(t *testing.T) {
	body, err := linear.BuildSetStateRequest("issue-123", "state-456")
	if err != nil {
		t.Fatalf("BuildSetStateRequest: unexpected error: %v", err)
	}

	want := mustMarshal(t, gqlRequest{
		Query: linear.SetStateMutation,
		Variables: map[string]any{
			"issueId": "issue-123",
			"stateId": "state-456",
		},
	})

	assertJSONEqual(t, body, want)
}

func TestBuildCommentRequest_ExactPayload(t *testing.T) {
	body, err := linear.BuildCommentRequest("issue-123", "blocked: needs input")
	if err != nil {
		t.Fatalf("BuildCommentRequest: unexpected error: %v", err)
	}

	want := mustMarshal(t, gqlRequest{
		Query: linear.CommentMutation,
		Variables: map[string]any{
			"issueId": "issue-123",
			"body":    "blocked: needs input",
		},
	})

	assertJSONEqual(t, body, want)
}

func TestCandidateIssuesQuery_KeepsCancelledIssuesInScope(t *testing.T) {
	// Cancellation is a human-only Linear event the dispatcher has no other
	// way to learn about, so "canceled" can't just be excluded outright (a
	// cancelled blocker's stale store row would freeze forever). But nor can
	// it stay in scope forever: this query has no pagination (Linear's
	// GraphQL default is first:50), so an unbounded population of cancelled
	// issues can silently push an active issue off the page. The fix: the
	// unconditional (non-terminal) branch excludes "canceled" outright, and a
	// second OR'd branch folds it back into scope, but ONLY when recently
	// updated -- the dispatcher only needs to OBSERVE a cancellation once (an
	// already-adopted cancelled row is terminal, see promote.go's
	// terminalStatuses, and is never polled again).
	q := linear.CandidateIssuesQuery

	// The unconditional branch excludes exactly completed and canceled, with
	// NO recency restriction -- backlog/unstarted/started/triage are never
	// terminal, so bounding them would risk losing a genuinely active issue.
	if !strings.Contains(q, `nin: ["completed", "canceled"]`) {
		t.Errorf("query = %q, want an unconditional branch excluding exactly completed and canceled", q)
	}

	// The recency-scoped branch folds canceled back into view, but only
	// within the last cancelledRecencyDays window.
	if !strings.Contains(q, `eq: "canceled"`) {
		t.Errorf("query = %q, want a second branch re-including canceled issues", q)
	}
	if !strings.Contains(q, `updatedAt: { gt: "-P14D" }`) {
		t.Errorf("query = %q, want a 14-day updatedAt recency clause scoping the canceled branch", q)
	}

	// "duplicate" is not a real Linear state type (the six real ones are
	// backlog/unstarted/started/completed/canceled/triage) -- it was dead
	// filter text pinned by a prior test, not a defense against anything.
	if strings.Contains(q, "duplicate") {
		t.Errorf("query = %q, want no reference to the nonexistent %q state type", q, "duplicate")
	}

	// state.type must be fetched so normalize can detect a cancelled state
	// regardless of its (per-team-configurable) display name.
	if !strings.Contains(q, "type") {
		t.Errorf("candidate query must fetch state.type for cancellation detection")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling fixture: %v", err)
	}
	return b
}

// assertJSONEqual compares two JSON payloads by decoding to generic values,
// so key ordering differences don't cause false failures while the actual
// query string and variables are still checked byte-for-byte in content.
func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("unmarshaling got: %v", err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("unmarshaling want: %v", err)
	}
	gotCanon, _ := json.Marshal(gotVal)
	wantCanon, _ := json.Marshal(wantVal)
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("request body mismatch:\n got:  %s\n want: %s", gotCanon, wantCanon)
	}
}
