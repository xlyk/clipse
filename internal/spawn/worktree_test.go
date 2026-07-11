package spawn_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xlyk/clipse/internal/spawn"
)

// runGit runs a git command with dir as its working directory, failing the
// test with combined output on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newPrimaryRepo creates a real git repo in t.TempDir(), configures a
// throwaway identity, and makes an initial commit on baseBranch so the repo
// has at least one ref other worktrees can be created from.
func newPrimaryRepo(t *testing.T, baseBranch string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", baseBranch)
	runGit(t, dir, "config", "user.name", "Clipse Test")
	runGit(t, dir, "config", "user.email", "clipse-test@example.com")

	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("clipse test fixture\n"), 0o644); err != nil {
		t.Fatalf("writing README.md: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial commit")

	return dir
}

// currentBranch returns the checked-out branch name in dir.
func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return runGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// branchExists reports whether branch appears in `git branch` output in dir.
func branchExists(t *testing.T, dir, branch string) bool {
	t.Helper()
	out := runGit(t, dir, "branch", "--list", branch)
	return strings.TrimSpace(out) != ""
}

// worktreeCount returns the number of entries in `git worktree list` for
// dir's repo.
func worktreeCount(t *testing.T, dir string) int {
	t.Helper()
	out := runGit(t, dir, "worktree", "list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}
	return len(lines)
}

func ctxWithTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestEnsureWorktree_CreatesNewBranch asserts EnsureWorktree creates a new
// worktree directory checked out on the requested branch, branching it off
// baseBranch, when no worktree for that branch exists yet.
func TestEnsureWorktree_CreatesNewBranch(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktreeRoot := t.TempDir()

	path, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, "clp-123-add-widget", "main", worktreeRoot)
	if err != nil {
		t.Fatalf("EnsureWorktree: unexpected error: %v", err)
	}

	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("worktree path %s does not exist: %v", path, statErr)
	}
	if got := currentBranch(t, path); got != "clp-123-add-widget" {
		t.Errorf("checked-out branch = %q, want %q", got, "clp-123-add-widget")
	}
	if !strings.HasPrefix(path, worktreeRoot) {
		t.Errorf("worktree path %s is not under worktreeRoot %s", path, worktreeRoot)
	}
}

// cloneRemote creates a separate working clone of remote with a throwaway
// identity, suitable for publishing commits into a bare test origin.
func cloneRemote(t *testing.T, remote, name string) string {
	t.Helper()
	parent := t.TempDir()
	clone := filepath.Join(parent, name)
	runGit(t, parent, "clone", remote, clone)
	runGit(t, clone, "config", "user.name", "Clipse Test")
	runGit(t, clone, "config", "user.email", "clipse-test@example.com")
	return clone
}

// newRemoteAndClone builds a real bare "origin" repo (with an initial commit
// on baseBranch) and a separate primary clone of it, mirroring the
// dispatcher's managed-clone-with-a-remote topology. It returns both paths.
func newRemoteAndClone(t *testing.T, baseBranch string) (origin, primary string) {
	t.Helper()
	seed := newPrimaryRepo(t, baseBranch)
	origin = filepath.Join(t.TempDir(), "origin.git")
	runGit(t, filepath.Dir(origin), "clone", "--bare", seed, origin)
	primary = cloneRemote(t, origin, "primary")
	return origin, primary
}

// TestEnsureWorktree_BranchesFromFetchedRemoteBase asserts a new worktree is
// branched from the REMOTE base tip (fetched at creation time), not the
// primary clone's stale clone-time local base: a commit landing on origin's
// base branch AFTER the clone must appear in a freshly-created worktree. The
// Reflex build ran an external fast-forward cron as a workaround because the
// kernel never fetched.
func TestEnsureWorktree_BranchesFromFetchedRemoteBase(t *testing.T) {
	origin, primary := newRemoteAndClone(t, "main")
	publisher := cloneRemote(t, origin, "publisher")

	// A commit on origin's main AFTER the clone, so the primary clone's local
	// main is now stale relative to the remote.
	writeFile(t, publisher, "post-clone.txt", "landed after the clone\n")
	runGit(t, publisher, "add", "post-clone.txt")
	runGit(t, publisher, "commit", "-m", "post-clone change on origin main")
	runGit(t, publisher, "push", "origin", "HEAD:main")
	wantSHA := runGit(t, publisher, "rev-parse", "HEAD")

	worktreeRoot := t.TempDir()
	path, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, "clp-1-fresh", "main", worktreeRoot)
	if err != nil {
		t.Fatalf("EnsureWorktree: unexpected error: %v", err)
	}

	if got := runGit(t, path, "rev-parse", "HEAD"); got != wantSHA {
		t.Errorf("worktree HEAD = %q, want the post-clone origin tip %q (branched from stale local base?)", got, wantSHA)
	}
	if _, err := os.Stat(filepath.Join(path, "post-clone.txt")); err != nil {
		t.Errorf("post-clone.txt missing from worktree: %v (worktree branched from stale local base)", err)
	}
}

// TestEnsureWorktree_UsesRemoteFeatureBranchWhenLocalBranchMissing asserts a
// Daytona-created feature branch that exists only on origin is fetched and
// used as the new local worktree branch's start point. The separate publisher
// clone and explicit push ensure this fixture exercises a real remote ref,
// rather than merely changing a non-bare origin's checked-out working tree.
func TestEnsureWorktree_UsesRemoteFeatureBranchWhenLocalBranchMissing(t *testing.T) {
	origin, primary := newRemoteAndClone(t, "main")
	daytona := cloneRemote(t, origin, "daytona")

	const branch = "feat/CLI-1"
	runGit(t, daytona, "checkout", "-b", branch)
	writeFile(t, daytona, "daytona.txt", "remote agent commit\n")
	runGit(t, daytona, "add", "daytona.txt")
	runGit(t, daytona, "commit", "-m", "feat: remote change")
	wantSHA := runGit(t, daytona, "rev-parse", "HEAD")
	runGit(t, daytona, "push", "origin", "HEAD:refs/heads/"+branch)

	if branchExists(t, primary, branch) {
		t.Fatalf("precondition: primary clone unexpectedly has local branch %s", branch)
	}
	if got := runGit(t, primary, "ls-remote", "--heads", "origin", "refs/heads/"+branch); !strings.Contains(got, wantSHA) {
		t.Fatalf("precondition: remote feature ref = %q, want SHA %s", got, wantSHA)
	}

	path, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, branch, "main", t.TempDir())
	if err != nil {
		t.Fatalf("EnsureWorktree: unexpected error: %v", err)
	}
	if got := runGit(t, path, "rev-parse", "HEAD"); got != wantSHA {
		t.Fatalf("worktree HEAD = %s, want remote feature %s", got, wantSHA)
	}
	if got := runGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Errorf("worktree branch = %q, want %q", got, branch)
	}
}

// writeFile writes name (relative to dir) with contents, failing the test on
// error.
func writeFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// TestEnsureWorktree_ReusesExisting asserts a second EnsureWorktree call for
// the same branch reuses the existing worktree: same path, no error, and no
// duplicate worktree entry created.
func TestEnsureWorktree_ReusesExisting(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktreeRoot := t.TempDir()

	path1, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, "clp-123-add-widget", "main", worktreeRoot)
	if err != nil {
		t.Fatalf("EnsureWorktree (first call): unexpected error: %v", err)
	}

	// Simulate on-disk progress from a prior turn: this file must survive
	// the second EnsureWorktree call.
	marker := filepath.Join(path1, "progress.txt")
	if err := os.WriteFile(marker, []byte("turn 1 progress\n"), 0o644); err != nil {
		t.Fatalf("writing marker file: %v", err)
	}

	countBefore := worktreeCount(t, primary)

	path2, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, "clp-123-add-widget", "main", worktreeRoot)
	if err != nil {
		t.Fatalf("EnsureWorktree (second call): unexpected error: %v", err)
	}

	if path2 != path1 {
		t.Errorf("second EnsureWorktree path = %q, want same as first %q", path2, path1)
	}
	if countBefore != worktreeCount(t, primary) {
		t.Errorf("worktree count changed on reuse: before=%d after=%d", countBefore, worktreeCount(t, primary))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("on-disk progress marker missing after reuse: %v", err)
	}
}

// TestEnsureWorktree_PrunesAndRetriesOnMissingButRegisteredCollision asserts
// that when a worktree's directory was removed directly (e.g. os.RemoveAll,
// bypassing `git worktree remove`/RemoveWorktree) rather than through the
// normal cleanup path, a later EnsureWorktree call for the same branch
// recovers instead of failing forever. git's own .git/worktrees
// administrative metadata still claims the path even though the directory
// itself is gone, so a bare retry of `git worktree add` fails with "is a
// missing but already registered worktree" (exit 128) every time; the fix is
// `git worktree prune` on the primary clone, then one retry of the same add.
func TestEnsureWorktree_PrunesAndRetriesOnMissingButRegisteredCollision(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktreeRoot := t.TempDir()

	path, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, "clp-999-recover", "main", worktreeRoot)
	if err != nil {
		t.Fatalf("EnsureWorktree (first call): unexpected error: %v", err)
	}

	// Simulate the directory vanishing without going through
	// RemoveWorktree (a crashed cleanup, a clobbered disk, an operator's
	// stray rm -rf): git's own worktree registration survives even though
	// the directory doesn't.
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("os.RemoveAll(%s): unexpected error: %v", path, err)
	}

	path2, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, "clp-999-recover", "main", worktreeRoot)
	if err != nil {
		t.Fatalf("EnsureWorktree (second call, after directory removed): unexpected error: %v", err)
	}
	if path2 != path {
		t.Errorf("recovered worktree path = %q, want same as first call's %q", path2, path)
	}
	if _, statErr := os.Stat(path2); statErr != nil {
		t.Errorf("recovered worktree path %s does not exist: %v", path2, statErr)
	}
	if got := currentBranch(t, path2); got != "clp-999-recover" {
		t.Errorf("recovered worktree checked-out branch = %q, want %q", got, "clp-999-recover")
	}
}

// TestRemoveWorktree_DeletesWorktreeAndBranch asserts RemoveWorktree removes
// the worktree directory and deletes the local branch.
func TestRemoveWorktree_DeletesWorktreeAndBranch(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktreeRoot := t.TempDir()

	path, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, "clp-456-fix-bug", "main", worktreeRoot)
	if err != nil {
		t.Fatalf("EnsureWorktree: unexpected error: %v", err)
	}

	if err := spawn.RemoveWorktree(ctxWithTimeout(t), primary, path, "clp-456-fix-bug"); err != nil {
		t.Fatalf("RemoveWorktree: unexpected error: %v", err)
	}

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("worktree path %s still exists after RemoveWorktree (stat err: %v)", path, statErr)
	}
	if branchExists(t, primary, "clp-456-fix-bug") {
		t.Errorf("branch clp-456-fix-bug still exists after RemoveWorktree")
	}
}

// TestRemoveWorktree_AlreadyRemovedIsNotError asserts calling RemoveWorktree
// twice (or on a worktree/branch that's already gone) does not error, so
// terminal-state cleanup can be safely retried.
func TestRemoveWorktree_AlreadyRemovedIsNotError(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktreeRoot := t.TempDir()

	path, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, "clp-789-cleanup", "main", worktreeRoot)
	if err != nil {
		t.Fatalf("EnsureWorktree: unexpected error: %v", err)
	}

	if err := spawn.RemoveWorktree(ctxWithTimeout(t), primary, path, "clp-789-cleanup"); err != nil {
		t.Fatalf("RemoveWorktree (first call): unexpected error: %v", err)
	}

	// Second call: worktree dir and branch are already gone.
	if err := spawn.RemoveWorktree(ctxWithTimeout(t), primary, path, "clp-789-cleanup"); err != nil {
		t.Errorf("RemoveWorktree (second call, already removed): unexpected error: %v", err)
	}
}

// TestWorktreeLifecycle_NoLeaks asserts a full create-then-remove cycle
// across multiple issues leaves no leaked worktrees registered against the
// primary repo.
func TestWorktreeLifecycle_NoLeaks(t *testing.T) {
	primary := newPrimaryRepo(t, "main")
	worktreeRoot := t.TempDir()

	branches := []string{"clp-1-a", "clp-2-b", "clp-3-c"}
	paths := make([]string, 0, len(branches))
	for _, b := range branches {
		path, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, b, "main", worktreeRoot)
		if err != nil {
			t.Fatalf("EnsureWorktree(%s): unexpected error: %v", b, err)
		}
		paths = append(paths, path)
	}

	if got, want := worktreeCount(t, primary), len(branches)+1; got != want {
		t.Fatalf("worktree count after creation = %d, want %d (primary + %d issue worktrees)", got, want, len(branches))
	}

	for i, b := range branches {
		if err := spawn.RemoveWorktree(ctxWithTimeout(t), primary, paths[i], b); err != nil {
			t.Fatalf("RemoveWorktree(%s): unexpected error: %v", b, err)
		}
	}

	if got, want := worktreeCount(t, primary), 1; got != want {
		t.Errorf("worktree count after cleanup = %d, want %d (primary only, no leaks)", got, want)
	}
}
