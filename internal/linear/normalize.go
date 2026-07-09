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
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    int    `json:"priority"`
	BranchName  string `json:"branchName"`
	UpdatedAt   string `json:"updatedAt"`
	State       struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	// InverseRelations, not Relations: a dependency of this issue is an issue
	// that BLOCKS it. Linear stores a blocking relationship as one record on
	// the blocker's source side (type "blocks"), so from the blocked issue's
	// perspective it surfaces here in inverseRelations, with `issue` being the
	// blocker. Reading the source-side `relations` instead inverted the whole
	// dependency graph (see normalizeIssueNode).
	InverseRelations struct {
		Nodes []struct {
			Type  string `json:"type"`
			Issue struct {
				ID    string `json:"id"`
				State struct {
					Type string `json:"type"`
				} `json:"state"`
			} `json:"issue"`
		} `json:"nodes"`
	} `json:"inverseRelations"`
}

// NormalizeCandidateIssues parses a candidate-issues GraphQL response body
// and maps it to Clipse's normalized Issue slice: lane labels are stripped
// to their bare lane (via labelPrefix, e.g. "agent:"), workflow-state names
// are mapped to our Column enum, and "blocks"/"blocked-by" relations are
// folded into a single Deps list.
func NormalizeCandidateIssues(body []byte, labelPrefix string) ([]Issue, error) {
	var resp candidateIssuesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("normalizing candidate issues: %w", err)
	}

	nodes := resp.Data.Issues.Nodes
	issues := make([]Issue, 0, len(nodes))
	for _, n := range nodes {
		issue, err := normalizeIssueNode(n, labelPrefix)
		if err != nil {
			return nil, fmt.Errorf("normalizing issue %s: %w", n.Identifier, err)
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// normalizeIssueNode maps a single raw issue node to a normalized Issue.
func normalizeIssueNode(n issueNode, labelPrefix string) (Issue, error) {
	labelNames := make([]string, 0, len(n.Labels.Nodes))
	for _, l := range n.Labels.Nodes {
		labelNames = append(labelNames, l.Name)
	}

	// Deps = the issues that block this one. Only a "blocks" inverse relation
	// is a dependency; "related"/"duplicate"/"similar" links are not and must
	// not gate promotion. r.Issue is the blocker (the source of the blocks
	// relation), which is exactly the issue this one must wait on.
	// A blocker already completed or canceled in Linear is satisfied at
	// ingest and dropped from Deps: blockers outside the candidate set
	// (unlabeled teammates' tickets, shipped work) are never ingested, so a
	// dep on one could never become terminal on the board and promote would
	// hold the child in todo forever. A blocker with no state in the payload
	// stays -- unknown is conservatively live.
	deps := make([]string, 0, len(n.InverseRelations.Nodes))
	for _, r := range n.InverseRelations.Nodes {
		if r.Type != "blocks" {
			continue
		}
		if t := r.Issue.State.Type; t == "completed" || t == "canceled" {
			continue
		}
		deps = append(deps, r.Issue.ID)
	}

	updatedAt, err := time.Parse(time.RFC3339, n.UpdatedAt)
	if err != nil {
		return Issue{}, fmt.Errorf("parsing updatedAt %q: %w", n.UpdatedAt, err)
	}

	return Issue{
		ID:          n.ID,
		Identifier:  n.Identifier,
		Title:       n.Title,
		Description: n.Description,
		Status:      statusFromWorkflowName(n.State.Name, n.State.Type),
		Lane:        laneFromLabels(labelNames, labelPrefix),
		Deps:        deps,
		Priority:    n.Priority,
		BranchName:  n.BranchName,
		UpdatedAt:   updatedAt.Unix(),
	}, nil
}
