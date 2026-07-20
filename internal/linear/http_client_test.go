package linear_test

import (
	"encoding/json"
	"reflect"
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
	body, err := linear.BuildCandidateIssuesRequest("CLI", "agent:")
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
	// The lane label is the opt-in gate and must be enforced SERVER-side:
	// without a label filter the query returns Linear's default first page
	// (50 nodes) of ALL team issues, so on a big shared board a labeled
	// ticket outside that window silently never ingests (2026-07-08
	// Spacelift relaunch: 854/850 missing from an 8/10 board), and the
	// Go-side lane guard filters an arbitrary 50, not the opted-in set.
	if !strings.Contains(linear.CandidateIssuesQuery, "labels: { some: { name: { startsWith: $labelPrefix } } }") {
		t.Errorf("candidate query must filter by the lane-label prefix server-side")
	}
	// Belt for boards bigger than Linear's default page: fetch the maximum
	// page. With the label filter this bounds CLIPSE-LABELED issues, not
	// team size.
	if !strings.Contains(linear.CandidateIssuesQuery, "first: 250") {
		t.Errorf("candidate query must request the maximum page size")
	}
	// Dependency gating needs each blocker's terminal-ness even when the
	// blocker itself is not a clipse candidate (unlabeled, already shipped):
	// the inverse relation must carry the blocker's state type.
	if !strings.Contains(linear.CandidateIssuesQuery, "state {\n            type") && !strings.Contains(linear.CandidateIssuesQuery, "state { type }") {
		t.Errorf("candidate query must fetch each blocker's state type on inverseRelations")
	}
	if !strings.Contains(linear.CandidateIssuesQuery, "state { type }\n            labels") {
		t.Errorf("candidate query must fetch each blocker's labels for label-state dependency gating")
	}
	wantVars := map[string]any{"teamKey": "CLI", "labelPrefix": "agent:"}
	if len(req.Variables) != len(wantVars) || req.Variables["teamKey"] != wantVars["teamKey"] || req.Variables["labelPrefix"] != wantVars["labelPrefix"] {
		t.Errorf("Variables = %v, want %v (team + label-prefix scoping)", req.Variables, wantVars)
	}
}

func TestBuildLabelStateCandidateIssuesRequest_ReconcilesRecentCompleted(t *testing.T) {
	body, err := linear.BuildLabelStateCandidateIssuesRequest("SPA", "agent:")
	if err != nil {
		t.Fatalf("BuildLabelStateCandidateIssuesRequest: unexpected error: %v", err)
	}

	var req gqlRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if req.Query != linear.LabelStateCandidateIssuesQuery {
		t.Errorf("Query = %q, want the label-state candidate query", req.Query)
	}
	if !strings.Contains(req.Query, `eq: "completed"`) {
		t.Errorf("label-state candidate query must re-include completed issues for terminal reconciliation")
	}
	if strings.Count(req.Query, `updatedAt: { gt: "-P14D" }`) != 2 {
		t.Errorf("label-state candidate query must recency-bound both canceled and completed terminal branches")
	}
	if strings.Contains(linear.CandidateIssuesQuery, `eq: "completed"`) {
		t.Errorf("workflow-state candidate query must remain unchanged and exclude completed issues")
	}
	wantVars := map[string]any{"teamKey": "SPA", "labelPrefix": "agent:"}
	if !reflect.DeepEqual(req.Variables, wantVars) {
		t.Errorf("Variables = %#v, want %#v", req.Variables, wantVars)
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
	// within the terminal-observation recency window.
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
