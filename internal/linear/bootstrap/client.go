// Package bootstrap is the Linear mutation client that executes a board
// reconciliation plan: it creates and updates issues, wires blocked-by
// relations, and ensures labels. It is deliberately separate from
// internal/linear.HTTPClient (the dispatcher's client) so the dispatcher can
// never gain the ability to create or delete issues — least-privilege by
// package boundary.
package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/xlyk/clipse/internal/board"
)

const (
	apiURL       = "https://api.linear.app/graphql"
	apiKeyEnvVar = "LINEAR_API_KEY"
)

// Client talks to Linear's GraphQL API to reconcile a board. It carries its
// own small transport (auth + POST + GraphQL-error check) rather than sharing
// the dispatcher client's, keeping internal/linear's surface tight.
type Client struct {
	apiKey     string
	baseURL    string
	teamKey    string
	httpClient *http.Client
}

// NewClient builds a Client scoped to teamKey against Linear's real API,
// reading the key from LINEAR_API_KEY.
func NewClient(teamKey string) (*Client, error) {
	return NewClientWithBaseURL(apiURL, teamKey)
}

// NewClientWithBaseURL is NewClient against baseURL, for httptest loopback.
func NewClientWithBaseURL(baseURL, teamKey string) (*Client, error) {
	apiKey := os.Getenv(apiKeyEnvVar)
	if apiKey == "" {
		return nil, fmt.Errorf("building linear bootstrap client: %s is not set", apiKeyEnvVar)
	}
	return &Client{
		apiKey:     apiKey,
		baseURL:    baseURL,
		teamKey:    teamKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// TeamIssues returns every issue on the configured team as the planner needs
// it: id, identifier, description (carrying the ref marker), and the Linear
// ids of its blockers (inverseRelations of type "blocks").
func (c *Client) TeamIssues(ctx context.Context) ([]board.BoardIssue, error) {
	reqBody, err := marshalGraphQL(TeamIssuesQuery, map[string]any{"teamKey": c.teamKey})
	if err != nil {
		return nil, fmt.Errorf("team issues: %w", err)
	}
	resp, err := c.do(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("team issues: %w", err)
	}
	var payload struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					ID               string `json:"id"`
					Identifier       string `json:"identifier"`
					Description      string `json:"description"`
					InverseRelations struct {
						Nodes []struct {
							Type  string `json:"type"`
							Issue struct {
								ID string `json:"id"`
							} `json:"issue"`
						} `json:"nodes"`
					} `json:"inverseRelations"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return nil, fmt.Errorf("team issues: decoding: %w", err)
	}
	out := make([]board.BoardIssue, 0, len(payload.Data.Issues.Nodes))
	for _, n := range payload.Data.Issues.Nodes {
		bi := board.BoardIssue{ID: n.ID, Identifier: n.Identifier, Description: n.Description}
		for _, r := range n.InverseRelations.Nodes {
			if r.Type == "blocks" {
				bi.BlockedBy = append(bi.BlockedBy, r.Issue.ID)
			}
		}
		out = append(out, bi)
	}
	return out, nil
}

// marshalGraphQL marshals a query/mutation and its variables into a request
// body for Linear's GraphQL endpoint.
func marshalGraphQL(query string, variables map[string]any) ([]byte, error) {
	body, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{Query: query, Variables: variables})
	if err != nil {
		return nil, fmt.Errorf("marshaling graphql request: %w", err)
	}
	return body, nil
}

// do POSTs a prebuilt GraphQL body and returns the raw response, checking the
// HTTP status and any GraphQL-level errors array.
func (c *Client) do(ctx context.Context, reqBody []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear api returned status %d: %s", resp.StatusCode, respBody)
	}
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decoding response envelope: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return nil, fmt.Errorf("linear api returned errors: %s", envelope.Errors[0].Message)
	}
	return respBody, nil
}
