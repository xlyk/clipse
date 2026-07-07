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
	// "canceled" must stay OUT of the state-type exclusion filter: it is the
	// dispatcher's only signal that a Linear-cancelled issue exists at all.
	// Unlike "completed" (the dispatcher learns about a merge from its OWN
	// action, before Linear's state even changes) and "duplicate" (never a
	// candidate to begin with), cancellation is a human-only Linear event.
	// Excluding it left a cancelled blocker's stale store row frozen forever,
	// permanently stalling any dependent still waiting on it in Todo.
	if strings.Contains(linear.CandidateIssuesQuery, `"canceled"`) {
		t.Errorf("candidate query excludes canceled issues, want them included so cancellation is observable")
	}
	for _, excluded := range []string{"completed", "duplicate"} {
		if !strings.Contains(linear.CandidateIssuesQuery, `"`+excluded+`"`) {
			t.Errorf("candidate query must still exclude %q", excluded)
		}
	}
	// state.type must be fetched so normalize can detect a cancelled state
	// regardless of its (per-team-configurable) display name.
	if !strings.Contains(linear.CandidateIssuesQuery, "type") {
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
