package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// checkBranchProtection reports whether spec.BaseBranch has branch
// protection configured on GitHub, via `gh api
// repos/{owner}/{repo}/branches/<base>/protection` ({owner}/{repo} are gh's
// own placeholders, resolved from spec.Workspace's git remote -- see `gh
// help api`). This intentionally does not inspect the protection payload's
// required-review-count/strict-checks details: required CI status is
// already gated separately by fetchChecks/classifyChecks, so "protection
// satisfied" here means exactly "GitHub reports the base branch is
// protected at all" -- an unprotected base branch is never an acceptable
// auto-merge target, protection specifics notwithstanding.
//
// A non-2xx response (most commonly a 404, "Branch not protected") makes
// `gh api` exit non-zero; that -- or a response that doesn't even decode
// as a non-empty JSON object -- is reported as unsatisfied, never as an
// error: an unprotected branch is an entirely ordinary, expected outcome,
// not an infrastructure failure.
func checkBranchProtection(ctx context.Context, spec Spec, runner CommandRunner) (ok bool, reason string, err error) {
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/branches/%s/protection", spec.BaseBranch)
	res, err := runner(ctx, []string{"gh", "api", endpoint}, spec.Workspace)
	if err != nil {
		return false, "", fmt.Errorf("gh api %s: %w", endpoint, err)
	}
	if res.ExitCode != 0 {
		return false, firstNonEmpty(strings.TrimSpace(res.Stderr), strings.TrimSpace(res.Stdout), "branch protection is not configured"), nil
	}

	var payload map[string]any
	if jsonErr := json.Unmarshal([]byte(res.Stdout), &payload); jsonErr != nil || len(payload) == 0 {
		return false, "branch protection response was empty or malformed", nil
	}
	return true, "", nil
}
