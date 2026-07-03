package gitops

import (
	"context"
	"testing"
)

// TestCheckBranchProtection_Satisfied asserts a successful `gh api
// .../protection` response (a non-empty JSON object, exit 0) is reported
// as satisfied.
func TestCheckBranchProtection_Satisfied(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 0, Stdout: `{"required_status_checks":{"strict":true,"contexts":[]}}`}, nil)
	ok, reason, err := checkBranchProtection(context.Background(), Spec{BaseBranch: "main"}, runner)
	if err != nil {
		t.Fatalf("checkBranchProtection: unexpected error: %v", err)
	}
	if !ok {
		t.Errorf("checkBranchProtection() ok = false, want true (reason: %q)", reason)
	}
}

// TestCheckBranchProtection_Unprotected asserts a 404-style failure (gh
// exits non-zero) is reported as unsatisfied, not an error -- an
// unprotected branch is an ordinary outcome, not an infrastructure
// failure.
func TestCheckBranchProtection_Unprotected(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 1, Stderr: "HTTP 404: Branch not protected"}, nil)
	ok, reason, err := checkBranchProtection(context.Background(), Spec{BaseBranch: "main"}, runner)
	if err != nil {
		t.Fatalf("checkBranchProtection: unexpected error: %v", err)
	}
	if ok {
		t.Error("checkBranchProtection() ok = true, want false for a 404 response")
	}
	if reason == "" {
		t.Error("checkBranchProtection() reason is empty, want the underlying gh message")
	}
}

// TestCheckBranchProtection_MalformedResponseIsUnsatisfied asserts a
// zero-exit but empty/unparseable response is treated as unsatisfied
// (fail closed), not as satisfied and not as an error.
func TestCheckBranchProtection_MalformedResponseIsUnsatisfied(t *testing.T) {
	runner := fakeRunner(CommandResult{ExitCode: 0, Stdout: ""}, nil)
	ok, _, err := checkBranchProtection(context.Background(), Spec{BaseBranch: "main"}, runner)
	if err != nil {
		t.Fatalf("checkBranchProtection: unexpected error: %v", err)
	}
	if ok {
		t.Error("checkBranchProtection() ok = true, want false for an empty response")
	}
}

// TestCheckBranchProtection_RunnerErrorPropagates asserts a
// CommandRunner-level error is not swallowed.
func TestCheckBranchProtection_RunnerErrorPropagates(t *testing.T) {
	runner := fakeRunner(CommandResult{}, context.DeadlineExceeded)
	if _, _, err := checkBranchProtection(context.Background(), Spec{BaseBranch: "main"}, runner); err == nil {
		t.Fatal("checkBranchProtection: expected an error, got nil")
	}
}
