package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// gitWorkspacer is the production Workspacer: it wraps spawn.EnsureWorktree
// against the dispatcher's configured repo clone and base branch, rooted at
// worktreeRoot (typically <boardDir>/worktrees).
type gitWorkspacer struct {
	primaryClonePath string
	baseBranch       string
	worktreeRoot     string
}

// NewGitWorkspacer returns a Workspacer that creates/reuses git worktrees for
// primaryClonePath (the dispatcher's single managed repo clone), branching
// off baseBranch, rooted at worktreeRoot.
func NewGitWorkspacer(primaryClonePath, baseBranch, worktreeRoot string) Workspacer {
	return &gitWorkspacer{
		primaryClonePath: primaryClonePath,
		baseBranch:       baseBranch,
		worktreeRoot:     worktreeRoot,
	}
}

// Ensure creates (or reuses) the worktree for issue.BranchName. A blank
// BranchName is a configuration/data error the caller should surface rather
// than silently falling back to the primary clone.
func (w *gitWorkspacer) Ensure(issue store.Issue) (string, error) {
	if issue.BranchName == "" {
		return "", fmt.Errorf("ensuring workspace for issue %s: no branch name set", issue.ID)
	}
	path, err := spawn.EnsureWorktree(context.Background(), w.primaryClonePath, issue.BranchName, w.baseBranch, w.worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("ensuring workspace for issue %s: %w", issue.ID, err)
	}
	return path, nil
}

// Remove idempotently removes issue's deterministic worktree path and local
// branch through the same spawn primitive used by Git-operator cleanup.
func (w *gitWorkspacer) Remove(issue store.Issue) error {
	if issue.BranchName == "" {
		return fmt.Errorf("removing workspace for issue %s: no branch name set", issue.ID)
	}
	path := filepath.Join(w.worktreeRoot, strings.ReplaceAll(issue.BranchName, "/", "-"))
	if err := spawn.RemoveWorktree(context.Background(), w.primaryClonePath, path, issue.BranchName); err != nil {
		return fmt.Errorf("removing workspace for issue %s: %w", issue.ID, err)
	}
	return nil
}

// marshalWorkerResult is a small helper shared by applyResult so both the
// "continue" and terminal-outcome branches serialize the same way.
func marshalWorkerResult(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshaling worker result: %w", err)
	}
	return string(b), nil
}
