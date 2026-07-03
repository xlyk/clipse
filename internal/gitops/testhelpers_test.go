package gitops

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

// ctxWithTimeout mirrors internal/spawn's test helper of the same name: a
// bounded context so a hung git/gh invocation fails the test instead of
// the suite.
func ctxWithTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// runGitT runs a git command with dir as its working directory, failing
// the test with combined output on error. Mirrors internal/spawn's
// worktree_test.go helper of the same name/shape.
func runGitT(t *testing.T, dir string, args ...string) string {
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
// throwaway identity, and commits a README on baseBranch, exactly like
// internal/spawn's worktree_test.go fixture -- gitops's stale-base conflict
// probe and worktree cleanup are real local git operations too.
func newPrimaryRepo(t *testing.T, baseBranch string) string {
	t.Helper()
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", baseBranch)
	runGitT(t, dir, "config", "user.name", "Clipse Test")
	runGitT(t, dir, "config", "user.email", "clipse-test@example.com")

	writeFileT(t, dir, "README.md", "clipse gitops test fixture\n")
	runGitT(t, dir, "add", "README.md")
	runGitT(t, dir, "commit", "-m", "initial commit")
	return dir
}

// writeFileT writes name (relative to dir) with contents, failing the test
// on error.
func writeFileT(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// newIssueWorktree creates a worktree+branch for branch off baseBranch in
// primary (via the real spawn.EnsureWorktree, the same primitive the
// dispatcher uses), then commits one change on it so it has content
// distinct from base.
func newIssueWorktree(t *testing.T, primary, branch, baseBranch string) string {
	t.Helper()
	worktreeRoot := t.TempDir()
	path, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, branch, baseBranch, worktreeRoot)
	if err != nil {
		t.Fatalf("EnsureWorktree(%s): unexpected error: %v", branch, err)
	}
	writeFileT(t, path, "feature.txt", branch+" work\n")
	runGitT(t, path, "add", "feature.txt")
	runGitT(t, path, "commit", "-m", "add "+branch)
	return path
}

// branchExistsT reports whether branch appears in `git branch` output in
// dir.
func branchExistsT(t *testing.T, dir, branch string) bool {
	t.Helper()
	out := runGitT(t, dir, "branch", "--list", branch)
	return out != ""
}

// worktreeCountT returns the number of entries in `git worktree list` for
// dir's repo.
func worktreeCountT(t *testing.T, dir string) int {
	t.Helper()
	out := runGitT(t, dir, "worktree", "list")
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

// writeFakeGh writes the fake `gh` PATH-shim script (fakeGhScript, defined
// in gitops_test.go) into dir as an executable named "gh", and returns dir.
// Tests prepend dir to PATH (via t.Setenv) so Run's DefaultCommandRunner --
// the real one, unmodified -- resolves "gh" to this script instead of a
// real gh binary. Only gh is faked this way; git commands run for real
// against the temp repos these tests build.
func writeFakeGh(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(fakeGhScript), 0o755); err != nil {
		t.Fatalf("writing fake gh script: %v", err)
	}
}

// prependPath puts dir first on PATH for the duration of the test.
func prependPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
