package board

import "testing"

func TestBuildPlanClassifiesCreateUpdateSkipOrphan(t *testing.T) {
	spec := &Spec{Team: "CLI", Issues: []Issue{
		{Ref: "keep", Title: "Keep", Body: "same"},
		{Ref: "chg", Title: "Chg", Body: "new"},
		{Ref: "new", Title: "New", Body: "fresh"},
	}}
	keepSHA := ContentSHA(spec.Issues[0])
	board := []BoardIssue{
		{ID: "L1", Description: "same\n\n" + RenderMarker("keep", keepSHA)},
		{ID: "L2", Description: "old\n\n" + RenderMarker("chg", "deadbeef")},
		{ID: "L9", Description: "x\n\n" + RenderMarker("gone", "cafe")},
	}
	p := BuildPlan(spec, board)
	got := map[string]Action{}
	for _, op := range p.Issues {
		got[op.Ref] = op.Action
	}
	if got["keep"] != Skip || got["chg"] != Update || got["new"] != Create {
		t.Errorf("actions = %v", got)
	}
	if len(p.Orphans) != 1 || p.Orphans[0] != "gone" {
		t.Errorf("orphans = %v, want [gone]", p.Orphans)
	}
	// The update op must carry the existing Linear id.
	for _, op := range p.Issues {
		if op.Ref == "chg" && op.ExistingID != "L2" {
			t.Errorf("chg ExistingID = %q, want L2", op.ExistingID)
		}
	}
}

func TestBuildPlanEmitsRelationsFromDeps(t *testing.T) {
	spec := &Spec{Team: "CLI", Issues: []Issue{
		{Ref: "a", Title: "A", Body: "x"},
		{Ref: "b", Title: "B", Body: "y", Deps: []string{"a"}},
	}}
	p := BuildPlan(spec, nil)
	if len(p.Relations) != 1 || p.Relations[0].FromRef != "b" || p.Relations[0].ToRef != "a" {
		t.Errorf("relations = %v, want [{b a}]", p.Relations)
	}
}
