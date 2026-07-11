package board

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// fakeLinear records the calls Apply makes so tests can assert order + payloads.
type fakeLinear struct {
	labelsEnsured []string
	created       []CreateInput // in call order
	updated       map[string]UpdateInput
	relations     [][2]string // {dependentID, blockerID}
	nextID        int
}

func newFakeLinear() *fakeLinear { return &fakeLinear{updated: map[string]UpdateInput{}} }

func (f *fakeLinear) EnsureLabels(_ context.Context, names []string) error {
	f.labelsEnsured = names
	return nil
}
func (f *fakeLinear) StartStateID(context.Context) (string, error) { return "state-ready", nil }
func (f *fakeLinear) CreateIssue(_ context.Context, in CreateInput) (string, error) {
	f.created = append(f.created, in)
	f.nextID++
	return fmt.Sprintf("L%d", f.nextID), nil
}
func (f *fakeLinear) UpdateIssue(_ context.Context, id string, in UpdateInput) error {
	f.updated[id] = in
	return nil
}
func (f *fakeLinear) AddBlockedBy(_ context.Context, dep, blk string) error {
	f.relations = append(f.relations, [2]string{dep, blk})
	return nil
}

func TestApplyCreatesInDepOrderWithMarkersAndRelations(t *testing.T) {
	spec := &Spec{Team: "CLI", DefaultLabels: []string{"agent:coder"}, Issues: []Issue{
		{Ref: "b", Title: "B", Body: "bb", Deps: []string{"a"}},
		{Ref: "a", Title: "A", Body: "aa"},
		{Ref: "h", Title: "H", Body: "hh", Human: true, Deps: []string{"a"}},
	}}
	p := BuildPlan(spec, nil) // all create; relations b->a, h->a
	f := newFakeLinear()
	if err := Apply(context.Background(), f, spec, p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Created in topo order: A before B and H (both depend on a).
	gotTitles := []string{f.created[0].Title, f.created[1].Title, f.created[2].Title}
	if !slices.Equal(gotTitles, []string{"A", "B", "H"}) {
		t.Errorf("create order = %v, want [A B H]", gotTitles)
	}
	// Every created description carries its ref marker.
	if !strings.Contains(f.created[0].Description, "clipse-ref: a") {
		t.Errorf("A description missing marker: %q", f.created[0].Description)
	}
	// Human issue gets only the human label; agent issues get agent:coder.
	if !slices.Equal(f.created[2].Labels, []string{"human"}) {
		t.Errorf("H labels = %v, want [human]", f.created[2].Labels)
	}
	if !slices.Equal(f.created[0].Labels, []string{"agent:coder"}) {
		t.Errorf("A labels = %v, want [agent:coder]", f.created[0].Labels)
	}
	// Labels ensured (sorted).
	if !slices.Equal(f.labelsEnsured, []string{"agent:coder", "human"}) {
		t.Errorf("ensured = %v, want [agent:coder human]", f.labelsEnsured)
	}
	// Relations wired with resolved ids: A=L1, B=L2, H=L3 → B->A, H->A.
	want := [][2]string{{"L2", "L1"}, {"L3", "L1"}}
	if !slices.Equal(f.relations, want) {
		t.Errorf("relations = %v, want %v", f.relations, want)
	}
}

func TestApplyUpdatesChangedIssue(t *testing.T) {
	spec := &Spec{Team: "CLI", Issues: []Issue{{Ref: "a", Title: "A", Body: "new"}}}
	board := []BoardIssue{{ID: "LA", Description: "old\n\n" + RenderMarker("a", "stale000")}}
	p := BuildPlan(spec, board)
	f := newFakeLinear()
	if err := Apply(context.Background(), f, spec, p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.created) != 0 {
		t.Errorf("expected no creates, got %d", len(f.created))
	}
	up, ok := f.updated["LA"]
	if !ok || !strings.Contains(up.Description, "clipse-ref: a") {
		t.Errorf("expected LA updated with refreshed marker, got %+v", f.updated)
	}
}
