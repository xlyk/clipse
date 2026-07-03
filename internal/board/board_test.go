package board

import (
	"errors"
	"testing"

	"github.com/xlyk/clipse/internal/contract"
)

// allOutcomes and allColumns enumerate every enum value from internal/contract
// so the table-driven test below can assert on the full (outcome x column)
// cross product — every pair must be either a documented legal transition or
// an illegal-transition error, with nothing left unspecified.
var allOutcomes = []string{
	string(contract.WorkerResultOutcomeDone),
	string(contract.WorkerResultOutcomeNeedsReview),
	string(contract.WorkerResultOutcomeChangesRequested),
	string(contract.WorkerResultOutcomeBlocked),
	string(contract.WorkerResultOutcomeContinue),
}

var allColumns = []string{
	string(contract.ColumnTodo),
	string(contract.ColumnReady),
	string(contract.ColumnRunning),
	string(contract.ColumnReview),
	string(contract.ColumnMerging),
	string(contract.ColumnDone),
	string(contract.ColumnRework),
	string(contract.ColumnBlocked),
}

// legalCase is one legal (outcome, current) -> (next, action) transition from
// the design doc's "Board & state machine" table.
type legalCase struct {
	outcome string
	current string
	next    string
	action  string
}

// legalTransitions is the full set of legal transitions this test asserts.
// Any (outcome, current) pair not listed here is expected to be illegal (see
// TestNext_AllPairsCovered, which cross-checks this against allOutcomes x
// allColumns).
var legalTransitions = []legalCase{
	// Running: Coder lane finishes a turn.
	{outcome: "needs_review", current: "running", next: "review", action: ActionOpenReview},
	{outcome: "blocked", current: "running", next: "blocked", action: ActionCommentBlock},
	{outcome: "continue", current: "running", next: "running", action: ActionRespawn},

	// Review: Reviewer lane passes judgment.
	{outcome: "done", current: "review", next: "merging", action: ActionMerge},
	{outcome: "changes_requested", current: "review", next: "rework", action: ActionRequestChanges},
	{outcome: "blocked", current: "review", next: "blocked", action: ActionCommentBlock},

	// Rework: Coder lane re-runs after review feedback.
	{outcome: "needs_review", current: "rework", next: "review", action: ActionOpenReview},
	{outcome: "blocked", current: "rework", next: "blocked", action: ActionCommentBlock},
	{outcome: "continue", current: "rework", next: "rework", action: ActionRespawn},

	// Merging: Git-operator lane lands the PR; a merge goes straight to Done
	// (documentation is written inside the Coder turn, no separate stage).
	{outcome: "done", current: "merging", next: "done", action: ActionComplete},
	{outcome: "blocked", current: "merging", next: "blocked", action: ActionCommentBlock},
	// Merging -> Rework: the Git-operator lane's stale-base-conflict route
	// (internal/gitops.OutcomeStaleBaseConflict maps to changes_requested
	// from merging, the only board.Next entry that lands on rework from
	// this column) -- a base update landed a real, unresolvable conflict,
	// so the Coder lane gets another attempt rather than parking the issue
	// in Blocked outright.
	{outcome: "changes_requested", current: "merging", next: "rework", action: ActionRequestChanges},
}

func legalKey(outcome, current string) string { return outcome + "|" + current }

func TestNext_LegalTransitions(t *testing.T) {
	for _, tc := range legalTransitions {
		t.Run(tc.outcome+"_from_"+tc.current, func(t *testing.T) {
			next, action, err := Next(tc.outcome, tc.current)
			if err != nil {
				t.Fatalf("Next(%q, %q) returned unexpected error: %v", tc.outcome, tc.current, err)
			}
			if next != tc.next {
				t.Errorf("Next(%q, %q) next = %q, want %q", tc.outcome, tc.current, next, tc.next)
			}
			if action != tc.action {
				t.Errorf("Next(%q, %q) action = %q, want %q", tc.outcome, tc.current, action, tc.action)
			}
		})
	}
}

// TestNext_AllPairsCovered exhaustively checks every (outcome, current) pair
// drawn from the contract enums: pairs in legalTransitions must succeed and
// match exactly; every other pair must return a non-nil error and leave
// next/action zeroed.
func TestNext_AllPairsCovered(t *testing.T) {
	legal := make(map[string]legalCase, len(legalTransitions))
	for _, tc := range legalTransitions {
		legal[legalKey(tc.outcome, tc.current)] = tc
	}

	for _, outcome := range allOutcomes {
		for _, current := range allColumns {
			outcome, current := outcome, current
			t.Run(outcome+"_from_"+current, func(t *testing.T) {
				next, action, err := Next(outcome, current)
				want, ok := legal[legalKey(outcome, current)]
				if ok {
					if err != nil {
						t.Fatalf("Next(%q, %q) returned unexpected error: %v", outcome, current, err)
					}
					if next != want.next || action != want.action {
						t.Errorf("Next(%q, %q) = (%q, %q), want (%q, %q)", outcome, current, next, action, want.next, want.action)
					}
					return
				}
				if err == nil {
					t.Fatalf("Next(%q, %q) = (%q, %q), want illegal-transition error", outcome, current, next, action)
				}
				if next != "" || action != "" {
					t.Errorf("Next(%q, %q) error path returned non-zero (next=%q, action=%q)", outcome, current, next, action)
				}
			})
		}
	}
}

func TestNext_ErrorNamesOutcomeAndCurrent(t *testing.T) {
	_, _, err := Next("done", "running")
	if err == nil {
		t.Fatal("Next(done, running) expected an illegal-transition error, got nil")
	}
	if !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("Next(done, running) error should wrap ErrIllegalTransition, got: %v", err)
	}
	msg := err.Error()
	if !containsAll(msg, "done", "running") {
		t.Errorf("Next(done, running) error %q should name both the outcome and the current column", msg)
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestPromote(t *testing.T) {
	tests := []struct {
		name    string
		current string
		deps    []DepState
		want    bool
	}{
		{
			name:    "todo with no deps promotes",
			current: "todo",
			deps:    nil,
			want:    true,
		},
		{
			name:    "todo with empty deps slice promotes",
			current: "todo",
			deps:    []DepState{},
			want:    true,
		},
		{
			name:    "todo with all terminal deps promotes",
			current: "todo",
			deps: []DepState{
				{Terminal: true},
				{Terminal: true},
			},
			want: true,
		},
		{
			name:    "todo with one non-terminal dep does not promote",
			current: "todo",
			deps: []DepState{
				{Terminal: true},
				{Terminal: false},
			},
			want: false,
		},
		{
			name:    "todo with all non-terminal deps does not promote",
			current: "todo",
			deps: []DepState{
				{Terminal: false},
			},
			want: false,
		},
		{
			name:    "non-todo current never promotes, even with all deps terminal",
			current: "ready",
			deps: []DepState{
				{Terminal: true},
			},
			want: false,
		},
		{
			name:    "non-todo current with no deps does not promote",
			current: "running",
			deps:    nil,
			want:    false,
		},
		{
			name:    "blocked current does not promote",
			current: "blocked",
			deps: []DepState{
				{Terminal: true},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Promote(tc.current, tc.deps)
			if got != tc.want {
				t.Errorf("Promote(%q, %v) = %v, want %v", tc.current, tc.deps, got, tc.want)
			}
		})
	}
}
