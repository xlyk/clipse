package board

import (
	"strings"
	"testing"
)

func TestPlanRenderSummary(t *testing.T) {
	p := &Plan{
		Issues: []IssueOp{
			{Ref: "a", Action: Create, Issue: Issue{Title: "A"}},
			{Ref: "b", Action: Skip, Issue: Issue{Title: "B"}},
		},
		Relations: []RelationOp{{FromRef: "b", ToRef: "a"}},
		Orphans:   []string{"z"},
	}
	out := p.Render()
	if !strings.Contains(out, "+ create") || !strings.Contains(out, "= skip") {
		t.Errorf("missing action rows:\n%s", out)
	}
	if !strings.Contains(out, "1 create") || !strings.Contains(out, "1 orphan") {
		t.Errorf("bad summary:\n%s", out)
	}
	if !strings.Contains(out, "relation b blocked-by a") {
		t.Errorf("missing relation row:\n%s", out)
	}
}
