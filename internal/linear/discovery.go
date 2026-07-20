package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

const teamsQuery = `query ClipseSetupTeams {
  teams(first: 250) {
    nodes {
      id
      key
      name
    }
  }
}`

// Team is one Linear team visible to the current LINEAR_API_KEY.
type Team struct {
	ID   string
	Key  string
	Name string
}

// DiscoverTeams lists the teams visible to the current process credential.
// It is read-only and exists for operator setup; the dispatcher remains
// scoped to the explicit team key/id stored in its config.
func DiscoverTeams(ctx context.Context) ([]Team, error) {
	return DiscoverTeamsWithBaseURL(ctx, apiURL)
}

// DiscoverTeamsWithBaseURL is DiscoverTeams pointed at a loopback endpoint;
// it is exported so external-package tests can verify the exact auth boundary.
func DiscoverTeamsWithBaseURL(ctx context.Context, baseURL string) ([]Team, error) {
	tr, err := newTransport(baseURL)
	if err != nil {
		return nil, fmt.Errorf("building Linear team discovery client: %w", err)
	}
	req, err := marshalGraphQLRequest(teamsQuery, nil)
	if err != nil {
		return nil, fmt.Errorf("building Linear teams query: %w", err)
	}
	body, err := tr.do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("discovering Linear teams: %w", err)
	}
	var payload struct {
		Data struct {
			Teams struct {
				Nodes []Team `json:"nodes"`
			} `json:"teams"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decoding Linear teams: %w", err)
	}
	teams := payload.Data.Teams.Nodes
	sort.Slice(teams, func(i, j int) bool { return teams[i].Key < teams[j].Key })
	return teams, nil
}
