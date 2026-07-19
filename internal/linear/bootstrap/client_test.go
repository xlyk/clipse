package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/boardspec"
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

func TestTeamIssuesPaginates(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-key")
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		after, _ := request.Variables["after"].(string)
		switch after {
		case "":
			fmt.Fprint(w, `{"data":{"issues":{"nodes":[
				{"id":"L1","identifier":"CLI-1","description":"one","inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false}}}
			],"pageInfo":{"hasNextPage":true,"endCursor":"page-2"}}}}`)
		case "page-2":
			fmt.Fprint(w, `{"data":{"issues":{"nodes":[
				{"id":"L2","identifier":"CLI-2","description":"two","inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false}}}
			],"pageInfo":{"hasNextPage":false,"endCursor":"page-2"}}}}`)
		default:
			t.Fatalf("unexpected after cursor %q", after)
		}
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
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(issues) != 2 || issues[0].ID != "L1" || issues[1].ID != "L2" {
		t.Fatalf("issues = %+v, want L1 then L2", issues)
	}
}

func TestTeamIssuesRejectsTruncatedInverseRelations(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"issues":{"nodes":[
			{"id":"L1","identifier":"CLI-1","description":"one","inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"more"}}}
		],"pageInfo":{"hasNextPage":false}}}}`)
	}))
	defer srv.Close()

	c, err := NewClientWithBaseURL(srv.URL, "CLI")
	if err != nil {
		t.Fatalf("NewClientWithBaseURL: %v", err)
	}
	_, err = c.TeamIssues(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CLI-1") || !strings.Contains(err.Error(), "pagination") {
		t.Fatalf("want truncated-relation pagination error, got %v", err)
	}
}

func TestStartStateRejectsTruncatedTeamMetadata(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"team":{"id":"TEAM1",
			"states":{"nodes":[{"id":"S1","name":"Todo","type":"unstarted"}],"pageInfo":{"hasNextPage":false}},
			"labels":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"more"}}}}}`)
	}))
	defer srv.Close()

	c, err := NewClientWithBaseURL(srv.URL, "CLI")
	if err != nil {
		t.Fatalf("NewClientWithBaseURL: %v", err)
	}
	_, err = c.StartStateID(context.Background())
	if err == nil || !strings.Contains(err.Error(), "labels") || !strings.Contains(err.Error(), "pagination") {
		t.Fatalf("want truncated-label pagination error, got %v", err)
	}
}

func TestMutationPayloadValidation(t *testing.T) {
	tests := []struct {
		name     string
		response string
		call     func(context.Context, *Client) error
	}{
		{
			name:     "label success false",
			response: `{"data":{"issueLabelCreate":{"success":false,"issueLabel":{"id":"LB1"}}}}`,
			call: func(ctx context.Context, c *Client) error {
				return c.EnsureLabels(ctx, []string{"agent:coder"})
			},
		},
		{
			name:     "label empty id",
			response: `{"data":{"issueLabelCreate":{"success":true,"issueLabel":{"id":""}}}}`,
			call: func(ctx context.Context, c *Client) error {
				return c.EnsureLabels(ctx, []string{"agent:coder"})
			},
		},
		{
			name:     "issue success false",
			response: `{"data":{"issueCreate":{"success":false,"issue":{"id":"L1"}}}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.CreateIssue(ctx, boardspec.CreateInput{Title: "A", Description: "body", StateID: "S1"})
				return err
			},
		},
		{
			name:     "issue empty id",
			response: `{"data":{"issueCreate":{"success":true,"issue":{"id":""}}}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.CreateIssue(ctx, boardspec.CreateInput{Title: "A", Description: "body", StateID: "S1"})
				return err
			},
		},
		{
			name:     "update success false",
			response: `{"data":{"issueUpdate":{"success":false}}}`,
			call: func(ctx context.Context, c *Client) error {
				return c.UpdateIssue(ctx, "L1", boardspec.UpdateInput{Title: "A", Description: "body"})
			},
		},
		{
			name:     "relation success false",
			response: `{"data":{"issueRelationCreate":{"success":false}}}`,
			call: func(ctx context.Context, c *Client) error {
				return c.AddBlockedBy(ctx, "L2", "L1")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tt.response)
			}))
			defer srv.Close()
			c := &Client{
				apiKey:      "test-key",
				baseURL:     srv.URL,
				teamKey:     "CLI",
				httpClient:  srv.Client(),
				resolved:    true,
				teamID:      "TEAM1",
				stateByName: map[string]string{"todo": "S1"},
				labelByName: map[string]string{},
			}
			if err := tt.call(context.Background(), c); err == nil {
				t.Fatal("want mutation payload error, got nil")
			}
		})
	}
}
