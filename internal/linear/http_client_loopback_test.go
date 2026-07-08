package linear_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
)

// testTeamKey/testTeamID are the Linear team clipse's loopback tests pretend
// to operate against — the real values named in the Phase-2 task (team key
// "CLI"), reused here purely as fixed test constants (no real network).
const (
	testTeamKey = "CLI"
	testTeamID  = "8b5b3301-8da3-4933-9b07-9efc027bc09d"
)

// newLoopbackClient points an HTTPClient at a local httptest.Server instead
// of Linear's real API. httptest.Server binds to 127.0.0.1 on an ephemeral
// port and never leaves the machine, so this stays "zero network" in the
// sense the task cares about (no real Linear/API calls) while still
// exercising HTTPClient's actual HTTP wiring end to end.
func newLoopbackClient(t *testing.T, handler http.HandlerFunc) *linear.HTTPClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	t.Setenv("LINEAR_API_KEY", "test-key")
	c, err := linear.NewHTTPClientWithBaseURL(srv.URL, testTeamKey, testTeamID, "agent:")
	if err != nil {
		t.Fatalf("NewHTTPClientWithBaseURL: unexpected error: %v", err)
	}
	return c
}

func TestNewHTTPClient_MissingAPIKey(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	os.Unsetenv("LINEAR_API_KEY")

	_, err := linear.NewHTTPClient(testTeamKey, testTeamID, "agent:")
	if err == nil {
		t.Fatal("NewHTTPClient: expected error when LINEAR_API_KEY is unset, got nil")
	}
}

func TestHTTPClient_CandidateIssues_ParsesLoopbackResponse(t *testing.T) {
	fixture, err := os.ReadFile("testdata/candidate_issues.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	var gotBody []byte
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "test-key" {
			t.Errorf("Authorization header = %q, want %q", got, "test-key")
		}
		var err error
		gotBody, err = readAll(r)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	})

	issues, err := c.CandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("CandidateIssues: unexpected error: %v", err)
	}
	if len(issues) != 4 {
		t.Fatalf("len(issues) = %d, want 4", len(issues))
	}

	// The candidate-issues query must filter to the client's configured
	// team, not the whole Linear workspace.
	want, err := linear.BuildCandidateIssuesRequest(testTeamKey)
	if err != nil {
		t.Fatalf("BuildCandidateIssuesRequest: unexpected error: %v", err)
	}
	assertJSONEqual(t, gotBody, want)

	// title/description must round-trip end to end through the real
	// HTTPClient (query -> loopback response -> NormalizeCandidateIssues),
	// not just through NormalizeCandidateIssues in isolation -- this is the
	// worker's task text (CLIPSE_ISSUE_TEXT), so a query missing these
	// fields would silently empty it in production.
	var got12 *linear.Issue
	for i := range issues {
		if issues[i].Identifier == "CLP-12" {
			got12 = &issues[i]
		}
	}
	if got12 == nil {
		t.Fatalf("issues = %+v, want an entry for CLP-12", issues)
	}
	if got12.Title != "Add the thing" {
		t.Errorf("CLP-12 Title = %q, want %q", got12.Title, "Add the thing")
	}
	if got12.Description != "Implement the thing that does the stuff." {
		t.Errorf("CLP-12 Description = %q, want %q", got12.Description, "Implement the thing that does the stuff.")
	}
}

func TestHTTPClient_Comment_SendsExactBody(t *testing.T) {
	var gotBody []byte
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = readAll(r)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"commentCreate":{"success":true}}}`))
	})

	if err := c.Comment(context.Background(), "issue-1", "blocked: needs input"); err != nil {
		t.Fatalf("Comment: unexpected error: %v", err)
	}

	want, err := linear.BuildCommentRequest("issue-1", "blocked: needs input")
	if err != nil {
		t.Fatalf("BuildCommentRequest: unexpected error: %v", err)
	}
	assertJSONEqual(t, gotBody, want)
}

func TestHTTPClient_IssueComments_DecodesCannedResponse(t *testing.T) {
	const resp = `{"data":{"issue":{"comments":{"nodes":[
		{"body":"### coder handoff — done\n- schema uses integer epoch-ms timestamps","createdAt":"2026-07-01T00:00:00.000Z"},
		{"body":"second comment","createdAt":"2026-07-02T00:00:00.000Z"}
	]}}}}`

	var gotBody []byte
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = readAll(r)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(resp))
	})

	comments, err := c.IssueComments(context.Background(), "issue-123")
	if err != nil {
		t.Fatalf("IssueComments: unexpected error: %v", err)
	}

	want, err := linear.BuildIssueCommentsRequest("issue-123")
	if err != nil {
		t.Fatalf("BuildIssueCommentsRequest: unexpected error: %v", err)
	}
	assertJSONEqual(t, gotBody, want)

	if len(comments) != 2 {
		t.Fatalf("len(comments) = %d, want 2", len(comments))
	}
	if !strings.Contains(comments[0].Body, "schema uses integer epoch-ms timestamps") {
		t.Errorf("comments[0].Body = %q, want the handoff body", comments[0].Body)
	}
	if comments[0].CreatedAt != "2026-07-01T00:00:00.000Z" {
		t.Errorf("comments[0].CreatedAt = %q, want the fixture timestamp", comments[0].CreatedAt)
	}
	if comments[1].Body != "second comment" {
		t.Errorf("comments[1].Body = %q, want %q", comments[1].Body, "second comment")
	}
}

func TestHTTPClient_GraphQLErrors_ReturnsError(t *testing.T) {
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp, _ := json.Marshal(map[string]any{
			"errors": []map[string]any{{"message": "team not found"}},
		})
		w.Write(resp)
	})

	// SetState's first network call is always the workflow-states query
	// (resolving "review" -> a state id), so a graphql-level error there
	// must propagate just like it would from the mutation itself.
	err := c.SetState(context.Background(), "missing-issue", "review")
	if err == nil {
		t.Fatal("SetState: expected error for graphql-level error response, got nil")
	}
}

func TestHTTPClient_NonOKStatus_ReturnsError(t *testing.T) {
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("unauthorized"))
	})

	_, err := c.CandidateIssues(context.Background())
	if err == nil {
		t.Fatal("CandidateIssues: expected error for non-200 status, got nil")
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}
