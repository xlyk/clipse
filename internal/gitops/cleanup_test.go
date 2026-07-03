package gitops

import (
	"os"
	"testing"
)

// TestTagRelease_CreatesLocalTag asserts tagRelease creates a lightweight
// tag at the branch's current tip.
func TestTagRelease_CreatesLocalTag(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktree := newIssueWorktree(t, primary, "clp-1-feature", "main")

	spec := Spec{Workspace: worktree, Branch: "clp-1-feature", Tag: "v0.1.0"}
	if err := tagRelease(ctxWithTimeout(t), spec); err != nil {
		t.Fatalf("tagRelease: unexpected error: %v", err)
	}

	tagged := runGitT(t, worktree, "tag", "--points-at", "HEAD")
	if tagged != "v0.1.0" {
		t.Errorf("git tag --points-at HEAD = %q, want %q", tagged, "v0.1.0")
	}
}

// TestRemoveWorktree_RemovesWorktreeAndBranch asserts removeWorktree
// deletes both the worktree directory and the local branch, via
// spawn.RemoveWorktree.
func TestRemoveWorktree_RemovesWorktreeAndBranch(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktree := newIssueWorktree(t, primary, "clp-1-feature", "main")

	spec := Spec{
		Workspace:        worktree,
		Branch:           "clp-1-feature",
		PrimaryClonePath: primary,
	}
	if err := removeWorktree(ctxWithTimeout(t), spec); err != nil {
		t.Fatalf("removeWorktree: unexpected error: %v", err)
	}

	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("worktree %s still exists after removeWorktree (stat err: %v)", worktree, err)
	}
	if branchExistsT(t, primary, "clp-1-feature") {
		t.Error("branch clp-1-feature still exists after removeWorktree")
	}
}
