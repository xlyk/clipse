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
	"strings"
	"sync"
	"time"

	"github.com/xlyk/clipse/internal/boardspec"
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

	// mu guards the lazily-resolved team metadata (see resolve).
	mu          sync.Mutex
	resolved    bool
	teamID      string
	stateByName map[string]string // lowercased state name -> id
	labelByName map[string]string // label name -> id
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
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
func (c *Client) TeamIssues(ctx context.Context) ([]boardspec.BoardIssue, error) {
	var out []boardspec.BoardIssue
	var after any
	seenCursors := map[string]bool{}
	for {
		reqBody, err := marshalGraphQL(TeamIssuesQuery, map[string]any{"teamKey": c.teamKey, "after": after})
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
							PageInfo pageInfo `json:"pageInfo"`
						} `json:"inverseRelations"`
					} `json:"nodes"`
					PageInfo pageInfo `json:"pageInfo"`
				} `json:"issues"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp, &payload); err != nil {
			return nil, fmt.Errorf("team issues: decoding: %w", err)
		}
		for _, n := range payload.Data.Issues.Nodes {
			if n.InverseRelations.PageInfo.HasNextPage {
				return nil, fmt.Errorf("team issues: issue %s inverse-relation pagination exceeds 250; refusing an incomplete board snapshot", n.Identifier)
			}
			bi := boardspec.BoardIssue{ID: n.ID, Identifier: n.Identifier, Description: n.Description}
			for _, r := range n.InverseRelations.Nodes {
				if r.Type == "blocks" {
					bi.BlockedBy = append(bi.BlockedBy, r.Issue.ID)
				}
			}
			out = append(out, bi)
		}
		info := payload.Data.Issues.PageInfo
		if !info.HasNextPage {
			return out, nil
		}
		if info.EndCursor == "" || seenCursors[info.EndCursor] {
			return nil, fmt.Errorf("team issues: invalid repeated or empty pagination cursor")
		}
		seenCursors[info.EndCursor] = true
		after = info.EndCursor
	}
}

// resolve lazily fetches and caches the team's id, workflow states, and
// labels. Safe to call repeatedly; the network fetch happens once.
func (c *Client) resolve(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved {
		return nil
	}
	reqBody, err := marshalGraphQL(TeamMetaQuery, map[string]any{"teamKey": c.teamKey})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, reqBody)
	if err != nil {
		return err
	}
	var payload struct {
		Data struct {
			Team struct {
				ID     string `json:"id"`
				States struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
						Type string `json:"type"`
					} `json:"nodes"`
					PageInfo pageInfo `json:"pageInfo"`
				} `json:"states"`
				Labels struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
					PageInfo pageInfo `json:"pageInfo"`
				} `json:"labels"`
			} `json:"team"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return fmt.Errorf("decoding team meta: %w", err)
	}
	t := payload.Data.Team
	if t.ID == "" {
		return fmt.Errorf("team %q not found", c.teamKey)
	}
	if t.States.PageInfo.HasNextPage {
		return fmt.Errorf("team %q states require pagination beyond 250; refusing incomplete metadata", c.teamKey)
	}
	if t.Labels.PageInfo.HasNextPage {
		return fmt.Errorf("team %q labels require pagination beyond 250; refusing incomplete metadata", c.teamKey)
	}
	c.teamID = t.ID
	c.stateByName = make(map[string]string, len(t.States.Nodes))
	for _, s := range t.States.Nodes {
		c.stateByName[strings.ToLower(s.Name)] = s.ID
	}
	c.labelByName = make(map[string]string, len(t.Labels.Nodes))
	for _, l := range t.Labels.Nodes {
		c.labelByName[l.Name] = l.ID
	}
	c.resolved = true
	return nil
}

// EnsureLabels creates any of names not already present on the team, caching
// each new label's id. Implements boardspec.Linear.
func (c *Client) EnsureLabels(ctx context.Context, names []string) error {
	if err := c.resolve(ctx); err != nil {
		return err
	}
	for _, name := range names {
		c.mu.Lock()
		_, exists := c.labelByName[name]
		teamID := c.teamID
		c.mu.Unlock()
		if exists {
			continue
		}
		reqBody, err := marshalGraphQL(IssueLabelCreateMutation, map[string]any{"name": name, "teamId": teamID})
		if err != nil {
			return err
		}
		resp, err := c.do(ctx, reqBody)
		if err != nil {
			return fmt.Errorf("creating label %q: %w", name, err)
		}
		var payload struct {
			Data struct {
				IssueLabelCreate struct {
					Success    bool `json:"success"`
					IssueLabel struct {
						ID string `json:"id"`
					} `json:"issueLabel"`
				} `json:"issueLabelCreate"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp, &payload); err != nil {
			return fmt.Errorf("decoding label create %q: %w", name, err)
		}
		created := payload.Data.IssueLabelCreate
		if !created.Success || created.IssueLabel.ID == "" {
			return fmt.Errorf("creating label %q: mutation returned success=%t id=%q", name, created.Success, created.IssueLabel.ID)
		}
		c.mu.Lock()
		c.labelByName[name] = created.IssueLabel.ID
		c.mu.Unlock()
	}
	return nil
}

// StartStateID returns the id of the state new issues start in: the "todo"
// state (falling back to "backlog"), which clipse's dispatcher promotes to
// "ready" once an issue's dependencies clear. Implements boardspec.Linear.
func (c *Client) StartStateID(ctx context.Context) (string, error) {
	if err := c.resolve(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range []string{"todo", "backlog"} {
		if id, ok := c.stateByName[name]; ok {
			return id, nil
		}
	}
	names := make([]string, 0, len(c.stateByName))
	for n := range c.stateByName {
		names = append(names, n)
	}
	return "", fmt.Errorf("no start state (todo/backlog) on team %q; states: %v", c.teamKey, names)
}

// labelIDs maps label names to their cached ids, erroring on any unknown name
// (EnsureLabels should have created them first).
func (c *Client) labelIDs(names []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]string, 0, len(names))
	for _, n := range names {
		id, ok := c.labelByName[n]
		if !ok {
			return nil, fmt.Errorf("label %q not resolved (ensure it first)", n)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// CreateIssue creates an issue and returns its Linear id. Implements
// boardspec.Linear.
func (c *Client) CreateIssue(ctx context.Context, in boardspec.CreateInput) (string, error) {
	if err := c.resolve(ctx); err != nil {
		return "", err
	}
	labelIDs, err := c.labelIDs(in.Labels)
	if err != nil {
		return "", err
	}
	reqBody, err := marshalGraphQL(IssueCreateMutation, map[string]any{
		"teamId":      c.teamID,
		"title":       in.Title,
		"description": in.Description,
		"stateId":     in.StateID,
		"labelIds":    labelIDs,
	})
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, reqBody)
	if err != nil {
		return "", err
	}
	var payload struct {
		Data struct {
			IssueCreate struct {
				Success bool `json:"success"`
				Issue   struct {
					ID string `json:"id"`
				} `json:"issue"`
			} `json:"issueCreate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return "", fmt.Errorf("decoding issue create: %w", err)
	}
	created := payload.Data.IssueCreate
	if !created.Success || created.Issue.ID == "" {
		return "", fmt.Errorf("creating issue: mutation returned success=%t id=%q", created.Success, created.Issue.ID)
	}
	return created.Issue.ID, nil
}

// UpdateIssue updates an existing issue. Implements boardspec.Linear.
func (c *Client) UpdateIssue(ctx context.Context, id string, in boardspec.UpdateInput) error {
	if err := c.resolve(ctx); err != nil {
		return err
	}
	labelIDs, err := c.labelIDs(in.Labels)
	if err != nil {
		return err
	}
	reqBody, err := marshalGraphQL(IssueUpdateMutation, map[string]any{
		"id":          id,
		"title":       in.Title,
		"description": in.Description,
		"labelIds":    labelIDs,
	})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, reqBody)
	if err != nil {
		return err
	}
	var payload struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return fmt.Errorf("decoding issue update %q: %w", id, err)
	}
	if !payload.Data.IssueUpdate.Success {
		return fmt.Errorf("updating issue %q: mutation returned success=false", id)
	}
	return nil
}

// AddBlockedBy records that dependentID is blocked by blockerID. The relation
// is created on the blocker's side (issueId=blocker) as type "blocks", so the
// dependent sees it in inverseRelations. Implements boardspec.Linear.
func (c *Client) AddBlockedBy(ctx context.Context, dependentID, blockerID string) error {
	reqBody, err := marshalGraphQL(IssueRelationCreateMutation, map[string]any{
		"issueId":        blockerID,
		"relatedIssueId": dependentID,
	})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, reqBody)
	if err != nil {
		return err
	}
	var payload struct {
		Data struct {
			IssueRelationCreate struct {
				Success bool `json:"success"`
			} `json:"issueRelationCreate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return fmt.Errorf("decoding blocked-by relation %q -> %q: %w", dependentID, blockerID, err)
	}
	if !payload.Data.IssueRelationCreate.Success {
		return fmt.Errorf("creating blocked-by relation %q -> %q: mutation returned success=false", dependentID, blockerID)
	}
	return nil
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
