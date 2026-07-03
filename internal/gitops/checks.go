package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ghCheck is one entry of `gh pr checks --json name,bucket`'s array
// output. bucket categorizes a check's state into "pass", "fail",
// "pending", "skipping", or "cancel" (gh's own documented field, distinct
// from the raw provider-specific "state" string).
type ghCheck struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
}

// checksState is classifyChecks' verdict over a PR's required checks.
type checksState int

const (
	// checksGreen means every required check passed (or was skipped).
	checksGreen checksState = iota
	// checksAbsent means no required checks were reported at all.
	checksAbsent
	// checksFailing means at least one required check failed or was
	// cancelled.
	checksFailing
	// checksPending means no check has failed, but at least one is still
	// running.
	checksPending
)

// classifyChecks reduces checks to a single checksState plus the names
// relevant to that state (failing check names for checksFailing, pending
// check names for checksPending; nil otherwise). A failing check always
// wins over a pending one: "at least one required check will never pass"
// is a stronger signal than "some checks haven't finished yet".
func classifyChecks(checks []ghCheck) (checksState, []string) {
	if len(checks) == 0 {
		return checksAbsent, nil
	}

	var failing []string
	for _, c := range checks {
		if c.Bucket == "fail" || c.Bucket == "cancel" {
			failing = append(failing, c.Name)
		}
	}
	if len(failing) > 0 {
		return checksFailing, failing
	}

	var pending []string
	for _, c := range checks {
		if c.Bucket == "pending" {
			pending = append(pending, c.Name)
		}
	}
	if len(pending) > 0 {
		return checksPending, pending
	}

	return checksGreen, nil
}

// fetchChecks runs `gh pr checks --required` for spec.Branch and decodes
// its --json response into []ghCheck.
//
// gh pr checks exits non-zero (1 for a failing check, 8 for a pending one)
// even when it successfully reported checks via --json, so exit code alone
// can't distinguish "gh itself failed" from "the checks aren't all green" --
// this only ever treats a genuinely empty stdout as a failure. A
// legitimately empty *result* (no required checks configured at all) still
// has non-empty stdout: gh writes the literal JSON array "[]", which
// decodes to a zero-length slice and classifyChecks reports as
// checksAbsent, not an error.
func fetchChecks(ctx context.Context, spec Spec, runner CommandRunner) ([]ghCheck, error) {
	argv := []string{"gh", "pr", "checks", spec.Branch, "--required", "--json", "name,bucket"}
	res, err := runner(ctx, argv, spec.Workspace)
	if err != nil {
		return nil, fmt.Errorf("gh pr checks %s: %w", spec.Branch, err)
	}

	stdout := strings.TrimSpace(res.Stdout)
	if stdout == "" {
		return nil, fmt.Errorf("gh pr checks %s: exit %d: %s", spec.Branch, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	var checks []ghCheck
	if err := json.Unmarshal([]byte(stdout), &checks); err != nil {
		return nil, fmt.Errorf("parsing gh pr checks output for branch %s: %w", spec.Branch, err)
	}
	return checks, nil
}
