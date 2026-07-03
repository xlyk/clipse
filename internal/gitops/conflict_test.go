package gitops

import (
	"strings"
	"testing"
)

// TestProbeConflictFiles_NoConflict asserts a base-branch change that
// doesn't touch any file the issue branch touched probes clean (no
// conflicting files) and leaves the worktree in a clean, mergeable state
// afterward.
func TestProbeConflictFiles_NoConflict(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktree := newIssueWorktree(t, primary, "clp-1-feature", "main")

	// Advance main with an unrelated change (newIssueWorktree already
	// committed feature.txt on the branch before this).
	writeFileT(t, primary, "unrelated.txt", "someone else's change\n")
	runGitT(t, primary, "add", "unrelated.txt")
	runGitT(t, primary, "commit", "-m", "unrelated change on main")

	files, err := probeConflictFiles(ctxWithTimeout(t), Spec{Workspace: worktree, BaseBranch: "main"})
	if err != nil {
		t.Fatalf("probeConflictFiles: unexpected error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("probeConflictFiles() = %v, want no conflicting files", files)
	}

	if status := runGitT(t, worktree, "status", "--porcelain"); status != "" {
		t.Errorf("worktree not clean after probe: %q", status)
	}
	assertNoMergeInProgress(t, worktree)
}

// TestProbeConflictFiles_Conflict asserts a base-branch change that
// conflicts with the issue branch's own change to the same file is
// reported by name, and the worktree is left clean (merge aborted)
// afterward so a Rework re-run can use it.
func TestProbeConflictFiles_Conflict(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktree := newIssueWorktree(t, primary, "clp-2-feature", "main")

	// The issue branch's own change to README.md.
	writeFileT(t, worktree, "README.md", "issue branch changed this line\n")
	runGitT(t, worktree, "commit", "-am", "issue branch edits README")

	// A conflicting change to the SAME line on main.
	writeFileT(t, primary, "README.md", "main branch changed this line differently\n")
	runGitT(t, primary, "commit", "-am", "main edits README differently")

	files, err := probeConflictFiles(ctxWithTimeout(t), Spec{Workspace: worktree, BaseBranch: "main"})
	if err != nil {
		t.Fatalf("probeConflictFiles: unexpected error: %v", err)
	}
	if len(files) != 1 || files[0] != "README.md" {
		t.Errorf("probeConflictFiles() = %v, want [README.md]", files)
	}

	if status := runGitT(t, worktree, "status", "--porcelain"); status != "" {
		t.Errorf("worktree not clean after probe: %q", status)
	}
	assertNoMergeInProgress(t, worktree)
}

// assertNoMergeInProgress fails the test if dir's repo is left mid-merge
// (MERGE_HEAD still set), which would block a later Coder rework turn from
// committing anything.
func assertNoMergeInProgress(t *testing.T, dir string) {
	t.Helper()
	out := runGitOutputT(t, dir, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	if strings.TrimSpace(out) != "" {
		t.Errorf("MERGE_HEAD still set in %s after probe: %q", dir, out)
	}
}

// runGitOutputT runs a git command that is allowed to exit non-zero
// (e.g. `rev-parse --verify` on a ref that doesn't exist), returning
// whatever it printed on stdout without failing the test on a non-zero
// exit.
func runGitOutputT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, _ := runGitOutput(ctxWithTimeout(t), dir, args...)
	return out
}
