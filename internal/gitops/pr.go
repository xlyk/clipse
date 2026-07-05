package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// prView is the subset of `gh pr view --json ...` fields Run needs: state
// ("OPEN" | "CLOSED" | "MERGED"), mergeable ("MERGEABLE" | "CONFLICTING" |
// "UNKNOWN"), and mergeStateStatus ("BEHIND" | "BLOCKED" | "CLEAN" |
// "DIRTY" | "DRAFT" | "HAS_HOOKS" | "UNKNOWN" | "UNSTABLE") are gh/GitHub's
// own enum values, used verbatim.
type prView struct {
	Number           int    `json:"number"`
	URL              string `json:"url"`
	State            string `json:"state"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	IsDraft          bool   `json:"isDraft"`
}

// fetchPRView runs `gh pr view` for spec.Branch and decodes its --json
// response. Unlike fetchChecks, a non-zero exit here always means a real
// failure (most commonly: no PR exists for this branch), so it's always an
// error -- there is no analogous "empty is a meaningful, non-error result"
// case.
func fetchPRView(ctx context.Context, spec Spec, runner CommandRunner) (prView, error) {
	argv := []string{"gh", "pr", "view", spec.Branch, "--json", "number,url,state,mergeable,mergeStateStatus,isDraft"}
	res, err := runner(ctx, argv, spec.Workspace)
	if err != nil {
		return prView{}, fmt.Errorf("gh pr view %s: %w", spec.Branch, err)
	}
	if res.ExitCode != 0 {
		return prView{}, fmt.Errorf("gh pr view %s: exit %d: %s", spec.Branch, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	var v prView
	if err := json.Unmarshal([]byte(res.Stdout), &v); err != nil {
		return prView{}, fmt.Errorf("parsing gh pr view output for branch %s: %w", spec.Branch, err)
	}
	return v, nil
}

// needsBaseUpdate reports whether view's PR is merely behind its base --
// cleanly catchable via an update, not yet conflicting.
func needsBaseUpdate(view prView) bool {
	return view.MergeStateStatus == "BEHIND"
}

// hasConflict reports whether view's PR conflicts with its base. GitHub's
// mergeStateStatus only ever reports "DIRTY" when a merge commit can't be
// cleanly created against the current base, so this is definitionally the
// same condition Run treats as a stale-base conflict.
func hasConflict(view prView) bool {
	return view.Mergeable == "CONFLICTING" || view.MergeStateStatus == "DIRTY"
}

// mergeabilityUnknown reports GitHub's transient UNKNOWN state: the
// mergeability computation was invalidated (typically by a concurrent merge
// advancing the base) and hasn't finished recomputing. A merge attempt now
// is guaranteed to be refused; the only correct move is to re-check next poll.
func mergeabilityUnknown(view prView) bool {
	return view.Mergeable == "UNKNOWN"
}

// updateBranch runs `gh pr update-branch` for spec.Branch, asking GitHub to
// bring it up to date with its base (a merge commit by default; gh has no
// separate rebase mode wired here since Run always re-checks mergeability
// afterward regardless of which strategy GitHub used).
func updateBranch(ctx context.Context, spec Spec, runner CommandRunner) (CommandResult, error) {
	argv := []string{"gh", "pr", "update-branch", spec.Branch}
	res, err := runner(ctx, argv, spec.Workspace)
	if err != nil {
		return CommandResult{}, fmt.Errorf("gh pr update-branch %s: %w", spec.Branch, err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("gh pr update-branch %s: exit %d: %s", spec.Branch, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res, nil
}

// mergeFlag maps a Spec.MergeMethod value to its `gh pr merge` flag,
// defaulting to --squash (a clean, linear history) when unset.
func mergeFlag(method string) string {
	switch method {
	case "merge":
		return "--merge"
	case "rebase":
		return "--rebase"
	default:
		return "--squash"
	}
}

// readyPR runs `gh pr ready` to convert a draft PR to ready-for-review.
// The Coder lane opens PRs as drafts (project convention), but `gh pr merge`
// refuses a draft ("Pull Request is still a draft" -- caught by the live
// full-pipeline smoke), so Run marks a draft ready immediately before merging.
// Called only when view.IsDraft, so a non-draft PR is never touched (gh
// errors when readying a PR that is already ready for review).
func readyPR(ctx context.Context, spec Spec, runner CommandRunner) error {
	argv := []string{"gh", "pr", "ready", spec.Branch}
	res, err := runner(ctx, argv, spec.Workspace)
	if err != nil {
		return fmt.Errorf("gh pr ready %s: %w", spec.Branch, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("gh pr ready %s: exit %d: %s", spec.Branch, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// mergePR runs `gh pr merge` for spec.Branch. A non-zero exit is not
// treated as a Go error here -- Run maps it to OutcomeNotMergeable using
// the captured output as the reason, the same as a failing-checks or
// unsatisfied-protection verdict, since "gh refused to merge" is exactly
// as ordinary an outcome as those.
//
// When the spec carries both an issue id and title, mergePR adds an explicit
// --subject "<lower(issueID)>: <title> (#<pr>)" so the squash commit's
// subject reads from the issue rather than the PR title (which is coder
// narration). Both empty leaves gh's default (the PR title).
func mergePR(ctx context.Context, spec Spec, prNumber int, runner CommandRunner) (CommandResult, error) {
	argv := []string{"gh", "pr", "merge", spec.Branch, mergeFlag(spec.MergeMethod)}
	if spec.IssueID != "" && spec.IssueTitle != "" {
		subject := fmt.Sprintf("%s: %s (#%d)", strings.ToLower(spec.IssueID), spec.IssueTitle, prNumber)
		argv = append(argv, "--subject", subject)
	}
	res, err := runner(ctx, argv, spec.Workspace)
	if err != nil {
		return CommandResult{}, fmt.Errorf("gh pr merge %s: %w", spec.Branch, err)
	}
	return res, nil
}
