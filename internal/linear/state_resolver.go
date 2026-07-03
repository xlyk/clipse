package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TeamWorkflowStatesQuery fetches a team's workflow states (id, name, type)
// so SetState can resolve a board Column to the Linear state id that column
// maps to on the configured team — the inverse of statusFromWorkflowName,
// via canonicalWorkflowName (status.go).
const TeamWorkflowStatesQuery = `query TeamWorkflowStates($teamId: String!) {
  team(id: $teamId) {
    states {
      nodes {
        id
        name
        type
      }
    }
  }
}`

// BuildTeamWorkflowStatesRequest builds the request body for
// TeamWorkflowStatesQuery.
func BuildTeamWorkflowStatesRequest(teamID string) ([]byte, error) {
	return marshalGraphQLRequest(TeamWorkflowStatesQuery, map[string]any{
		"teamId": teamID,
	})
}

// teamStatesResponse mirrors the shape of a TeamWorkflowStatesQuery response
// body.
type teamStatesResponse struct {
	Data struct {
		Team struct {
			States struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	} `json:"data"`
}

// parseTeamStatesResponse parses a TeamWorkflowStatesQuery response body
// into a map from workflow-state name (lowercased, matching
// statusFromWorkflowName's case-insensitive convention) to that state's
// Linear id.
func parseTeamStatesResponse(body []byte) (map[string]string, error) {
	var resp teamStatesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing team workflow states: %w", err)
	}

	ids := make(map[string]string, len(resp.Data.Team.States.Nodes))
	for _, n := range resp.Data.Team.States.Nodes {
		ids[strings.ToLower(n.Name)] = n.ID
	}
	return ids, nil
}

// resolveStateID returns the Linear workflow-state id that column (a bare
// board Column string, e.g. "review") resolves to on c's configured team,
// resolving and caching the team's full states map on first use (see
// workflowStateIDs).
func (c *HTTPClient) resolveStateID(ctx context.Context, column string) (string, error) {
	canonicalName, ok := canonicalWorkflowName(column)
	if !ok {
		return "", fmt.Errorf("resolving state id: unrecognized column %q", column)
	}

	ids, err := c.workflowStateIDs(ctx)
	if err != nil {
		return "", err
	}

	id, ok := ids[strings.ToLower(canonicalName)]
	if !ok {
		return "", fmt.Errorf("resolving state id: team %s has no workflow state named %q (column %q)", c.teamID, canonicalName, column)
	}
	return id, nil
}

// workflowStateIDs returns the cached name(lowercase)->id map for c's
// configured team, fetching it from Linear via TeamWorkflowStatesQuery on
// first call and reusing the cached result for the remaining lifetime of c.
func (c *HTTPClient) workflowStateIDs(ctx context.Context) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stateIDs != nil {
		return c.stateIDs, nil
	}
	if c.teamID == "" {
		return nil, fmt.Errorf("resolving workflow states: no team id configured")
	}

	reqBody, err := BuildTeamWorkflowStatesRequest(c.teamID)
	if err != nil {
		return nil, fmt.Errorf("resolving workflow states: %w", err)
	}
	respBody, err := c.do(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("resolving workflow states: %w", err)
	}
	ids, err := parseTeamStatesResponse(respBody)
	if err != nil {
		return nil, fmt.Errorf("resolving workflow states: %w", err)
	}

	c.stateIDs = ids
	return ids, nil
}
