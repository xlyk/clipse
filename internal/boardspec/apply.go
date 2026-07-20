package boardspec

import (
	"context"
	"fmt"
	"sort"
)

// Linear is the subset of Linear operations the applier needs. It is declared
// at the consumer (per clipse convention); internal/linear/bootstrap.Client
// implements it. The dispatcher's client deliberately does NOT.
type Linear interface {
	// EnsureLabels creates any of names not already present on the team.
	EnsureLabels(ctx context.Context, names []string) error
	// StartStateID returns the Linear workflow-state id new issues start in
	// (the state mapping to the board's ready/Todo column).
	StartStateID(ctx context.Context) (string, error)
	// CreateIssue creates an issue and returns its Linear id.
	CreateIssue(ctx context.Context, in CreateInput) (string, error)
	// UpdateIssue updates an existing issue's title/description/labels.
	UpdateIssue(ctx context.Context, id string, in UpdateInput) error
	// AddBlockedBy records that dependentID is blocked by blockerID.
	AddBlockedBy(ctx context.Context, dependentID, blockerID string) error
}

// CreateInput is the payload for creating one issue.
type CreateInput struct {
	Title       string
	Description string
	StateID     string
	Labels      []string
}

// UpdateInput is the payload for updating one issue.
type UpdateInput struct {
	Title       string
	Description string
	Labels      []string
}

// Apply executes a reconciliation plan against l. It ensures every label
// exists, creates issues in dependency order (blockers first) writing the ref
// marker into each description, updates changed issues, then wires the
// missing blocked-by relations the planner selected. Because every created
// issue carries its marker, a run that fails partway is safely resumable.
func Apply(ctx context.Context, l Linear, spec *Spec, p *Plan) error {
	// 1. Ensure every label used across the plan exists.
	labelSet := map[string]bool{}
	for _, op := range p.Issues {
		for _, name := range effectiveLabels(spec, op.Issue) {
			labelSet[name] = true
		}
	}
	if len(labelSet) > 0 {
		names := make([]string, 0, len(labelSet))
		for name := range labelSet {
			names = append(names, name)
		}
		sort.Strings(names)
		if err := l.EnsureLabels(ctx, names); err != nil {
			return fmt.Errorf("ensuring labels: %w", err)
		}
	}

	// 2. Resolve the start state once.
	stateID, err := l.StartStateID(ctx)
	if err != nil {
		return fmt.Errorf("resolving start state: %w", err)
	}

	// 3. Index ops by ref; seed the ref->Linear-id map with existing issues.
	opByRef := make(map[string]IssueOp, len(p.Issues))
	idByRef := make(map[string]string, len(p.Issues))
	for _, op := range p.Issues {
		opByRef[op.Ref] = op
		if op.ExistingID != "" {
			idByRef[op.Ref] = op.ExistingID
		}
	}

	// 4. Create/update in dependency order so a blocker exists before the
	// issue that depends on it.
	order, err := spec.TopoOrder()
	if err != nil {
		return fmt.Errorf("ordering issues: %w", err)
	}
	for _, idx := range order {
		is := spec.Issues[idx]
		op := opByRef[is.Ref]
		switch op.Action {
		case Create:
			id, err := l.CreateIssue(ctx, CreateInput{
				Title:       is.Title,
				Description: WithBody(spec, is),
				StateID:     stateID,
				Labels:      effectiveLabels(spec, is),
			})
			if err != nil {
				return fmt.Errorf("creating issue %q: %w", is.Ref, err)
			}
			idByRef[is.Ref] = id
		case Update:
			if err := l.UpdateIssue(ctx, op.ExistingID, UpdateInput{
				Title:       is.Title,
				Description: WithBody(spec, is),
				Labels:      effectiveLabels(spec, is),
			}); err != nil {
				return fmt.Errorf("updating issue %q: %w", is.Ref, err)
			}
		case Skip:
			// nothing to do
		}
	}

	// 5. Wire the missing relations (planner already dropped present ones).
	for _, r := range p.Relations {
		from, to := idByRef[r.FromRef], idByRef[r.ToRef]
		if from == "" || to == "" {
			return fmt.Errorf("relation %s blocked-by %s: unresolved issue id", r.FromRef, r.ToRef)
		}
		if err := l.AddBlockedBy(ctx, from, to); err != nil {
			return fmt.Errorf("relation %s blocked-by %s: %w", r.FromRef, r.ToRef, err)
		}
	}
	return nil
}

// effectiveLabels is the label set applied to an issue: a human ticket gets
// only the "human" label (never an agent lane label, so the dispatcher skips
// it); otherwise the issue's explicit labels, falling back to the spec's
// default_labels.
func effectiveLabels(spec *Spec, is Issue) []string {
	if is.Human {
		return []string{"human"}
	}
	if len(is.Labels) > 0 {
		return is.Labels
	}
	return spec.DefaultLabels
}
