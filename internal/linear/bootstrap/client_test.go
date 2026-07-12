package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTeamIssuesParsesMarkersAndBlockers(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"issues":{"nodes":[
			{"id":"L1","identifier":"CLI-1","description":"b\n\n<!-- clipse-ref: core-1 sha:abc123de -->","inverseRelations":{"nodes":[]}},
			{"id":"L2","identifier":"CLI-2","description":"x","inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"L1"}}]}}
		]}}}`))
	}))
	defer srv.Close()

	c, err := NewClientWithBaseURL(srv.URL, "CLI")
	if err != nil {
		t.Fatalf("NewClientWithBaseURL: %v", err)
	}
	issues, err := c.TeamIssues(context.Background())
	if err != nil {
		t.Fatalf("TeamIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues", len(issues))
	}
	if issues[1].ID != "L2" || len(issues[1].BlockedBy) != 1 || issues[1].BlockedBy[0] != "L1" {
		t.Errorf("L2 blockers = %v", issues[1].BlockedBy)
	}
}

func TestNewClientRequiresAPIKey(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	if _, err := NewClient("CLI"); err == nil {
		t.Error("expected error when LINEAR_API_KEY unset")
	}
}
