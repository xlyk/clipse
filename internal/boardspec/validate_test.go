package boardspec

import (
	"strings"
	"testing"
)

func specWith(issues ...Issue) *Spec { return &Spec{Team: "CLI", Issues: issues} }

func TestValidateRejectsDuplicateRef(t *testing.T) {
	s := specWith(
		Issue{Ref: "a", Title: "A", Body: "x"},
		Issue{Ref: "a", Title: "A2", Body: "y"},
	)
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate ref") {
		t.Fatalf("want duplicate ref error, got %v", err)
	}
}

func TestValidateRejectsUndefinedDep(t *testing.T) {
	s := specWith(Issue{Ref: "a", Title: "A", Body: "x", Deps: []string{"ghost"}})
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want undefined dep error, got %v", err)
	}
}

func TestValidateRejectsCycle(t *testing.T) {
	s := specWith(
		Issue{Ref: "a", Title: "A", Body: "x", Deps: []string{"b"}},
		Issue{Ref: "b", Title: "B", Body: "y", Deps: []string{"a"}},
	)
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestValidateRejectsMissingBody(t *testing.T) {
	s := specWith(Issue{Ref: "a", Title: "A"})
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "body") {
		t.Fatalf("want missing body error, got %v", err)
	}
}

func TestValidateAcceptsValidDAGAndTopoOrders(t *testing.T) {
	s := specWith(
		Issue{Ref: "b", Title: "B", Body: "y", Deps: []string{"a"}},
		Issue{Ref: "a", Title: "A", Body: "x"},
	)
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	order, err := s.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	// "a" (index 1) must come before "b" (index 0).
	if order[0] != 1 || order[1] != 0 {
		t.Errorf("order = %v, want [1 0]", order)
	}
}
