package bootstrap

// TeamIssuesQuery fetches a team's issues with the fields the planner needs:
// id, identifier, description (which carries the clipse-ref marker), and the
// inverse blocking relations (the issues that block this one). It mirrors
// internal/linear.CandidateIssuesQuery's team-key filter and inverseRelations
// convention, but is unfiltered by label/state: board reconciliation must see
// every clipse-managed issue, not only the dispatchable ones. first:250 is
// Linear's max page (pagination is a known v1 limitation).
const TeamIssuesQuery = `query TeamIssues($teamKey: String!) {
  issues(first: 250, filter: { team: { key: { eq: $teamKey } } }) {
    nodes {
      id
      identifier
      description
      inverseRelations {
        nodes {
          type
          issue { id }
        }
      }
    }
  }
}`
