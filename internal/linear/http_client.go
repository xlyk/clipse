package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// apiURL is Linear's GraphQL endpoint.
const apiURL = "https://api.linear.app/graphql"

// cancelledRecencyDays bounds how long a Linear-cancelled issue stays in
// CandidateIssuesQuery's scope after its last update. The dispatcher only
// needs to OBSERVE a cancellation once: adoptLinearMove (dispatcher/poll.go)
// writes board_status="cancelled" into SQLite the first time it sees one, and
// promote.go's terminalStatuses already treats "cancelled" as terminal, so an
// already-adopted cancelled row is never read from Linear again. Without this
// bound, every issue a team has ever cancelled stays in the candidate result
// forever — and this query has no pagination (Linear's GraphQL default page
// size is first:50), so a large enough backlog of old cancellations can
// silently push a genuinely active issue off the page. 14 days comfortably
// covers the poll-interval-to-adoption lag (normally seconds) with generous
// slack for a dispatcher outage.
const cancelledRecencyDays = 14

// CandidateIssuesQuery fetches active-state issues on the configured team
// along with the fields NormalizeCandidateIssues needs: title, description,
// workflow state name AND type, agent:<lane> labels, inverse blocking
// relations (the issues that block this one — see below), priority, branch
// name, and updatedAt.
//
// It fetches inverseRelations, NOT relations: a dependency of an issue is an
// issue that blocks it, and Linear records a blocking relationship once, on
// the blocker's source side (type "blocks"). The blocked issue therefore sees
// it in inverseRelations (issue = the blocker). Fetching source-side relations
// instead inverted the dependency graph — a dependent issue looked
// dependency-free and promoted immediately while its blocker waited on it.
//
// title/description are the task text a Coder-lane worker actually needs to
// do the work (the dispatcher injects them into the worker's environment as
// CLIPSE_ISSUE_TEXT) -- without them here, that env var is always empty
// regardless of anything downstream.
//
// The filter is an OR of two branches (Linear has no "active" type; the real
// types are backlog/unstarted/started/completed/canceled/triage):
//
//  1. An unconditional branch excluding "completed" and "canceled", with NO
//     recency restriction — backlog/unstarted/started/triage are never
//     terminal, so bounding them by updatedAt would risk losing a genuinely
//     active issue that just hasn't been touched in a while. "completed" is
//     excluded because the dispatcher learns about a merge from its OWN
//     action (the git-operator lane merges it, then the dispatcher writes
//     board_status="done" itself, before Linear's state even changes) — it
//     never needs to observe "completed" from Linear at all. ("duplicate" is
//     NOT a real Linear state type and was dead filter text.)
//  2. A second branch folding "canceled" back into scope, but only when
//     updated within the last cancelledRecencyDays — cancellation is a
//     human-only Linear event with no other signal, so it can't be excluded
//     outright the way "completed" is (see status.go's cancelled-type
//     mapping and dispatcher/promote.go's dependency-gating), but nor can it
//     stay in scope forever (see cancelledRecencyDays's doc comment).
//
// Filtering to team.key scopes the candidate set to the single team clipse is
// configured against (config.Config.TeamKey), so a workspace with other
// teams' issues never surfaces them as candidates.
//
// The labels filter scopes candidates SERVER-side to issues carrying a
// "<labelPrefix>..." lane label -- the opt-in gate. Without it the query
// returns Linear's default first page (50 nodes) of ALL team issues, so on
// a big shared board a labeled ticket outside that window silently never
// ingests, and the Go-side lane guard (dispatcher/poll.go) filters an
// arbitrary 50 rather than the opted-in set (2026-07-08 Spacelift
// relaunch). first: 250 (Linear's max page) is the belt on top: with the
// label filter it bounds clipse-labeled issues, not team size.
//
// inverseRelations carries each blocker's state.type so normalization can
// drop already-terminal blockers from Deps at ingest -- a blocker outside
// the candidate set (unlabeled, shipped) never reaches the board, so a dep
// on one could otherwise never be satisfied.
var CandidateIssuesQuery = fmt.Sprintf(`query CandidateIssues($teamKey: String!, $labelPrefix: String!) {
  issues(first: 250, filter: { team: { key: { eq: $teamKey } }, labels: { some: { name: { startsWith: $labelPrefix } } }, or: [
    { state: { type: { nin: ["completed", "canceled"] } } },
    { state: { type: { eq: "canceled" } }, updatedAt: { gt: "-P%dD" } }
  ] }) {
    nodes {
      id
      identifier
      title
      description
      priority
      branchName
      updatedAt
      state {
        name
        type
      }
      labels {
        nodes {
          name
        }
      }
      inverseRelations {
        nodes {
          type
          issue {
            id
            state { type }
          }
        }
      }
    }
  }
}`, cancelledRecencyDays)

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

// IssueCommentsQuery fetches the comments on a single issue (by id), newest
// batch capped at 50, with the body and createdAt each normalized Comment
// needs. The dispatcher reads these at coder-spawn time to thread an issue's
// and its blockers' decision history into the worker prompt.
const IssueCommentsQuery = `query IssueComments($id: String!) {
  issue(id: $id) {
    comments(first: 50) {
      nodes {
        body
        createdAt
      }
    }
  }
}`

// graphqlRequest is the wire shape of a GraphQL POST body.
type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// BuildCandidateIssuesRequest builds the request body for CandidateIssuesQuery,
// scoped to teamKey and to issues carrying a labelPrefix-prefixed lane label.
// Factored out from the HTTP call so tests can assert the exact payload
// without sending anything.
func BuildCandidateIssuesRequest(teamKey, labelPrefix string) ([]byte, error) {
	return marshalGraphQLRequest(CandidateIssuesQuery, map[string]any{
		"teamKey":     teamKey,
		"labelPrefix": labelPrefix,
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

// BuildIssueCommentsRequest builds the request body for IssueCommentsQuery.
func BuildIssueCommentsRequest(issueID string) ([]byte, error) {
	return marshalGraphQLRequest(IssueCommentsQuery, map[string]any{
		"id": issueID,
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
	*transport
	teamKey     string
	teamID      string
	labelPrefix string

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
// resolution for SetState). labelPrefix is config.Config.LaneLabelPrefix,
// threaded through for Linear label parsing (see status.go's laneFromLabels).
// Returns an error if the environment variable is unset or empty.
func NewHTTPClient(teamKey, teamID, labelPrefix string) (*HTTPClient, error) {
	return newHTTPClient(apiURL, teamKey, teamID, labelPrefix)
}

// NewHTTPClientWithBaseURL builds an HTTPClient like NewHTTPClient, but
// against baseURL instead of Linear's real API. Intended for tests that
// point the client at a local httptest.Server; production code should use
// NewHTTPClient.
func NewHTTPClientWithBaseURL(baseURL, teamKey, teamID, labelPrefix string) (*HTTPClient, error) {
	return newHTTPClient(baseURL, teamKey, teamID, labelPrefix)
}

func newHTTPClient(baseURL, teamKey, teamID, labelPrefix string) (*HTTPClient, error) {
	tr, err := newTransport(baseURL)
	if err != nil {
		return nil, fmt.Errorf("building linear http client: %w", err)
	}
	return &HTTPClient{
		transport:   tr,
		teamKey:     teamKey,
		teamID:      teamID,
		labelPrefix: labelPrefix,
	}, nil
}

// CandidateIssues runs CandidateIssuesQuery, scoped to c's configured team,
// and normalizes the response.
func (c *HTTPClient) CandidateIssues(ctx context.Context) ([]Issue, error) {
	reqBody, err := BuildCandidateIssuesRequest(c.teamKey, c.labelPrefix)
	if err != nil {
		return nil, fmt.Errorf("candidate issues: %w", err)
	}

	respBody, err := c.do(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("candidate issues: %w", err)
	}

	issues, err := NormalizeCandidateIssues(respBody, c.labelPrefix)
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

// IssueComments runs IssueCommentsQuery for issueID and decodes the response
// into Comments. A missing issue or empty comment list decodes to an empty
// slice, not an error.
func (c *HTTPClient) IssueComments(ctx context.Context, issueID string) ([]Comment, error) {
	reqBody, err := BuildIssueCommentsRequest(issueID)
	if err != nil {
		return nil, fmt.Errorf("issue comments: %w", err)
	}

	respBody, err := c.do(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("issue comments: %w", err)
	}

	var payload struct {
		Data struct {
			Issue struct {
				Comments struct {
					Nodes []struct {
						Body      string `json:"body"`
						CreatedAt string `json:"createdAt"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("issue comments: decoding response: %w", err)
	}

	nodes := payload.Data.Issue.Comments.Nodes
	comments := make([]Comment, 0, len(nodes))
	for _, n := range nodes {
		comments = append(comments, Comment{Body: n.Body, CreatedAt: n.CreatedAt})
	}
	return comments, nil
}
