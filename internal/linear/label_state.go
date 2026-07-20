package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// StateLabelsQuery resolves every label in the configured state namespace
// for one team. The dispatcher uses it as a startup preflight and caches the
// resulting name-to-id map for later issue updates.
const StateLabelsQuery = `query StateLabels($teamId: ID!, $prefix: String!) {
  issueLabels(first: 250, filter: { team: { id: { eq: $teamId } }, name: { startsWith: $prefix } }) {
    nodes {
      id
      name
    }
  }
}`

// IssueLabelsQuery fetches the current label ids and names for a single
// issue. Label-backed SetState needs this snapshot to remove only the prior
// state label while leaving repository, project, and other user labels alone.
const IssueLabelsQuery = `query IssueLabels($issueId: String!) {
  issue(id: $issueId) {
    labels(first: 250) {
      nodes {
        id
        name
      }
    }
  }
}`

// UpdateStateLabelsMutation atomically adds the target state label and
// removes obsolete state labels. Linear applies addedLabelIds and
// removedLabelIds in one issueUpdate without replacing unrelated labels.
const UpdateStateLabelsMutation = `mutation UpdateStateLabels($issueId: String!, $addedLabelIds: [String!]!, $removedLabelIds: [String!]!) {
  issueUpdate(id: $issueId, input: { addedLabelIds: $addedLabelIds, removedLabelIds: $removedLabelIds }) {
    success
  }
}`

var stateLabelColumns = []string{
	"todo",
	"ready",
	"running",
	"review",
	"merging",
	"done",
	"rework",
	"blocked",
}

// BuildStateLabelsRequest builds the team-scoped state-label preflight.
func BuildStateLabelsRequest(teamID, prefix string) ([]byte, error) {
	return marshalGraphQLRequest(StateLabelsQuery, map[string]any{
		"teamId": teamID,
		"prefix": prefix,
	})
}

// BuildIssueLabelsRequest builds the current-label lookup for issueID.
func BuildIssueLabelsRequest(issueID string) ([]byte, error) {
	return marshalGraphQLRequest(IssueLabelsQuery, map[string]any{
		"issueId": issueID,
	})
}

// BuildUpdateStateLabelsRequest builds one atomic targeted label update.
func BuildUpdateStateLabelsRequest(issueID string, addedLabelIDs, removedLabelIDs []string) ([]byte, error) {
	return marshalGraphQLRequest(UpdateStateLabelsMutation, map[string]any{
		"issueId":         issueID,
		"addedLabelIds":   addedLabelIDs,
		"removedLabelIds": removedLabelIDs,
	})
}

type labelNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ValidateStateLabels verifies that label-backed state mode has all eight
// reserved labels available on the configured team. It is a no-op in the
// default workflow-state mode and caches a successful result.
func (c *HTTPClient) ValidateStateLabels(ctx context.Context) error {
	if c.stateLabelPrefix == "" {
		return nil
	}

	c.labelMu.Lock()
	defer c.labelMu.Unlock()
	if c.stateLabelsValidated {
		return nil
	}
	if c.teamID == "" {
		return fmt.Errorf("no team id configured")
	}

	reqBody, err := BuildStateLabelsRequest(c.teamID, c.stateLabelPrefix)
	if err != nil {
		return fmt.Errorf("building state label query: %w", err)
	}
	respBody, err := c.do(ctx, reqBody)
	if err != nil {
		return fmt.Errorf("querying state labels: %w", err)
	}

	var payload struct {
		Data struct {
			IssueLabels struct {
				Nodes []labelNode `json:"nodes"`
			} `json:"issueLabels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return fmt.Errorf("decoding state labels: %w", err)
	}

	ids := make(map[string]string, len(payload.Data.IssueLabels.Nodes))
	duplicates := make([]string, 0)
	for _, label := range payload.Data.IssueLabels.Nodes {
		if _, exists := ids[label.Name]; exists {
			duplicates = append(duplicates, label.Name)
		}
		ids[label.Name] = label.ID
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		return fmt.Errorf("team %s has duplicate state labels: %s", c.teamID, strings.Join(duplicates, ", "))
	}

	missing := make([]string, 0)
	for _, column := range stateLabelColumns {
		name := c.stateLabelPrefix + column
		if ids[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("team %s is missing required labels: %s", c.teamID, strings.Join(missing, ", "))
	}

	c.stateLabelIDs = ids
	c.stateLabelsValidated = true
	return nil
}

func (c *HTTPClient) setStateLabel(ctx context.Context, issueID, targetColumn string) error {
	if err := c.ValidateStateLabels(ctx); err != nil {
		return fmt.Errorf("validating state labels: %w", err)
	}

	c.labelMu.Lock()
	targetLabelID := c.stateLabelIDs[c.stateLabelPrefix+targetColumn]
	c.labelMu.Unlock()
	if targetLabelID == "" {
		return fmt.Errorf("state label %q was not resolved", c.stateLabelPrefix+targetColumn)
	}

	reqBody, err := BuildIssueLabelsRequest(issueID)
	if err != nil {
		return fmt.Errorf("building issue labels query: %w", err)
	}
	respBody, err := c.do(ctx, reqBody)
	if err != nil {
		return fmt.Errorf("querying issue labels: %w", err)
	}

	var payload struct {
		Data struct {
			Issue *struct {
				Labels struct {
					Nodes []labelNode `json:"nodes"`
				} `json:"labels"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return fmt.Errorf("decoding issue labels: %w", err)
	}
	if payload.Data.Issue == nil {
		return fmt.Errorf("issue %s was not found", issueID)
	}

	addedLabelIDs := []string{targetLabelID}
	removedLabelIDs := make([]string, 0)
	for _, label := range payload.Data.Issue.Labels.Nodes {
		if label.ID == targetLabelID {
			addedLabelIDs = addedLabelIDs[:0]
			continue
		}
		if strings.HasPrefix(label.Name, c.stateLabelPrefix) ||
			(targetColumn == "done" && strings.HasPrefix(label.Name, c.laneLabelPrefix)) {
			removedLabelIDs = append(removedLabelIDs, label.ID)
		}
	}

	if len(addedLabelIDs) == 0 && len(removedLabelIDs) == 0 {
		return nil
	}
	updateBody, err := BuildUpdateStateLabelsRequest(issueID, addedLabelIDs, removedLabelIDs)
	if err != nil {
		return fmt.Errorf("building state label update: %w", err)
	}
	if _, err := c.do(ctx, updateBody); err != nil {
		return fmt.Errorf("updating state labels: %w", err)
	}
	return nil
}
