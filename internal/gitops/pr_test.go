package gitops

import (
	"context"
	"errors"
	"testing"
)

func TestFetchPRView_ParsesJSON(t *testing.T) {
	runner := fakeRunner(CommandResult{
		ExitCode: 0,
		Stdout:   `{"number":42,"url":"https://github.com/x/y/pull/42","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN"}`,
	}, nil)

	view, err := fetchPRView(context.Background(), Spec{Branch: "clp-1"}, runner)
	if err != nil {
		t.Fatalf("fetchPRView: unexpected error: %v", err)
	}
	want := prView{Number: 42, URL: "https://github.com/x/y/pull/42", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}
	if view != want {
		t.Errorf("fetchPRView() = %+v, want %+v", view, want)
	}
}

func TestFetchPRView_NonZeroExitIsError(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 1, Stderr: "no pull requests found for branch \"clp-1\""}, nil)
	if _, err := fetchPRView(context.Background(), Spec{Branch: "clp-1"}, runner); err == nil {
		t.Fatal("fetchPRView: expected an error, got nil")
	}
}

func TestFetchPRView_RunnerErrorPropagates(t *testing.T) {
	runner := fakeRunner(CommandResult{}, errors.New("boom"))
	if _, err := fetchPRView(context.Background(), Spec{Branch: "clp-1"}, runner); err == nil {
		t.Fatal("fetchPRView: expected an error, got nil")
	}
}

func TestNeedsBaseUpdate(t *testing.T) {
	tests := []struct {
		name string
		view prView
		want bool
	}{
		{"behind", prView{MergeStateStatus: "BEHIND"}, true},
		{"clean", prView{MergeStateStatus: "CLEAN"}, false},
		{"dirty", prView{MergeStateStatus: "DIRTY"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsBaseUpdate(tt.view); got != tt.want {
				t.Errorf("needsBaseUpdate(%+v) = %v, want %v", tt.view, got, tt.want)
			}
		})
	}
}

func TestHasConflict(t *testing.T) {
	tests := []struct {
		name string
		view prView
		want bool
	}{
		{"conflicting mergeable", prView{Mergeable: "CONFLICTING", MergeStateStatus: "BEHIND"}, true},
		{"dirty state", prView{Mergeable: "UNKNOWN", MergeStateStatus: "DIRTY"}, true},
		{"clean", prView{Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}, false},
		{"behind only", prView{Mergeable: "MERGEABLE", MergeStateStatus: "BEHIND"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasConflict(tt.view); got != tt.want {
				t.Errorf("hasConflict(%+v) = %v, want %v", tt.view, got, tt.want)
			}
		})
	}
}

func TestMergeFlag(t *testing.T) {
	tests := []struct {
		method string
		want   string
	}{
		{"", "--squash"},
		{"squash", "--squash"},
		{"merge", "--merge"},
		{"rebase", "--rebase"},
		{"bogus", "--squash"},
	}
	for _, tt := range tests {
		if got := mergeFlag(tt.method); got != tt.want {
			t.Errorf("mergeFlag(%q) = %q, want %q", tt.method, got, tt.want)
		}
	}
}

func TestUpdateBranch_NonZeroExitIsError(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 1, Stderr: "could not update pull request branch"}, nil)
	if _, err := updateBranch(context.Background(), Spec{Branch: "clp-1"}, runner); err == nil {
		t.Fatal("updateBranch: expected an error, got nil")
	}
}

func TestMergePR_NonZeroExitIsNotAGoError(t *testing.T) {
	// mergePR deliberately does NOT error on a non-zero exit -- Run maps
	// that to OutcomeNotMergeable using the captured output, the same way
	// it treats failing checks or unsatisfied protection.
	runner := fakeRunner(CommandResult{ExitCode: 1, Stderr: "not all required checks have passed"}, nil)
	res, err := mergePR(context.Background(), Spec{Branch: "clp-1"}, runner)
	if err != nil {
		t.Fatalf("mergePR: unexpected error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("mergePR() ExitCode = %d, want 1", res.ExitCode)
	}
}
