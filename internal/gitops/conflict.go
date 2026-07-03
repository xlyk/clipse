package gitops

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// probeConflictFiles discovers which files conflict between spec's branch
// (checked out in spec.Workspace) and spec.BaseBranch, by actually
// attempting the merge locally and inspecting the result -- gh/GitHub
// exposes no API that lists a PR's conflicting files, so a real test-merge
// is the only faithful way to name them for a rework comment.
//
// This is real git, not the injectable CommandRunner: like
// internal/spawn's worktree lifecycle, it never leaves the local clone (the
// worktree and spec.BaseBranch share one repo's object store/refs), so
// tests exercise it against a real temporary repo rather than a fake.
//
// The probe never leaves the worktree in a half-merged state: on either
// outcome, it aborts the test merge before returning, so a caller that
// goes on to route the issue to Rework hands the Coder lane back a clean
// worktree to keep working in.
func probeConflictFiles(ctx context.Context, spec Spec) ([]string, error) {
	defer func() {
		// Best-effort: nothing further can be done if the abort itself
		// fails, and the merge attempt below already reports its own
		// error when relevant.
		_ = runGit(ctx, spec.Workspace, "merge", "--abort")
	}()

	mergeErr := runGit(ctx, spec.Workspace, "merge", "--no-commit", "--no-ff", spec.BaseBranch)
	if mergeErr == nil {
		// Locally clean even though gh reported a conflict remotely (e.g.
		// GitHub's own cached mergeability was stale). Report no files;
		// the caller still routes to Rework on gh's signal, since a
		// worktree merge succeeding locally doesn't retroactively make the
		// hosted PR mergeable.
		return nil, nil
	}

	out, statusErr := runGitOutput(ctx, spec.Workspace, "diff", "--name-only", "--diff-filter=U")
	if statusErr != nil {
		return nil, statusErr
	}
	return splitNonEmptyLines(out), nil
}

// runGit runs `git <args...>` with dir as its working directory, returning
// an error that wraps combined output so failures are debuggable from the
// returned error alone. Mirrors internal/spawn's private runGitCmd helper
// (kept package-local here rather than imported/exported across packages).
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runGitOutput runs `git <args...>` with dir as its working directory and
// returns its stdout.
func runGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// splitNonEmptyLines splits s on newlines, dropping empty lines (including
// the trailing one every git command's output ends with).
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
