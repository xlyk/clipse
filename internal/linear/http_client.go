package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// apiURL is Linear's GraphQL endpoint.
const apiURL = "https://api.linear.app/graphql"

// apiKeyEnvVar is the environment variable HTTPClient reads its Linear
// API key from.
const apiKeyEnvVar = "LINEAR_API_KEY"

// CandidateIssuesQuery fetches active-state issues on the configured team
// along with the fields NormalizeCandidateIssues needs: workflow state name,
// agent:<lane> labels, blocks/blocked-by relations, priority, branch name,
// and updatedAt.
//
// Filtering to "active" issues (Linear's built-in state-type filter) keeps
// completed/cancelled work out of the candidate set; the dispatcher decides
// dispatchability from Status/Deps, not from this query. Filtering to
// team.key scopes the candidate set to the single team clipse is configured
// against (config.Config.TeamKey), so a workspace with other teams' issues
// never surfaces them as candidates.
const CandidateIssuesQuery = `query CandidateIssues($teamKey: String!) {
  issues(filter: { state: { type: { eq: "active" } }, team: { key: { eq: $teamKey } } }) {
    nodes {
      id
      identifier
      priority
      branchName
      updatedAt
      state {
        name
      }
      labels {
        nodes {
          name
        }
      }
      relations {
        nodes {
          type
          relatedIssue {
            id
          }
        }
      }
    }
  }
}`

// SetStateMutation moves an issue to a given workflow state.
const SetStateMutation = `mutation SetState($issueId: String!, $stateId: String!) {
  issueUpdate(id: $issueId, input: { stateId: $stateId }) {
    success
  }
}`

// CommentMutation posts a comment on an issue.
const CommentMutation = `mutation Comment($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
  }
}`

// graphqlRequest is the wire shape of a GraphQL POST body.
type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// graphqlResponse is the wire shape of a GraphQL response: a "data" payload
// alongside an optional "errors" list, per the GraphQL spec.
type graphqlResponse struct {
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// BuildCandidateIssuesRequest builds the request body for CandidateIssuesQuery,
// scoped to teamKey. Factored out from the HTTP call so tests can assert the
// exact payload without sending anything.
func BuildCandidateIssuesRequest(teamKey string) ([]byte, error) {
	return marshalGraphQLRequest(CandidateIssuesQuery, map[string]any{
		"teamKey": teamKey,
	})
}

// BuildSetStateRequest builds the request body for SetStateMutation.
func BuildSetStateRequest(issueID, stateID string) ([]byte, error) {
	return marshalGraphQLRequest(SetStateMutation, map[string]any{
		"issueId": issueID,
		"stateId": stateID,
	})
}

// BuildCommentRequest builds the request body for CommentMutation.
func BuildCommentRequest(issueID, body string) ([]byte, error) {
	return marshalGraphQLRequest(CommentMutation, map[string]any{
		"issueId": issueID,
		"body":    body,
	})
}

// marshalGraphQLRequest marshals a GraphQL query/mutation and its variables
// into the request body bytes Linear's GraphQL endpoint expects.
func marshalGraphQLRequest(query string, variables map[string]any) ([]byte, error) {
	body, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, fmt.Errorf("marshaling graphql request: %w", err)
	}
	return body, nil
}

// HTTPClient is the real Client implementation: it talks to Linear's
// GraphQL API over net/http, authenticating with the API key read from
// the LINEAR_API_KEY environment variable, scoped to a single configured
// team.
type HTTPClient struct {
	apiKey     string
	baseURL    string
	teamKey    string
	teamID     string
	httpClient *http.Client

	// mu guards stateIDs, the lazily-resolved and cached name(lowercase)->id
	// map for teamID (see state_resolver.go). The dispatch loop is
	// single-goroutine (AGENTS.md), so this is defense in depth rather than
	// a load-bearing requirement.
	mu       sync.Mutex
	stateIDs map[string]string
}

// NewHTTPClient builds an HTTPClient using the API key from LINEAR_API_KEY,
// pointed at Linear's real GraphQL endpoint and scoped to the Linear team
// identified by teamKey (candidate-issues filter) and teamID (workflow-state
// resolution for SetState).
// Returns an error if the environment variable is unset or empty.
func NewHTTPClient(teamKey, teamID string) (*HTTPClient, error) {
	return newHTTPClient(apiURL, teamKey, teamID)
}

// NewHTTPClientWithBaseURL builds an HTTPClient like NewHTTPClient, but
// against baseURL instead of Linear's real API. Intended for tests that
// point the client at a local httptest.Server; production code should use
// NewHTTPClient.
func NewHTTPClientWithBaseURL(baseURL, teamKey, teamID string) (*HTTPClient, error) {
	return newHTTPClient(baseURL, teamKey, teamID)
}

func newHTTPClient(baseURL, teamKey, teamID string) (*HTTPClient, error) {
	apiKey := os.Getenv(apiKeyEnvVar)
	if apiKey == "" {
		return nil, fmt.Errorf("building linear http client: %s is not set", apiKeyEnvVar)
	}
	return &HTTPClient{
		apiKey:     apiKey,
		baseURL:    baseURL,
		teamKey:    teamKey,
		teamID:     teamID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// CandidateIssues runs CandidateIssuesQuery, scoped to c's configured team,
// and normalizes the response.
func (c *HTTPClient) CandidateIssues(ctx context.Context) ([]Issue, error) {
	reqBody, err := BuildCandidateIssuesRequest(c.teamKey)
	if err != nil {
		return nil, fmt.Errorf("candidate issues: %w", err)
	}

	respBody, err := c.do(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("candidate issues: %w", err)
	}

	issues, err := NormalizeCandidateIssues(respBody)
	if err != nil {
		return nil, fmt.Errorf("candidate issues: %w", err)
	}
	return issues, nil
}

// SetState moves issueID to targetColumn's Linear workflow state: it
// resolves targetColumn (a bare board Column string, e.g. "review") to that
// state's Linear id on c's configured team (state_resolver.go, cached after
// the first resolve), then runs SetStateMutation with the resolved id.
func (c *HTTPClient) SetState(ctx context.Context, issueID, targetColumn string) error {
	stateID, err := c.resolveStateID(ctx, targetColumn)
	if err != nil {
		return fmt.Errorf("set state: %w", err)
	}

	reqBody, err := BuildSetStateRequest(issueID, stateID)
	if err != nil {
		return fmt.Errorf("set state: %w", err)
	}
	if _, err := c.do(ctx, reqBody); err != nil {
		return fmt.Errorf("set state: %w", err)
	}
	return nil
}

// Comment runs CommentMutation to post body on issueID.
func (c *HTTPClient) Comment(ctx context.Context, issueID, body string) error {
	reqBody, err := BuildCommentRequest(issueID, body)
	if err != nil {
		return fmt.Errorf("comment: %w", err)
	}
	if _, err := c.do(ctx, reqBody); err != nil {
		return fmt.Errorf("comment: %w", err)
	}
	return nil
}

// do POSTs a prebuilt GraphQL request body to Linear's API and returns the
// raw response body, after checking the HTTP status and any GraphQL-level
// "errors" array.
func (c *HTTPClient) do(ctx context.Context, reqBody []byte) ([]byte, error) {
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

	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("decoding response envelope: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("linear api returned errors: %s", gqlResp.Errors[0].Message)
	}

	return respBody, nil
}
