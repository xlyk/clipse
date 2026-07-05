package spawn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnsureWorktree returns the deterministic path (derived from branch, see
// worktreePathFor) of a git worktree for branch off the primary clone at
// primaryClonePath, creating it if necessary.
//
// If a worktree already exists at that path, it is reused as-is: this is
// what lets a continuation turn pick up on-disk progress a prior turn left
// behind, so EnsureWorktree is idempotent by design, not just by accident.
//
// If no worktree exists yet, EnsureWorktree creates one, branching a new
// local branch off baseBranch when branch doesn't already exist locally, or
// checking out the existing local branch into the new worktree otherwise.
func EnsureWorktree(ctx context.Context, primaryClonePath, branch, baseBranch, worktreeRoot string) (string, error) {
	path := worktreePathFor(worktreeRoot, branch)

	if _, err := os.Stat(path); err == nil {
		// Reuse: a worktree directory already exists for this branch, so
		// trust it rather than re-running `git worktree add` (which would
		// fail against a non-empty target anyway).
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("checking for existing worktree at %s: %w", path, err)
	}

	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		return "", fmt.Errorf("creating worktree root %s: %w", worktreeRoot, err)
	}

	branchExistsLocally, err := localBranchExists(ctx, primaryClonePath, branch)
	if err != nil {
		return "", err
	}

	var args []string
	if branchExistsLocally {
		args = []string{"worktree", "add", path, branch}
	} else {
		// Branch from the REMOTE base tip, not the local ref: the primary
		// clone's local base only advances when something fetches it, and
		// nothing in the kernel did -- the Reflex build ran an external
		// fast-forward cron as a workaround, so dependents kept building on a
		// stale base. A fetch failure (offline dev, no reachable remote)
		// falls back to the local ref rather than failing the spawn.
		base := baseBranch
		if err := runGitCmd(ctx, primaryClonePath, "fetch", "origin", baseBranch); err == nil {
			base = "origin/" + baseBranch
		}
		args = []string{"worktree", "add", "-b", branch, path, base}
	}
	if err := runGitCmd(ctx, primaryClonePath, args...); err != nil {
		if !isMissingButRegisteredWorktreeErr(err) {
			return "", fmt.Errorf("creating worktree for branch %s at %s: %w", branch, path, err)
		}
		// The target directory was removed directly (e.g. os.RemoveAll,
		// bypassing RemoveWorktree/`git worktree remove`), but git's own
		// .git/worktrees administrative metadata still claims path, so the
		// add above fails with "is a missing but already registered
		// worktree" every time until that stale registration is pruned.
		// Prune once and retry the exact same add; a failure past that point
		// is a real error, not this recoverable collision.
		if pruneErr := runGitCmd(ctx, primaryClonePath, "worktree", "prune"); pruneErr != nil {
			return "", fmt.Errorf("pruning stale worktree registrations for branch %s at %s: %w", branch, path, pruneErr)
		}
		if retryErr := runGitCmd(ctx, primaryClonePath, args...); retryErr != nil {
			return "", fmt.Errorf("creating worktree for branch %s at %s (retry after prune): %w", branch, path, retryErr)
		}
	}

	return path, nil
}

// RemoveWorktree removes the git worktree at worktreePath and deletes the
// local branch, for use on terminal states (Done/Cancelled). It tolerates a
// worktree or branch that is already gone (e.g. a retried cleanup), so
// callers can call it unconditionally without checking prior state.
func RemoveWorktree(ctx context.Context, primaryClonePath, worktreePath, branch string) error {
	if err := runGitCmd(ctx, primaryClonePath, "worktree", "remove", "--force", worktreePath); err != nil {
		if !isAlreadyGoneWorktreeErr(err) {
			return fmt.Errorf("removing worktree %s: %w", worktreePath, err)
		}
	}

	if err := runGitCmd(ctx, primaryClonePath, "branch", "-D", branch); err != nil {
		if !isAlreadyGoneBranchErr(err) {
			return fmt.Errorf("deleting branch %s: %w", branch, err)
		}
	}

	return nil
}

// worktreePathFor derives a deterministic worktree path from branch, so
// callers can compute (or anticipate) a worktree's path without asking git.
// Branch names may contain '/' (e.g. "clp/123-add-widget"), which is not
// safe as a single path segment, so every '/' is replaced with '-'.
func worktreePathFor(worktreeRoot, branch string) string {
	sanitized := strings.ReplaceAll(branch, "/", "-")
	return filepath.Join(worktreeRoot, sanitized)
}

// localBranchExists reports whether branch already exists as a local branch
// in the repo at primaryClonePath.
func localBranchExists(ctx context.Context, primaryClonePath, branch string) (bool, error) {
	err := runGitCmd(ctx, primaryClonePath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		// show-ref --verify exits 1 (with no stderr) when the ref simply
		// doesn't exist, distinct from other failures.
		return false, nil
	}
	return false, fmt.Errorf("checking whether branch %s exists: %w", branch, err)
}

// runGitCmd runs `git <args...>` with dir as its working directory,
// returning an error that wraps the command's stderr output so failures are
// debuggable from the returned error alone.
func runGitCmd(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isMissingButRegisteredWorktreeErr reports whether a `git worktree add`
// failure is git's "missing but already registered worktree" error (exit
// 128): the target path's on-disk directory is gone, but git's own
// .git/worktrees metadata still has it registered, so EnsureWorktree can
// recover by pruning that stale registration and retrying once, rather than
// treating this the same as any other add failure.
func isMissingButRegisteredWorktreeErr(err error) bool {
	return strings.Contains(err.Error(), "is a missing but already registered worktree")
}

// isAlreadyGoneWorktreeErr reports whether a `git worktree remove` failure
// indicates the worktree is already gone (rather than some other failure
// RemoveWorktree should surface).
func isAlreadyGoneWorktreeErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "is not a working tree") ||
		strings.Contains(msg, "not a valid path") ||
		strings.Contains(msg, "No such file or directory")
}

// isAlreadyGoneBranchErr reports whether a `git branch -D` failure indicates
// the branch is already gone (rather than some other failure RemoveWorktree
// should surface).
func isAlreadyGoneBranchErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not found")
}
