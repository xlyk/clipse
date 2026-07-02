package gitops

import (
	"context"
	"testing"
)

// fakeRunner returns a CommandRunner that ignores argv/dir and always
// replays result (or err), for tests that only care about how this
// package's parsing/classification code reacts to a given CommandResult --
// not about exact argv shape (covered separately by the PATH-shim
// integration tests in gitops_test.go).
func fakeRunner(result CommandResult, err error) CommandRunner {
	return func(_ context.Context, _ []string, _ string) (CommandResult, error) {
		return result, err
	}
}

func TestClassifyChecks(t *testing.T) {
	tests := []struct {
		name        string
		checks      []ghCheck
		wantState   checksState
		wantNamesLn int
	}{
		{
			name:      "absent",
			checks:    nil,
			wantState: checksAbsent,
		},
		{
			name:        "all green",
			checks:      []ghCheck{{Name: "build", Bucket: "pass"}, {Name: "lint", Bucket: "skipping"}},
			wantState:   checksGreen,
			wantNamesLn: 0,
		},
		{
			name:        "one failing wins over pending",
			checks:      []ghCheck{{Name: "build", Bucket: "pass"}, {Name: "test", Bucket: "fail"}, {Name: "lint", Bucket: "pending"}},
			wantState:   checksFailing,
			wantNamesLn: 1,
		},
		{
			name:        "cancelled counts as failing",
			checks:      []ghCheck{{Name: "build", Bucket: "cancel"}},
			wantState:   checksFailing,
			wantNamesLn: 1,
		},
		{
			name:        "pending with no failures",
			checks:      []ghCheck{{Name: "build", Bucket: "pass"}, {Name: "test", Bucket: "pending"}},
			wantState:   checksPending,
			wantNamesLn: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, names := classifyChecks(tt.checks)
			if state != tt.wantState {
				t.Errorf("classifyChecks() state = %v, want %v", state, tt.wantState)
			}
			if len(names) != tt.wantNamesLn {
				t.Errorf("classifyChecks() names = %v, want length %d", names, tt.wantNamesLn)
			}
		})
	}
}

// TestFetchChecks_ParsesJSONArray asserts fetchChecks decodes a gh
// pr-checks --json response into []ghCheck.
func TestFetchChecks_ParsesJSONArray(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 0, Stdout: `[{"name":"build","bucket":"pass"},{"name":"test","bucket":"fail"}]`}, nil)
	checks, err := fetchChecks(context.Background(), Spec{Branch: "clp-1"}, runner)
	if err != nil {
		t.Fatalf("fetchChecks: unexpected error: %v", err)
	}
	if len(checks) != 2 || checks[0].Name != "build" || checks[1].Bucket != "fail" {
		t.Errorf("fetchChecks() = %+v, want 2 decoded checks", checks)
	}
}

// TestFetchChecks_EmptyArrayIsAbsent asserts a literal "[]" response (no
// required checks configured at all) decodes as a zero-length slice, not
// an error -- gh pr checks can exit non-zero even on this legitimate
// "nothing to report" response, so a non-zero exit alone must not be
// confused with a real gh failure.
func TestFetchChecks_EmptyArrayIsAbsent(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 8, Stdout: "[]", Stderr: ""}, nil)
	checks, err := fetchChecks(context.Background(), Spec{Branch: "clp-1"}, runner)
	if err != nil {
		t.Fatalf("fetchChecks: unexpected error: %v", err)
	}
	if len(checks) != 0 {
		t.Errorf("fetchChecks() = %+v, want empty", checks)
	}
}

// TestFetchChecks_NonZeroExitWithNoOutputIsError asserts a genuine gh
// failure (nonzero exit, nothing usable on stdout, something on stderr) is
// surfaced as an error rather than silently treated as "absent".
func TestFetchChecks_NonZeroExitWithNoOutputIsError(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 1, Stdout: "", Stderr: "no pull requests found"}, nil)
	if _, err := fetchChecks(context.Background(), Spec{Branch: "clp-1"}, runner); err == nil {
		t.Fatal("fetchChecks: expected an error, got nil")
	}
}

// TestFetchChecks_MalformedJSONIsError asserts unparseable stdout is
// surfaced as an error rather than swallowed.
func TestFetchChecks_MalformedJSONIsError(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 0, Stdout: "not json"}, nil)
	if _, err := fetchChecks(context.Background(), Spec{Branch: "clp-1"}, runner); err == nil {
		t.Fatal("fetchChecks: expected an error, got nil")
	}
}

// TestFetchChecks_RunnerErrorPropagates asserts a CommandRunner-level error
// (e.g. gh not found) propagates rather than being swallowed.
func TestFetchChecks_RunnerErrorPropagates(t *testing.T) {
	wantErr := context.DeadlineExceeded
	runner := fakeRunner(CommandResult{}, wantErr)
	if _, err := fetchChecks(context.Background(), Spec{Branch: "clp-1"}, runner); err == nil {
		t.Fatal("fetchChecks: expected an error, got nil")
	}
}
