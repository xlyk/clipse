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

// TestFetchChecks_NoChecksReportedIsAbsent asserts gh's other "no checks"
// shape -- exit 1 with EMPTY stdout and a "no checks reported on the
// '<branch>' branch" stderr, distinct from the documented "[]" stdout -- is
// treated as zero checks (absent), not an error. On a repo whose CI
// registers late, gh emits this form on every PR until the first check
// registers; erroring here loops the merging claim forever (Reflex retro,
// failure category 1).
func TestFetchChecks_NoChecksReportedIsAbsent(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 1, Stdout: "", Stderr: "no checks reported on the 'kylehanks/ref-1-x' branch"}, nil)
	checks, err := fetchChecks(context.Background(), Spec{Branch: "clp-1"}, runner)
	if err != nil {
		t.Fatalf("fetchChecks: expected no error for 'no checks reported', got: %v", err)
	}
	if len(checks) != 0 {
		t.Errorf("fetchChecks() = %+v, want zero checks", checks)
	}
}

// TestFetchChecks_NoRequiredChecksReportedIsAbsent asserts the message gh
// actually emits with the `--required` flag fetchChecks always passes: on a
// conflicting/draft PR (no CI merge commit) gh exits 1 with empty stdout and
// "no required checks reported on the '<branch>' branch". The prior parser
// matched only "no checks reported" (a DIFFERENT string -- "no required
// checks" never contains "no checks"), so it fell through to a hard error
// and the dispatcher retried the merging claim every poll forever
// (2026-07-09 Spacelift: SPA-857/859 conflicted with the advanced base;
// gitops errored before conflictBeforeCIWait could route them to rework).
func TestFetchChecks_NoRequiredChecksReportedIsAbsent(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 1, Stdout: "", Stderr: "no required checks reported on the 'kyle/spa-857-x' branch"}, nil)
	checks, err := fetchChecks(context.Background(), Spec{Branch: "spa-857"}, runner)
	if err != nil {
		t.Fatalf("fetchChecks: expected no error for 'no required checks reported', got: %v", err)
	}
	if len(checks) != 0 {
		t.Errorf("fetchChecks() = %+v, want zero checks", checks)
	}
}

// TestFetchChecks_EmptyStdoutOtherErrorStillFails asserts any other
// empty-stdout failure (without the no-checks marker) is still surfaced as an
// error, so a genuine gh outage isn't silently read as "no checks".
func TestFetchChecks_EmptyStdoutOtherErrorStillFails(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 1, Stdout: "", Stderr: "gh: connection refused"}, nil)
	if _, err := fetchChecks(context.Background(), Spec{Branch: "clp-1"}, runner); err == nil {
		t.Fatal("fetchChecks: expected an error for empty stdout without the no-checks marker")
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
