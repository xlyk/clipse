package boardspec

import (
	"fmt"
	"sort"
)

// Action is what BuildPlan decided for one spec issue.
type Action int

const (
	Create Action = iota
	Update
	Skip
)

func (a Action) String() string {
	switch a {
	case Create:
		return "create"
	case Update:
		return "update"
	default:
		return "skip"
	}
}

// BoardIssue is one existing Linear issue as the planner needs to see it.
// BlockedBy holds the Linear ids of this issue's blockers.
type BoardIssue struct {
	ID          string
	Identifier  string
	Description string
	BlockedBy   []string
}

// IssueOp is the planned action for one spec issue.
type IssueOp struct {
	Ref        string
	Action     Action
	Issue      Issue
	ExistingID string // set for Update/Skip
}

// RelationOp is a blocked-by edge the spec requires (FromRef is blocked by
// ToRef). The applier skips ones already present on the board.
type RelationOp struct {
	FromRef string
	ToRef   string
}

// Plan is the full reconciliation plan. It mutates nothing on its own.
type Plan struct {
	Issues         []IssueOp
	Relations      []RelationOp
	StaleRelations []RelationOp
	Orphans        []string
}

// BuildPlan diffs a validated spec against the current board (matched by
// marker ref) and returns the reconciliation plan.
func BuildPlan(spec *Spec, board []BoardIssue) (*Plan, error) {
	existingByRef := map[string]BoardIssue{}
	for _, bi := range board {
		if ref, _, ok := ParseMarker(bi.Description); ok {
			if existing, duplicate := existingByRef[ref]; duplicate {
				return nil, fmt.Errorf("duplicate clipse-ref %q on Linear issues %s and %s", ref, boardIssueName(existing), boardIssueName(bi))
			}
			existingByRef[ref] = bi
		}
	}
	refByID := make(map[string]string, len(existingByRef))
	for ref, issue := range existingByRef {
		refByID[issue.ID] = ref
	}
	specRefs := map[string]bool{}
	p := &Plan{}
	staleSeen := map[RelationOp]bool{}
	for _, is := range spec.Issues {
		specRefs[is.Ref] = true
		op := IssueOp{Ref: is.Ref, Issue: is}
		if bi, ok := existingByRef[is.Ref]; ok {
			op.ExistingID = bi.ID
			_, sha, _ := ParseMarker(bi.Description)
			if sha == ContentSHA(spec, is) {
				op.Action = Skip
			} else {
				op.Action = Update
			}
		} else {
			op.Action = Create
		}
		p.Issues = append(p.Issues, op)
		for _, d := range is.Deps {
			if relationExists(existingByRef, is.Ref, d) {
				continue
			}
			p.Relations = append(p.Relations, RelationOp{FromRef: is.Ref, ToRef: d})
		}
		if existing, ok := existingByRef[is.Ref]; ok {
			desired := make(map[string]bool, len(is.Deps))
			for _, dep := range is.Deps {
				desired[dep] = true
			}
			for _, blockerID := range existing.BlockedBy {
				blockerRef, managed := refByID[blockerID]
				stale := RelationOp{FromRef: is.Ref, ToRef: blockerRef}
				if managed && !desired[blockerRef] && !staleSeen[stale] {
					p.StaleRelations = append(p.StaleRelations, stale)
					staleSeen[stale] = true
				}
			}
		}
	}
	for ref := range existingByRef {
		if !specRefs[ref] {
			p.Orphans = append(p.Orphans, ref)
		}
	}
	sort.Strings(p.Orphans)
	sort.Slice(p.StaleRelations, func(i, j int) bool {
		if p.StaleRelations[i].FromRef == p.StaleRelations[j].FromRef {
			return p.StaleRelations[i].ToRef < p.StaleRelations[j].ToRef
		}
		return p.StaleRelations[i].FromRef < p.StaleRelations[j].FromRef
	})
	return p, nil
}

func boardIssueName(issue BoardIssue) string {
	if issue.Identifier != "" {
		return issue.Identifier
	}
	return issue.ID
}

// relationExists reports whether the board already records fromRef as
// blocked-by toRef. Only meaningful when both refs already exist on the
// board; if either is being created this run, the relation cannot pre-exist.
func relationExists(existingByRef map[string]BoardIssue, fromRef, toRef string) bool {
	from, okFrom := existingByRef[fromRef]
	to, okTo := existingByRef[toRef]
	if !okFrom || !okTo {
		return false
	}
	for _, id := range from.BlockedBy {
		if id == to.ID {
			return true
		}
	}
	return false
}
