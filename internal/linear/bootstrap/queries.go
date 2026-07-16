package bootstrap

// TeamIssuesQuery fetches a team's issues with the fields the planner needs:
// id, identifier, description (which carries the clipse-ref marker), and the
// inverse blocking relations (the issues that block this one). It mirrors
// internal/linear.CandidateIssuesQuery's team-key filter and inverseRelations
// convention, but is unfiltered by label/state: board reconciliation must see
// every clipse-managed issue, not only the dispatchable ones. The client walks
// every top-level page; nested inverse relations fail closed if they exceed the
// maximum page so an incomplete dependency snapshot is never reconciled.
const TeamIssuesQuery = `query TeamIssues($teamKey: String!, $after: String) {
  issues(first: 250, after: $after, filter: { team: { key: { eq: $teamKey } } }) {
    nodes {
      id
      identifier
      description
      inverseRelations(first: 250) {
        nodes {
          type
          issue { id }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

// TeamMetaQuery resolves the team's id plus its workflow states and labels
// (id + name), so the applier can create issues in the start state, set
// labels by id, and know which labels already exist.
const TeamMetaQuery = `query TeamMeta($teamKey: String!) {
  team(key: $teamKey) {
    id
    states(first: 250) {
      nodes { id name type }
      pageInfo { hasNextPage endCursor }
    }
    labels(first: 250) {
      nodes { id name }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// IssueLabelCreateMutation creates a team label by name.
const IssueLabelCreateMutation = `mutation IssueLabelCreate($name: String!, $teamId: String!) {
  issueLabelCreate(input: { name: $name, teamId: $teamId }) {
    success
    issueLabel { id }
  }
}`

// IssueCreateMutation creates an issue in a given team, state, and label set,
// returning its id and human identifier.
const IssueCreateMutation = `mutation IssueCreate($teamId: String!, $title: String!, $description: String!, $stateId: String!, $labelIds: [String!]) {
  issueCreate(input: { teamId: $teamId, title: $title, description: $description, stateId: $stateId, labelIds: $labelIds }) {
    success
    issue { id identifier }
  }
}`

// IssueUpdateMutation updates an existing issue's title, description, labels.
const IssueUpdateMutation = `mutation IssueUpdate($id: String!, $title: String!, $description: String!, $labelIds: [String!]) {
  issueUpdate(id: $id, input: { title: $title, description: $description, labelIds: $labelIds }) {
    success
  }
}`

// IssueRelationCreateMutation records a blocking relation. It is created on
// the blocker's side as type "blocks", so the blocked (dependent) issue sees
// it in inverseRelations — matching TeamIssuesQuery's decode.
const IssueRelationCreateMutation = `mutation IssueRelationCreate($issueId: String!, $relatedIssueId: String!) {
  issueRelationCreate(input: { issueId: $issueId, relatedIssueId: $relatedIssueId, type: blocks }) {
    success
  }
}`
