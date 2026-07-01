package linear

import (
	"encoding/json"
	"fmt"
	"time"
)

// candidateIssuesResponse mirrors the shape of the candidate-issues GraphQL
// query response body (the "data" envelope Linear's API wraps every query
// result in).
type candidateIssuesResponse struct {
	Data struct {
		Issues struct {
			Nodes []issueNode `json:"nodes"`
		} `json:"issues"`
	} `json:"data"`
}

// issueNode mirrors one issue node as shaped by candidateIssuesQuery.
type issueNode struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Priority   int    `json:"priority"`
	BranchName string `json:"branchName"`
	UpdatedAt  string `json:"updatedAt"`
	State      struct {
		Name string `json:"name"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Relations struct {
		Nodes []struct {
			Type         string `json:"type"`
			RelatedIssue struct {
				ID string `json:"id"`
			} `json:"relatedIssue"`
		} `json:"nodes"`
	} `json:"relations"`
}

// NormalizeCandidateIssues parses a candidate-issues GraphQL response body
// and maps it to Clipse's normalized Issue slice: lane labels are stripped
// to their bare lane, workflow-state names are mapped to our Column enum,
// and "blocks"/"blocked-by" relations are folded into a single Deps list.
func NormalizeCandidateIssues(body []byte) ([]Issue, error) {
	var resp candidateIssuesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("normalizing candidate issues: %w", err)
	}

	nodes := resp.Data.Issues.Nodes
	issues := make([]Issue, 0, len(nodes))
	for _, n := range nodes {
		issue, err := normalizeIssueNode(n)
		if err != nil {
			return nil, fmt.Errorf("normalizing issue %s: %w", n.Identifier, err)
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// normalizeIssueNode maps a single raw issue node to a normalized Issue.
func normalizeIssueNode(n issueNode) (Issue, error) {
	labelNames := make([]string, 0, len(n.Labels.Nodes))
	for _, l := range n.Labels.Nodes {
		labelNames = append(labelNames, l.Name)
	}

	deps := make([]string, 0, len(n.Relations.Nodes))
	for _, r := range n.Relations.Nodes {
		switch r.Type {
		case "blocks", "blocked-by":
			deps = append(deps, r.RelatedIssue.ID)
		}
	}

	updatedAt, err := time.Parse(time.RFC3339, n.UpdatedAt)
	if err != nil {
		return Issue{}, fmt.Errorf("parsing updatedAt %q: %w", n.UpdatedAt, err)
	}

	return Issue{
		ID:         n.ID,
		Identifier: n.Identifier,
		Status:     statusFromWorkflowName(n.State.Name),
		Lane:       laneFromLabels(labelNames),
		Deps:       deps,
		Priority:   n.Priority,
		BranchName: n.BranchName,
		UpdatedAt:  updatedAt.Unix(),
	}, nil
}
