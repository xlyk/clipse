package board

import "sort"

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
	Issues    []IssueOp
	Relations []RelationOp
	Orphans   []string
}

// BuildPlan diffs a validated spec against the current board (matched by
// marker ref) and returns the reconciliation plan.
func BuildPlan(spec *Spec, board []BoardIssue) *Plan {
	existingByRef := map[string]BoardIssue{}
	for _, bi := range board {
		if ref, _, ok := ParseMarker(bi.Description); ok {
			existingByRef[ref] = bi
		}
	}
	specRefs := map[string]bool{}
	p := &Plan{}
	for _, is := range spec.Issues {
		specRefs[is.Ref] = true
		op := IssueOp{Ref: is.Ref, Issue: is}
		if bi, ok := existingByRef[is.Ref]; ok {
			op.ExistingID = bi.ID
			_, sha, _ := ParseMarker(bi.Description)
			if sha == ContentSHA(is) {
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
	}
	for ref := range existingByRef {
		if !specRefs[ref] {
			p.Orphans = append(p.Orphans, ref)
		}
	}
	sort.Strings(p.Orphans)
	return p
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
