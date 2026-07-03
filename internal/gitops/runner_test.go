package gitops

import (
	"context"
	"strings"
	"testing"
)

// TestDefaultCommandRunner_CapturesExitZero asserts a successful command
// reports ExitCode 0 and captures its stdout, with no error.
func TestDefaultCommandRunner_CapturesExitZero(t *testing.T) {
	dir := t.TempDir()
	res, err := DefaultCommandRunner(context.Background(), []string{"echo", "-n", "hello"}, dir)
	if err != nil {
		t.Fatalf("DefaultCommandRunner: unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hello" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hello")
	}
}

// TestDefaultCommandRunner_CapturesNonZeroExit asserts a command that exits
// non-zero is reported via CommandResult.ExitCode/Stderr, NOT as a Go error
// -- mirroring the coder graph's Python CommandRunner (subprocess.run
// without check=True), so callers can branch on exit code without a
// type-switch on error.
func TestDefaultCommandRunner_CapturesNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	res, err := DefaultCommandRunner(context.Background(), []string{"sh", "-c", "echo boom >&2; exit 3"}, dir)
	if err != nil {
		t.Fatalf("DefaultCommandRunner: unexpected error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Errorf("Stderr = %q, want it to contain %q", res.Stderr, "boom")
	}
}

// TestDefaultCommandRunner_RunsInDir asserts the command runs with dir as
// its working directory.
func TestDefaultCommandRunner_RunsInDir(t *testing.T) {
	dir := t.TempDir()
	res, err := DefaultCommandRunner(context.Background(), []string{"pwd"}, dir)
	if err != nil {
		t.Fatalf("DefaultCommandRunner: unexpected error: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != dir {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

// TestDefaultCommandRunner_MissingBinaryIsError asserts a binary that
// cannot be found or started (as opposed to one that runs and exits
// non-zero) surfaces as a genuine Go error, since no CommandResult can
// meaningfully describe "never ran".
func TestDefaultCommandRunner_MissingBinaryIsError(t *testing.T) {
	dir := t.TempDir()
	_, err := DefaultCommandRunner(context.Background(), []string{"clipse-gitops-nonexistent-binary-xyz"}, dir)
	if err == nil {
		t.Fatal("DefaultCommandRunner: expected an error for a missing binary, got nil")
	}
}

// TestDefaultCommandRunner_EmptyArgvIsError asserts an empty argv is
// rejected before ever touching exec.Command.
func TestDefaultCommandRunner_EmptyArgvIsError(t *testing.T) {
	if _, err := DefaultCommandRunner(context.Background(), nil, t.TempDir()); err == nil {
		t.Fatal("DefaultCommandRunner: expected an error for empty argv, got nil")
	}
}
