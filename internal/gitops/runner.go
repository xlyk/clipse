package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// CommandResult is the outcome of running one git/gh command. Mirrors the
// Coder graph's Python dataclass of the same name/shape
// (agent/src/clipse_agent/graphs/coder.py's CommandResult) so the two
// planes' subprocess seams read the same way.
type CommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// CommandRunner is an injectable stand-in for exec.Command: given an argv
// and a working directory, run it and report what happened. Production
// code (Run's default) uses DefaultCommandRunner, a real subprocess
// invocation; tests substitute a fake `gh` on PATH and still call
// DefaultCommandRunner (see the package's *_test.go files) so the exact
// argv-building and output-parsing logic in this package stays under test
// -- nothing about gh invocation is mocked away, only gh itself is faked.
//
// Only `gh` calls need faking in tests: they are this package's sole
// network/GitHub touchpoint. Local git operations (worktree/branch
// cleanup, the stale-base conflict probe) run against a real temporary
// repo instead, matching internal/spawn's own test convention.
type CommandRunner func(ctx context.Context, argv []string, dir string) (CommandResult, error)

// DefaultCommandRunner is the production CommandRunner: it execs argv[0]
// with the remaining elements as arguments, resolved against PATH exactly
// as exec.Command does, with dir as the working directory.
//
// A command that runs and exits non-zero is reported via
// CommandResult.ExitCode/Stderr with a nil error -- mirroring
// subprocess.run without check=True on the Python side -- because callers
// in this package branch on exit code/output for perfectly ordinary
// outcomes (a failing CI check, an unprotected branch). Only a failure to
// even start the command (missing binary, permission error, a cancelled
// ctx) is a Go error, since no CommandResult can describe a command that
// never ran.
func DefaultCommandRunner(ctx context.Context, argv []string, dir string) (CommandResult, error) {
	if len(argv) == 0 {
		return CommandResult{}, errors.New("gitops: DefaultCommandRunner: empty argv")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return CommandResult{}, fmt.Errorf("running %s: %w", strings.Join(argv, " "), runErr)
	}

	exitCode := 0
	if exitErr != nil {
		exitCode = exitErr.ExitCode()
	}
	return CommandResult{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}
