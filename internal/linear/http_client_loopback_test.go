package linear_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
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
	c, err := linear.NewHTTPClientWithBaseURL(srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPClientWithBaseURL: unexpected error: %v", err)
	}
	return c
}

func TestNewHTTPClient_MissingAPIKey(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	os.Unsetenv("LINEAR_API_KEY")

	_, err := linear.NewHTTPClient()
	if err == nil {
		t.Fatal("NewHTTPClient: expected error when LINEAR_API_KEY is unset, got nil")
	}
}

func TestHTTPClient_CandidateIssues_ParsesLoopbackResponse(t *testing.T) {
	fixture, err := os.ReadFile("testdata/candidate_issues.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "test-key" {
			t.Errorf("Authorization header = %q, want %q", got, "test-key")
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
}

func TestHTTPClient_SetState_SendsExactBody(t *testing.T) {
	var gotBody []byte
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = readAll(r)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
	})

	if err := c.SetState(context.Background(), "issue-1", "state-1"); err != nil {
		t.Fatalf("SetState: unexpected error: %v", err)
	}

	want, err := linear.BuildSetStateRequest("issue-1", "state-1")
	if err != nil {
		t.Fatalf("BuildSetStateRequest: unexpected error: %v", err)
	}
	assertJSONEqual(t, gotBody, want)
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

func TestHTTPClient_GraphQLErrors_ReturnsError(t *testing.T) {
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp, _ := json.Marshal(map[string]any{
			"errors": []map[string]any{{"message": "issue not found"}},
		})
		w.Write(resp)
	})

	err := c.SetState(context.Background(), "missing-issue", "state-1")
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
