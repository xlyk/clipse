package boardspec

import (
	"strings"
	"testing"
)

func mustBuildPlan(t *testing.T, spec *Spec, board []BoardIssue) *Plan {
	t.Helper()
	p, err := BuildPlan(spec, board)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func TestBuildPlanClassifiesCreateUpdateSkipOrphan(t *testing.T) {
	spec := &Spec{Team: "CLI", Issues: []Issue{
		{Ref: "keep", Title: "Keep", Body: "same"},
		{Ref: "chg", Title: "Chg", Body: "new"},
		{Ref: "new", Title: "New", Body: "fresh"},
	}}
	keepSHA := ContentSHA(spec, spec.Issues[0])
	board := []BoardIssue{
		{ID: "L1", Description: "same\n\n" + RenderMarker("keep", keepSHA)},
		{ID: "L2", Description: "old\n\n" + RenderMarker("chg", "deadbeef")},
		{ID: "L9", Description: "x\n\n" + RenderMarker("gone", "cafe")},
	}
	p := mustBuildPlan(t, spec, board)
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
	p := mustBuildPlan(t, spec, nil)
	if len(p.Relations) != 1 || p.Relations[0].FromRef != "b" || p.Relations[0].ToRef != "a" {
		t.Errorf("relations = %v, want [{b a}]", p.Relations)
	}
}

func TestBuildPlanSkipsExistingRelation(t *testing.T) {
	spec := &Spec{Team: "CLI", Issues: []Issue{
		{Ref: "a", Title: "A", Body: "x"},
		{Ref: "b", Title: "B", Body: "y", Deps: []string{"a"}},
	}}
	aSHA := ContentSHA(spec, spec.Issues[0])
	bSHA := ContentSHA(spec, spec.Issues[1])
	board := []BoardIssue{
		{ID: "LA", Description: "x\n\n" + RenderMarker("a", aSHA)},
		// b already blocked-by a (LA), so the relation must NOT be re-emitted.
		{ID: "LB", Description: "y\n\n" + RenderMarker("b", bSHA), BlockedBy: []string{"LA"}},
	}
	p := mustBuildPlan(t, spec, board)
	if len(p.Relations) != 0 {
		t.Errorf("relations = %v, want none (already present)", p.Relations)
	}
}

func TestBuildPlanRejectsDuplicateBoardRefs(t *testing.T) {
	spec := &Spec{Team: "CLI", Issues: []Issue{{Ref: "a", Title: "A", Body: "x"}}}
	board := []BoardIssue{
		{ID: "L1", Identifier: "CLI-1", Description: "x\n\n" + RenderMarker("a", "one")},
		{ID: "L2", Identifier: "CLI-2", Description: "x\n\n" + RenderMarker("a", "two")},
	}
	_, err := BuildPlan(spec, board)
	if err == nil || !strings.Contains(err.Error(), "CLI-1") || !strings.Contains(err.Error(), "CLI-2") {
		t.Fatalf("want duplicate-marker error naming both issues, got %v", err)
	}
}

func TestBuildPlanUpdatesAfterEffectiveLabelChange(t *testing.T) {
	is := Issue{Ref: "a", Title: "A", Body: "x"}
	oldSpec := &Spec{Team: "CLI", DefaultLabels: []string{"agent:coder"}, Issues: []Issue{is}}
	newSpec := &Spec{Team: "CLI", DefaultLabels: []string{"agent:reviewer"}, Issues: []Issue{is}}
	board := []BoardIssue{{ID: "L1", Identifier: "CLI-1", Description: WithBody(oldSpec, is)}}
	p, err := BuildPlan(newSpec, board)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Issues[0].Action != Update {
		t.Fatalf("action = %v, want update after default-label change", p.Issues[0].Action)
	}

	humanSpec := &Spec{Team: "CLI", DefaultLabels: []string{"agent:coder"}, Issues: []Issue{{Ref: "a", Title: "A", Body: "x", Human: true}}}
	p, err = BuildPlan(humanSpec, board)
	if err != nil {
		t.Fatalf("BuildPlan human: %v", err)
	}
	if p.Issues[0].Action != Update {
		t.Fatalf("human action = %v, want update after human toggle", p.Issues[0].Action)
	}
}

func TestBuildPlanReportsStaleManagedRelation(t *testing.T) {
	spec := &Spec{Team: "CLI", Issues: []Issue{
		{Ref: "a", Title: "A", Body: "x"},
		{Ref: "b", Title: "B", Body: "y"},
	}}
	board := []BoardIssue{
		{ID: "LA", Identifier: "CLI-1", Description: WithBody(spec, spec.Issues[0])},
		{ID: "LB", Identifier: "CLI-2", Description: WithBody(spec, spec.Issues[1]), BlockedBy: []string{"LA"}},
	}
	p := mustBuildPlan(t, spec, board)
	if len(p.StaleRelations) != 1 || p.StaleRelations[0].FromRef != "b" || p.StaleRelations[0].ToRef != "a" {
		t.Fatalf("stale relations = %v, want [{b a}]", p.StaleRelations)
	}
}
