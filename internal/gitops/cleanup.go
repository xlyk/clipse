package gitops

import (
	"context"
	"fmt"

	"github.com/xlyk/clipse/internal/spawn"
)

// tagRelease creates a local git tag named spec.Tag at spec.Branch's
// current tip in spec.Workspace. Local-only (no push): the design doc
// calls tagging "optional", and a personal single-repo tool has no
// consumer for a pushed tag yet -- extend this when one exists rather than
// speculatively wiring a push with no caller to test it against.
func tagRelease(ctx context.Context, spec Spec) error {
	return runGit(ctx, spec.Workspace, "tag", spec.Tag)
}

// removeWorktree tears down spec.Workspace and deletes spec.Branch locally,
// via spawn.RemoveWorktree -- the same primitive dispatcher.Workspacer.Remove
// wraps for the terminal-transition cleanup path. Run calls this only after
// a successful merge (design doc decision F): a Blocked or Rework outcome
// must leave the worktree in place, since Rework re-dispatches the Coder
// lane onto it and a human-requeued Blocked issue's next Coder claim
// reuses it via the same EnsureWorktree idempotency.
func removeWorktree(ctx context.Context, spec Spec) error {
	if err := spawn.RemoveWorktree(ctx, spec.PrimaryClonePath, spec.Workspace, spec.Branch); err != nil {
		return fmt.Errorf("removing worktree %s for branch %s: %w", spec.Workspace, spec.Branch, err)
	}
	return nil
}
