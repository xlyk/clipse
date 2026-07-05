# Retro Quick Fixes + Handoff Comments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the mechanical failure classes the Reflex MVP build exposed (see `reflex-v2/docs/clipse-retro.md`) and add per-run handoff comments with claim-time injection.

**Architecture:** Three surfaces. (1) Go kernel: `internal/gitops` outcome classification, `dispatcher` orphan recovery and block-kind mapping, `internal/spawn` base-ref freshness. (2) Python agent: `dac.py` last-message extraction, coder structured output tail (TITLE/STATUS/HANDOFF) with blocked routing, reviewer severity protocol. (3) The handoff loop: a new `handoff` field flows Python → contract → dispatcher → Linear comment, and comments flow back in via a new env var at spawn time.

**Tech Stack:** Go 1.25 (table-driven tests, `internal/gitops` CommandRunner stubs), Python 3.14 + uv + pytest (`agent/tests/`), Linear GraphQL via `internal/linear`.

## Global Constraints

- TDD: every behavior change lands with a failing test first.
- Go: wrap errors with context (`fmt.Errorf("doing x: %w", err)`); table-driven tests; match existing doc-comment density in `internal/gitops`.
- Python: type hints on signatures; pytest; run `cd agent && uv run ruff check` before each commit.
- Commits: lowercase terse conventional, one concern per commit.
- Never log or echo `LINEAR_API_KEY` or any credential.
- Verify suites: `go test ./...` from repo root; `cd agent && uv run pytest -q`.
- The contract is triple-sourced: `schema/worker-result.schema.json` + `internal/contract/contract.go` + `agent/src/clipse_agent/contract.py` must change together (Task 12).

---

## Part A — gitops kernel (Go)

### Task 1: `fetchChecks` recognizes "no checks reported"

The 40-min-late-CI repo case: `gh pr checks` on a branch with zero reported
checks exits 1 with **empty stdout** and stderr `no checks reported on the
'<branch>' branch`. `fetchChecks` currently returns an error for any empty
stdout (`internal/gitops/checks.go:86-89`), which `runGitopsClaim` treats as
infrastructure → infinite 20s re-claim loop.

**Files:**
- Modify: `internal/gitops/checks.go:86-89`
- Test: `internal/gitops/checks_test.go`

**Interfaces:**
- Produces: `fetchChecks` returns `([]ghCheck{}, nil)` (empty, non-error) when stdout is empty AND stderr contains `no checks reported`. Any other empty-stdout failure still errors.

- [ ] **Step 1: Write the failing test** (match the existing stub style in `checks_test.go` — a `CommandRunner` closure returning a scripted `CommandResult`):

```go
func TestFetchChecksNoChecksReportedIsAbsent(t *testing.T) {
	runner := func(ctx context.Context, argv []string, workspace string) (CommandResult, error) {
		return CommandResult{
			Stdout:   "",
			Stderr:   "no checks reported on the 'kylehanks/ref-1-x' branch",
			ExitCode: 1,
		}, nil
	}
	checks, err := fetchChecks(context.Background(), testSpec(), runner)
	if err != nil {
		t.Fatalf("expected no error for 'no checks reported', got: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected zero checks, got %d", len(checks))
	}
}

func TestFetchChecksEmptyStdoutOtherErrorStillFails(t *testing.T) {
	runner := func(ctx context.Context, argv []string, workspace string) (CommandResult, error) {
		return CommandResult{Stdout: "", Stderr: "gh: connection refused", ExitCode: 1}, nil
	}
	if _, err := fetchChecks(context.Background(), testSpec(), runner); err == nil {
		t.Fatal("expected error for empty stdout without the no-checks marker")
	}
}
```

If `checks_test.go` has no `testSpec()` helper, use the Spec literal its existing tests use.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/gitops/ -run TestFetchChecksNoChecks -v`
Expected: FAIL (`expected no error for 'no checks reported'`)

- [ ] **Step 3: Implement** — in `fetchChecks`, replace the empty-stdout error return:

```go
	stdout := strings.TrimSpace(res.Stdout)
	if stdout == "" {
		// gh prints "no checks reported on the '<branch>' branch" to stderr
		// with exit 1 and NO stdout when a branch has zero checks -- distinct
		// from the documented `[]` case, but the same meaning: no required
		// checks exist (yet). Observed on every PR of a repo whose CI
		// registers late; treating it as an error loops the merging claim
		// forever (Reflex retro, failure category 1).
		if strings.Contains(res.Stderr, "no checks reported") {
			return []ghCheck{}, nil
		}
		return nil, fmt.Errorf("gh pr checks %s: exit %d: %s", spec.Branch, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/gitops/ -run TestFetchChecks -v`
Expected: PASS (both new tests plus existing ones)

- [ ] **Step 5: Commit**

```bash
git add internal/gitops/checks.go internal/gitops/checks_test.go
git commit -m "fix(gitops): treat gh 'no checks reported' as absent checks, not error"
```

### Task 2: `checksAbsent` waits (or proceeds) instead of blocking

`Run` currently maps `checksAbsent` → `OutcomeNotMergeable` (`gitops.go:198-199`).
Wrong both ways: on a repo whose CI registers late the PR must **wait**
(CIPending); on a repo with genuinely no CI the merge should **proceed**.
Add `Spec.RequireChecks` to pick.

**Files:**
- Modify: `internal/gitops/gitops.go` (Spec struct + the `checksAbsent` case)
- Modify: `dispatcher/gitops.go:90-95` (populate the field)
- Modify: `internal/config/config.go` (new `Repo.RequireChecks` with default true — follow the existing default-setting pattern where `RecoverCap` and friends get theirs)
- Test: `internal/gitops/gitops_test.go`

**Interfaces:**
- Produces: `gitops.Spec.RequireChecks bool`. True → absent checks return `OutcomeCIPending` (wait for registration). False → absent checks fall through to protection/merge. Dispatcher sets it from `cfg.Repo.RequireChecks` (yaml `repo.require_checks`, default true).

- [ ] **Step 1: Write the failing tests** (use the package's existing scripted-runner helpers from `fakegh_test.go`/`testhelpers_test.go` to script: PR view OPEN, checks `[]`):

```go
func TestRunAbsentChecksPendsWhenRequired(t *testing.T) {
	spec := testSpec()
	spec.RequireChecks = true
	res, err := Run(context.Background(), spec, fakeGH(t, withPRView(openPR), withChecksJSON("[]")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != OutcomeCIPending {
		t.Fatalf("want ci_pending for absent checks with RequireChecks, got %s", res.Outcome)
	}
}

func TestRunAbsentChecksProceedsWhenNotRequired(t *testing.T) {
	spec := testSpec()
	spec.RequireChecks = false
	// script: view OPEN, checks [], protection ok, merge exit 0
	res, err := Run(context.Background(), spec, fakeGH(t, withPRView(openPR), withChecksJSON("[]"), withProtectionOK(), withMergeExit(0)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("want merged when checks not required, got %s", res.Outcome)
	}
}
```

Adapt the `fakeGH`/`withX` names to whatever `gitops_test.go` actually calls its scripted runner builders — do not invent a parallel harness.

- [ ] **Step 2: Run to verify failure** — `go test ./internal/gitops/ -run TestRunAbsentChecks -v` → FAIL.

- [ ] **Step 3: Implement.** Spec field:

```go
	// RequireChecks controls the checksAbsent verdict: true (the safe
	// default -- CI that registers late must be WAITED for, not blocked on)
	// maps absent checks to OutcomeCIPending; false (a repo with no CI at
	// all) lets the merge proceed on protection alone.
	RequireChecks bool
```

The switch case in `Run`:

```go
	case checksAbsent:
		if spec.RequireChecks {
			// No checks *yet*: on this repo CI may just not have registered
			// (observed ~40 min late during the Reflex build). Same handling
			// as pending -- wait, don't block.
			return Result{Outcome: OutcomeCIPending, PRURL: view.URL, PRNumber: view.Number}, nil
		}
		// No CI on this repo by declaration: fall through to protection/merge.
```

Dispatcher (`runGitopsClaim`): `RequireChecks: d.cfg.Repo.RequireChecks`. Config: add `RequireChecks bool` to the Repo config struct with yaml/mapstructure tag `require_checks`, defaulted to `true` alongside the other defaults.

- [ ] **Step 4: Run** — `go test ./internal/gitops/ ./dispatcher/ ./internal/config/ -v` → PASS (fix any existing test that asserted the old NotMergeable mapping).

- [ ] **Step 5: Commit** — `git commit -m "fix(gitops): absent checks wait for CI registration (require_checks config)"`

### Task 3: failed `gh pr update-branch` on a conflicting PR routes to rework

The 193-attempt loop: GitHub refuses `update-branch` when the PR conflicts;
`resolveStaleBase` returns that as an infrastructure error
(`gitops.go:277-279`) → claim re-claimed forever. When the triggering view
already shows a conflict, a failed update-branch IS the stale-base-conflict
outcome.

**Files:**
- Modify: `internal/gitops/gitops.go` (`Run` call site + `resolveStaleBase` signature)
- Test: `internal/gitops/gitops_test.go`

**Interfaces:**
- Produces: `resolveStaleBase(ctx, spec, view prView, runner)` — takes the already-fetched view. On updateBranch error with `hasConflict(view)`, probes conflict files and returns `OutcomeStaleBaseConflict`.

- [ ] **Step 1: Failing test** — script: view OPEN + `mergeable=CONFLICTING`, update-branch exits 1 (stderr `merge conflict between base and head`), local conflict probe returns files:

```go
func TestRunUpdateBranchRefusedOnConflictIsRework(t *testing.T) {
	res, err := Run(context.Background(), testSpec(), fakeGH(t,
		withPRView(conflictingPR), withChecksGreen(), withProtectionOK(),
		withUpdateBranchExit(1, "GraphQL: merge conflict between base and head")))
	if err != nil {
		t.Fatalf("expected a Result, not an infrastructure error: %v", err)
	}
	if res.Outcome != OutcomeStaleBaseConflict {
		t.Fatalf("want stale_base_conflict, got %s (reason %q)", res.Outcome, res.Reason)
	}
}
```

(The local `probeConflictFiles` runs real git; follow whatever the existing `OutcomeStaleBaseConflict` test does for that — `conflict_test.go` has the fixture pattern.)

- [ ] **Step 2: Run** — `go test ./internal/gitops/ -run TestRunUpdateBranchRefused -v` → FAIL (today it errors).

- [ ] **Step 3: Implement.** Change the call site in `Run`:

```go
	if needsBaseUpdate(view) || hasConflict(view) {
		result, proceed, err := resolveStaleBase(ctx, spec, view, runCommand)
```

And the top of `resolveStaleBase`:

```go
func resolveStaleBase(ctx context.Context, spec Spec, view prView, runner CommandRunner) (result Result, proceed bool, err error) {
	if _, updateErr := updateBranch(ctx, spec, runner); updateErr != nil {
		if !hasConflict(view) {
			return Result{}, false, fmt.Errorf("gitops: updating branch %s onto %s: %w", spec.Branch, spec.BaseBranch, updateErr)
		}
		// GitHub refuses update-branch outright when the PR conflicts with
		// its base -- that refusal IS the conflict verdict, not an
		// infrastructure failure. Returning an error here re-claims the card
		// forever (the Reflex build's 193-attempt loop); routing to rework
		// hands the coder a conflict-resolution turn instead.
		files, probeErr := probeConflictFiles(ctx, spec)
		if probeErr != nil {
			return Result{}, false, fmt.Errorf("gitops: probing conflicting files for branch %s against %s: %w", spec.Branch, spec.BaseBranch, probeErr)
		}
		return Result{
			Outcome:          OutcomeStaleBaseConflict,
			Reason:           fmt.Sprintf("gh pr update-branch refused for %s (conflict with base %s)", spec.Branch, spec.BaseBranch),
			ConflictingFiles: files,
			PRURL:            view.URL,
			PRNumber:         view.Number,
		}, false, nil
	}
```

- [ ] **Step 4: Run** — `go test ./internal/gitops/ -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "fix(gitops): refused update-branch on a conflicting pr routes to rework, not error"`

### Task 4: `mergeable=UNKNOWN` short-circuits to CIPending

Under rapid serial merges GitHub recomputes mergeability and reports
`UNKNOWN`; `Run` today falls through to a doomed `gh pr merge` (rescued only
by `mergeNotReady` string-matching). Skip the doomed call.

**Files:**
- Modify: `internal/gitops/pr.go` (new predicate), `internal/gitops/gitops.go` (early return after protection check)
- Test: `internal/gitops/gitops_test.go`

- [ ] **Step 1: Failing test** — view OPEN with `Mergeable: "UNKNOWN"`, checks green, protection ok; assert `OutcomeCIPending` and (if the fake runner records calls) that no `gh pr merge` was invoked.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** `pr.go`:

```go
// mergeabilityUnknown reports GitHub's transient UNKNOWN state: the
// mergeability computation was invalidated (typically by a concurrent merge
// advancing the base) and hasn't finished recomputing. A merge attempt now
// is guaranteed to be refused; the only correct move is to re-check next poll.
func mergeabilityUnknown(view prView) bool {
	return view.Mergeable == "UNKNOWN"
}
```

`gitops.go`, immediately after the protection check and before the stale-base block:

```go
	if mergeabilityUnknown(view) {
		return Result{Outcome: OutcomeCIPending, PRURL: view.URL, PRNumber: view.Number}, nil
	}
```

- [ ] **Step 4: Run** → PASS. **Step 5: Commit** — `git commit -m "fix(gitops): skip doomed merge while mergeability is unknown"`

### Task 5: gate the gitops pass on the PR before touching the worktree

Zombie anatomy: `runGitopsClaim` calls `d.ws.Ensure(issue)` first
(`dispatcher/gitops.go:84`), resurrecting hand-deleted worktrees/branches,
then fails at `gh pr view` on a gone branch, forever. Check the PR first,
from the primary clone; also give "no PR exists" a terminal outcome.

**Files:**
- Modify: `dispatcher/gitops.go` (`runGitopsClaim`)
- Modify: `internal/gitops/gitops.go`, `internal/gitops/pr.go` (no-PR classification)
- Test: `dispatcher/gitops_test.go` (or wherever the fake `GitOpsRunner` tests live), `internal/gitops/gitops_test.go`

**Interfaces:**
- Produces: `gitops.Run` returns `notMergeable` with reason `no pull request exists for branch <b>` (instead of an error) when `gh pr view` fails with gh's `no pull requests found` message; `Result.Retriable=false` (Task 6). `runGitopsClaim` calls `d.gitOps` with `Spec.Workspace` set to the issue worktree only after a successful `Ensure`, but `Ensure` is now attempted only when the PR pre-check doesn't already resolve the pass.

- [ ] **Step 1: Failing tests.**
  - gitops: scripted runner where `gh pr view` exits 1 with stderr `no pull requests found for branch "x"` → expect `OutcomeNotMergeable` (not error), reason mentions `no pull request exists`.
  - dispatcher: fake GitOpsRunner asserting workspace... (skip — covered by the gitops-level ordering below).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** In `fetchPRView`'s caller (`Run`):

```go
	view, err := fetchPRView(ctx, spec, runCommand)
	if err != nil {
		if strings.Contains(err.Error(), "no pull requests found") {
			// The branch has no PR (deleted after a manual merge, or never
			// pushed). Deterministic: retrying can never fix it. Park it for
			// a human instead of looping (Reflex retro: zombie runs).
			return Result{Outcome: OutcomeNotMergeable, Retriable: false,
				Reason: fmt.Sprintf("no pull request exists for branch %s", spec.Branch)}, nil
		}
		return Result{}, fmt.Errorf("gitops: fetching PR state for branch %s: %w", spec.Branch, err)
	}
```

(`Retriable` lands in Task 6 — implement these two tasks on one branch-local sequence, committing Task 5 after Task 6's field exists, or add the field here and the mapping there; keep both tests.)

In `runGitopsClaim`, run the merged-PR short-circuit before `Ensure` by giving `Run` the primary clone when no worktree exists yet is complex — instead keep ordering but make `Ensure` conditional:

```go
	// Pre-check the PR from the primary clone before ensuring a worktree:
	// an already-merged or missing PR must not resurrect a deleted
	// worktree/branch just to find that out (Reflex retro: zombie gitops
	// runs re-created hand-deleted branches every 20s).
	preSpec := gitops.Spec{
		Branch:           issue.BranchName,
		BaseBranch:       d.cfg.Repo.BaseBranch,
		Workspace:        d.cfg.Repo.Path,
		PrimaryClonePath: d.cfg.Repo.Path,
		RequireChecks:    d.cfg.Repo.RequireChecks,
	}
	result, err := d.gitOps(ctx, preSpec)
	if err == nil && (result.Outcome == gitops.OutcomeMerged || !result.Retriable) {
		return d.applyGitopsResult(ctx, issue, claim.Run.RunID, claim.Run.Lane, result)
	}

	workspace, err := d.ws.Ensure(issue)
	...
```

Note: `gh pr view/checks/merge <branch>` resolve the repo from the working directory, so the primary clone works for every gh call; only the *local* operations (conflict probe, tag, worktree cleanup) need the worktree. Running the full pass from the primary clone is therefore safe for all outcomes except `OutcomeMerged`'s cleanup and `StaleBaseConflict`'s probe — to keep this task small, run the full pass from the worktree as today when the pre-check outcome is Retriable-NotMergeable/CIPending/StaleBaseConflict, and accept one extra `gh pr view` per pass.

- [ ] **Step 4: Run** — `go test ./dispatcher/ ./internal/gitops/ -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "fix(dispatcher): pre-check pr state before ensuring a gitops worktree"`

### Task 6: deterministic gitops failures park instead of auto-retrying

`applyGitopsResult` hardcodes `BlockKindTransient` for every NotMergeable
(`dispatcher/gitops.go:129-131`), so "required checks failing" burned 5
identical retries per ticket. Let gitops declare retriability.

**Files:**
- Modify: `internal/gitops/gitops.go` (Result field; set at each notMergeable site)
- Modify: `dispatcher/gitops.go` (kind mapping)
- Test: `internal/gitops/gitops_test.go`, `dispatcher/gitops_test.go`

**Interfaces:**
- Produces: `gitops.Result.Retriable bool` — true only where a retry could plausibly succeed (merge refused for reasons `mergeNotReady` didn't match, protection unsatisfied). False for: checks failing, no-PR (Task 5), update-branch-conflict is rework not blocked. Dispatcher maps `Retriable=false` → `contract.BlockKindNeedsInput`, `true` → `BlockKindTransient`.

- [ ] **Step 1: Failing tests.** gitops: checks-failing Result has `Retriable == false`. dispatcher: a fake Result `{Outcome: NotMergeable, Retriable: false}` produces a Transition whose BlockKind is `needs_input` (assert via the store fake / applyTerminalWorkerOutcome plumbing the existing dispatcher tests use).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** Result field:

```go
	// Retriable reports whether an OutcomeNotMergeable could plausibly
	// resolve on its own (true: e.g. a refused merge from a transient GitHub
	// state) or is deterministic until a human or coder acts (false: failing
	// required checks, a missing PR). The dispatcher maps false to a
	// non-retrying block kind -- re-running an identical merge gate against
	// identical inputs burned 5 retries per incident in the Reflex build.
	Retriable bool `json:"retriable,omitempty"`
```

`notMergeable(view, reason)` gains a `retriable bool` parameter; call sites: checks-failing → false, protection-unsatisfied → true (protection can be fixed out-of-band and is cheap to re-check), merge-failed-unmatched → true, no-PR (Task 5) → false. Dispatcher:

```go
	case gitops.OutcomeNotMergeable:
		bk := contract.BlockKindNeedsInput
		if result.Retriable {
			bk = contract.BlockKindTransient
		}
```

- [ ] **Step 4: Run** → PASS. **Step 5: Commit** — `git commit -m "fix(dispatcher): deterministic gitops failures park as needs_input, not transient"`

### Task 7: orphan recovery skips terminal-state issues

`recoverOrphanRun` blocks any open run at the attempt cap without looking at
the issue (`dispatcher/recover.go:55-58`) — Done tickets flapped to Blocked
(and were mirrored to Linear) on every restart.

**Files:**
- Modify: `dispatcher/recover.go`
- Test: `dispatcher/recover_test.go`

- [ ] **Step 1: Failing test** — seed the store fake with an issue whose `BoardStatus` is `done` plus an open run at `Attempt >= MaxAttempts`; run `RecoverOrphans`; assert the issue's status is still `done`, no Linear set-state/comment was enqueued, and the run row is closed (status `terminalized`).
- [ ] **Step 2: Run** → FAIL (today it transitions to blocked).
- [ ] **Step 3: Implement** — in `recoverOrphanRun`, after loading the issue:

```go
	if issue.BoardStatus == "done" || issue.BoardStatus == "cancelled" {
		// The issue already finished; this run row is just restart debris.
		// Blocking here would flap a terminal ticket back to blocked and
		// mirror that to Linear (Reflex retro: done tickets un-done by every
		// restart). Close the run and leave the issue alone.
		if err := d.store.CloseRun(ctx, run.RunID, "terminalized"); err != nil {
			return fmt.Errorf("terminalizing leftover run %s on %s issue %s: %w", run.RunID, issue.BoardStatus, issue.ID, err)
		}
		d.logger.Info("orphan run terminalized (issue already terminal)", "issue_id", issue.ID, "run_id", run.RunID, "board_status", issue.BoardStatus)
		return nil
	}
```

If `store.CloseRun(ctx, runID, status string)` doesn't exist, add it to `internal/store` as a one-statement UPDATE mirroring how `Transition` closes runs (`CloseRunID`/`RunStatus`), with its own small test.

- [ ] **Step 4: Run** — `go test ./dispatcher/ ./internal/store/ -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "fix(dispatcher): orphan recovery never touches done/cancelled issues"`

### Task 8: fetch the base ref before creating a worktree

`EnsureWorktree` branches new worktrees off the frozen local base
(`internal/spawn/worktree.go:49`); during the build an external cron
fast-forwarded local main as a workaround.

**Files:**
- Modify: `internal/spawn/worktree.go`
- Test: `internal/spawn/worktree_test.go`

- [ ] **Step 1: Failing test** — build two local repos (fixture pattern from the existing worktree tests): `origin` bare repo, primary clone; add a commit to origin's main *after* cloning; call `EnsureWorktree`; assert the new worktree's HEAD contains the post-clone commit.
- [ ] **Step 2: Run** → FAIL (worktree is at the stale clone-time main).
- [ ] **Step 3: Implement** — before the `worktree add` in the not-exists-locally branch:

```go
	if !branchExistsLocally {
		// Branch from the REMOTE base tip, not the local ref: the primary
		// clone's local base only advances when something fetches it, and
		// nothing in the kernel did -- the Reflex build ran an external
		// fast-forward cron as a workaround. A fetch failure (offline dev)
		// falls back to the local ref rather than failing the spawn.
		base := baseBranch
		if err := runGitCmd(ctx, primaryClonePath, "fetch", "origin", baseBranch); err == nil {
			base = "origin/" + baseBranch
		}
		args = []string{"worktree", "add", "-b", branch, path, base}
	} else {
		args = []string{"worktree", "add", path, branch}
	}
```

- [ ] **Step 4: Run** — `go test ./internal/spawn/ -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "fix(spawn): branch new worktrees from the fetched remote base"`

### Task 9: explicit squash subject

`mergePR` passes no `--subject`, so GitHub uses the PR title — which is coder
narration (Task 11 fixes the source; this fixes the sink).

**Files:**
- Modify: `internal/gitops/gitops.go` (Spec fields), `internal/gitops/pr.go` (`mergePR`)
- Modify: `dispatcher/gitops.go` (populate from issue)
- Test: `internal/gitops/pr_test.go`

**Interfaces:**
- Produces: `Spec.IssueID`, `Spec.IssueTitle` (both optional; empty → today's behavior). `mergePR` adds `--subject "<lower(issueID)>: <issueTitle> (#<pr>)"` when both are set. Dispatcher sets them from `issue.ID`/`issue.Title`.

- [ ] **Step 1: Failing test** — recording runner; call `mergePR` with a Spec carrying `IssueID: "REF-41"`, `IssueTitle: "Fix cerebras image encoding"`, prNumber 31; assert argv contains `--subject` and `ref-41: Fix cerebras image encoding (#31)`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** — `mergePR` gains a `prNumber int` parameter (Run passes `view.Number`):

```go
func mergePR(ctx context.Context, spec Spec, prNumber int, runner CommandRunner) (CommandResult, error) {
	argv := []string{"gh", "pr", "merge", spec.Branch, mergeFlag(spec.MergeMethod)}
	if spec.IssueID != "" && spec.IssueTitle != "" {
		subject := fmt.Sprintf("%s: %s (#%d)", strings.ToLower(spec.IssueID), spec.IssueTitle, prNumber)
		argv = append(argv, "--subject", subject)
	}
	...
```

- [ ] **Step 4: Run** → PASS. **Step 5: Commit** — `git commit -m "feat(gitops): derive squash subject from the issue, not the pr title"`

---

## Part B — coder lane (Python)

### Task 10: `drive_turn` exposes the last message's text

`final_text` concatenates every AI text block across the whole turn
(`dac.py:303`) — titles, summaries, and review feedback all read narration
soup. Track the final message separately.

**Files:**
- Modify: `agent/src/clipse_agent/dac.py` (`_accumulate_message_chunk`, `DacTurnResult`, `drive_turn`)
- Test: `agent/tests/test_dac.py`

**Interfaces:**
- Produces: `DacTurnResult.last_text: str` — the concatenated text blocks of only the final `AIMessage` (by message id) seen in the stream. `final_text` keeps its current all-text meaning (checkpoint/PR-body audit trail).

- [ ] **Step 1: Failing test** (follow `test_dac.py`'s existing fake-stream pattern for `drive_turn`):

```python
async def test_drive_turn_last_text_is_final_message_only(fake_graph_factory):
    graph = fake_graph_factory(
        messages=[
            ai_message(id="m1", text="Reading the ticket docs now."),
            tool_message("..."),
            ai_message(id="m2", text="STATUS: done\nTITLE: feat: add widget"),
        ]
    )
    result = await drive_turn(graph, config(), task_text="t", max_tokens=None)
    assert result.last_text == "STATUS: done\nTITLE: feat: add widget"
    assert "Reading the ticket docs now." in result.final_text
```

Adapt `ai_message`/`tool_message`/`fake_graph_factory` to the fixtures `test_dac.py` already defines for streaming tests.

- [ ] **Step 2: Run** — `cd agent && uv run pytest tests/test_dac.py -k last_text -q` → FAIL.
- [ ] **Step 3: Implement.** `_accumulate_message_chunk` gains a per-message accumulator:

```python
def _accumulate_message_chunk(
    data: tuple[Any, dict[str, Any]],
    text_parts: list[str],
    last_message: dict[str, Any],
) -> tuple[int, int]:
    ...
    message_id = getattr(message_obj, "id", None)
    if message_id != last_message.get("id"):
        last_message["id"] = message_id
        last_message["parts"] = []
    for block in getattr(message_obj, "content_blocks", None) or ():
        if isinstance(block, dict) and block.get("type") == "text":
            text = block.get("text", "")
            if text:
                text_parts.append(text)
                last_message["parts"].append(text)
    ...
```

`drive_turn` threads `last_message: dict[str, Any] = {}` through and returns `last_text="".join(last_message.get("parts", []))` on `DacTurnResult` (add the field with a `""` default so reviewer-graph call sites keep working before Task 11 touches them).

- [ ] **Step 4: Run** — `uv run pytest tests/test_dac.py -q` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(dac): expose the final message's text separately from the full transcript"`

### Task 11: structured output tail (STATUS / TITLE / HANDOFF)

Give the coder an explicit contract instead of scraping narration line 1
(`coder.py:634-639`, `760-764`).

**Files:**
- Create: `agent/src/clipse_agent/tail.py`
- Modify: `agent/src/clipse_agent/profiles/coder.py` (`_SYSTEM_PROMPT`)
- Modify: `agent/src/clipse_agent/graphs/coder.py` (`_commit_message`, `_pr_title`, state carries `dac_last_text`)
- Test: `agent/tests/test_tail.py`, `agent/tests/test_coder_graph.py`

**Interfaces:**
- Produces:

```python
@dataclass(frozen=True)
class StructuredTail:
    status: str          # "done" | "blocked" | "" (absent)
    blocked_reason: str  # text after "blocked:" when status == "blocked"
    title: str           # "" when absent
    handoff: str         # "" when absent

def parse_structured_tail(text: str) -> StructuredTail: ...
```

Parsing: scan the last 40 lines; `STATUS:` / `TITLE:` are single lines
(last occurrence wins, case-insensitive keys); `HANDOFF:` captures every
line after it until another `KEY:` line or end of text. Missing keys → empty
strings; the parser never raises.

- [ ] **Step 1: Failing tests** in `test_tail.py`: done status; blocked with reason; missing tail entirely (all fields empty); HANDOFF multi-line capture; TITLE longer than 72 chars is preserved verbatim (truncation is the consumer's job).
- [ ] **Step 2: Run** — `uv run pytest tests/test_tail.py -q` → FAIL (module missing).
- [ ] **Step 3: Implement `tail.py`:**

```python
import re
from dataclasses import dataclass

_KEY_RE = re.compile(r"^(STATUS|TITLE|HANDOFF):\s*(.*)$", re.IGNORECASE)


@dataclass(frozen=True)
class StructuredTail:
    status: str = ""
    blocked_reason: str = ""
    title: str = ""
    handoff: str = ""


def parse_structured_tail(text: str) -> StructuredTail:
    """Parse the STATUS/TITLE/HANDOFF tail from a lane's final message.

    Tolerant by design: absent keys yield empty strings, the last
    occurrence of a repeated key wins, and free text before the tail is
    ignored -- a model that skips the protocol degrades to legacy
    behavior, never to an exception.
    """
    status = ""
    blocked_reason = ""
    title = ""
    handoff_lines: list[str] | None = None
    for line in text.splitlines()[-40:]:
        match = _KEY_RE.match(line.strip())
        if match:
            key, value = match.group(1).upper(), match.group(2).strip()
            if key == "STATUS":
                handoff_lines = None
                lowered = value.lower()
                if lowered.startswith("blocked"):
                    status = "blocked"
                    _, _, reason = value.partition(":")
                    blocked_reason = reason.strip()
                elif lowered.startswith("done"):
                    status = "done"
            elif key == "TITLE":
                handoff_lines = None
                title = value
            elif key == "HANDOFF":
                handoff_lines = [value] if value else []
        elif handoff_lines is not None:
            handoff_lines.append(line)
    handoff = "\n".join(handoff_lines).strip() if handoff_lines else ""
    return StructuredTail(status=status, blocked_reason=blocked_reason, title=title, handoff=handoff)
```

- [ ] **Step 4: Prompt.** Append to `profiles/coder.py` `_SYSTEM_PROMPT`:

```
- End your FINAL message with this exact tail (own lines, in this order):
  STATUS: done            (or: STATUS: blocked: <what you need and why>)
  TITLE: <lowercase conventional-commit line for this change, <=60 chars>
  HANDOFF:
  <3-8 bullet lines for the next agent: decisions made, interfaces
  added (exact names/signatures), deviations from the issue text,
  gotchas for dependent work, what was intentionally NOT done>
```

- [ ] **Step 5: Consumers.** `make_run_dac` stores `dac_last_text` (from `DacTurnResult.last_text`) in state alongside `dac_summary`. `_commit_message` / `_pr_title`:

```python
def _commit_message(state: CoderState) -> str:
    issue_id = state.get("issue_id", "")
    tail = parse_structured_tail(state.get("dac_last_text") or "")
    if tail.title:
        return f"{issue_id}: {tail.title}"[:72]
    turn = state.get("turn_count", 0) + 1
    summary_lines = (state.get("dac_summary") or "").strip().splitlines()
    headline = summary_lines[0] if summary_lines else f"turn {turn}"
    return f"{issue_id}: {headline}"[:72]
```

(`_pr_title` identical shape, 120-char cap.) Add a `test_coder_graph.py` test: state with a tail-carrying `dac_last_text` produces `REF-9: feat: add widget` style titles; state without a tail falls back to the old headline.

- [ ] **Step 6: Run** — `uv run pytest tests/test_tail.py tests/test_coder_graph.py -q` → PASS.
- [ ] **Step 7: Commit** — `git commit -m "feat(coder): structured status/title/handoff tail replaces narration scraping"`

### Task 12: blocked routing + empty-branch guard

A coder that says "blocked" currently proceeds commit→push→`gh pr create`,
which crashes on the empty branch and gets retried 5× (REF-26). Route it.

**Files:**
- Modify: `agent/src/clipse_agent/graphs/coder.py` (`route_after_dac`, `make_open_pr`, `emit_result`)
- Modify: `agent/src/clipse_agent/contract.py`, `schema/worker-result.schema.json`, `internal/contract/contract.go` — only if `BlockKind` lacks a fitting member; use the existing `needs_input` kind.
- Test: `agent/tests/test_coder_graph.py`

- [ ] **Step 1: Failing tests.**
  - Graph test: DAC turn completes with `dac_last_text` ending `STATUS: blocked: need REFLEX_CEREBRAS_API_KEY`; assert the emitted `WorkerResult` has `outcome == "blocked"`, `block_kind == "needs_input"`, summary containing the reason — and that no `git commit` / `gh pr create` commands were run (the tests' recording `CommandRunner` asserts on invocations).
  - open_PR guard test: state reaches `open_PR` with zero commits ahead of base (`git rev-list --count origin/main..HEAD` → `0`) and no existing PR; assert no `gh pr create` invocation and the node returns `{"pr_url": ""}`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** Extend `route_after_dac` (it already routes interrupts): when the turn completed and `parse_structured_tail(state["dac_last_text"]).status == "blocked"`, route to `"emit_result"`, and stash `{"blocked_reason": tail.blocked_reason}` in state via the `run_DAC` node's return. `emit_result` maps `blocked_reason` → `outcome="blocked"`, `block_kind=BlockKind.needs_input`, `summary=f"coder blocked: {reason}"` (follow the existing blocked-emission shape used for token-ceiling blocks). In `make_open_pr._node`, before `gh pr create`:

```python
        base_branch = state.get("base_branch") or "main"
        ahead = _run(
            run_command,
            ["git", "rev-list", "--count", f"origin/{base_branch}..HEAD"],
            cwd,
            check=False,
        )
        if ahead.returncode == 0 and ahead.stdout.strip() == "0":
            # Nothing to open a PR from: the turn made no commits (a blocked
            # or no-op turn). gh pr create would fail "No commits between..."
            # and the kernel would classify that crash as transient and
            # retry it five times (REF-26). An empty pr_url is the honest
            # result.
            return {"pr_url": ""}
```

- [ ] **Step 4: Run** — `uv run pytest tests/test_coder_graph.py -q` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "fix(coder): blocked status routes to emit_result; never open a pr from an empty branch"`

### Task 13: workers read the target repo's guides, not clipse's

During the build every coder round carried `/Users/xlyk/Code/clipse/AGENTS.md`
(the dispatcher's own repo guide) as injected memory while the worktree's
AGENTS/CLAUDE guides were not injected. Root cause to confirm: DAC's memory
loader resolves from the worker process's cwd, and the dispatcher spawns
workers with its own cwd.

**Files:**
- Investigate: `internal/spawn/` (worker process spawn — find where `exec.Cmd` is built; note whether `Dir` is set), `deepagents_code.agent.create_cli_agent` (memory resolution — read its signature in the installed package under `agent/.venv/`)
- Modify: the spawn site (set `cmd.Dir` to the issue worktree) — expected one-line fix plus test
- Test: `dispatcher/` or `internal/spawn/` spawn test asserting the built command's `Dir` equals the worktree path

- [ ] **Step 1: Confirm the mechanism** — locate the `exec.Cmd` construction for worker spawns (`grep -rn "exec.Command\|exec.CommandContext" internal/spawn/ dispatcher/`); check whether `Dir` is set and to what. Then check `create_cli_agent`'s memory/AGENTS resolution in the vendored DAC package to confirm it is cwd-derived.
- [ ] **Step 2: Failing test** — spawn-spec test asserting `Dir` (or the WorkerSpec equivalent) is the issue's worktree path, not the dispatcher's cwd.
- [ ] **Step 3: Fix** — set the worker process working directory to the worktree at the spawn site. If investigation shows memory is instead resolved from an explicit path DAC computes, fix at `build_coder_agent`'s `cwd=` plumbing in `dac.py` instead and say so in the commit body.
- [ ] **Step 4: Run** — `go test ./internal/spawn/ ./dispatcher/ -v` and one manual smoke (`make build` + testworker) → PASS.
- [ ] **Step 5: Commit** — `git commit -m "fix(spawn): workers run with the issue worktree as cwd so dac injects the target repo's guides"`

---

## Part C — reviewer lane (Python)

### Task 14: severity protocol — only `blocking:` findings request changes

Three of the Reflex build's six rework cycles were pbxproj whitespace nits.

**Files:**
- Modify: `agent/src/clipse_agent/profiles/reviewer.py` (`_SYSTEM_PROMPT`)
- Modify: `agent/src/clipse_agent/graphs/reviewer.py` (`_INLINE_COMMENT_RE`, `InlineComment`, `classify`, `route_after_classify`)
- Test: `agent/tests/test_reviewer_graph.py`

**Interfaces:**
- Produces: `InlineComment.severity: str` (`"blocking"` | `"nit"`; unprefixed parses as `"blocking"` — conservative). `classify` returns `review_passed=True` when the verdict is PASS **or** every parsed finding is a nit; comments are posted whenever any exist (route change), so nits still land on the PR without a rework cycle.

- [ ] **Step 1: Failing tests:**
  - `- nit: app/x.pbxproj:12: tab width` parses with severity `nit`.
  - Verdict CHANGES_REQUESTED + only-nit findings → `review_passed is True`, comments non-empty.
  - Verdict CHANGES_REQUESTED + one `blocking:` finding among nits → `review_passed is False`.
  - Unprefixed finding still blocks (back-compat).
  - Route: passed-with-comments goes to `post_comments` (not straight to `emit_result`).
- [ ] **Step 2: Run** — `uv run pytest tests/test_reviewer_graph.py -k severity -q` → FAIL.
- [ ] **Step 3: Implement.** Regex:

```python
_INLINE_COMMENT_RE = re.compile(r"^-\s*(?:(blocking|nit):\s*)?([^\s:]+):(\d+):\s*(.+)$", re.IGNORECASE)
```

`classify`:

```python
    comments: list[InlineComment] = []
    if verdict_match is not None:
        comments = _parse_inline_comments(dac_summary, verdict_match.end())
    blocking = [c for c in comments if c.severity == "blocking"]
    passed = verdict_match is not None and (
        verdict_match.group(1).upper() == _VERDICT_PASS or not blocking
    )
```

(Keep the missing-verdict → not-passed fail-safe exactly as is: `verdict_match is None` still means `passed=False`, zero comments.) `route_after_classify` → `"post_comments"` whenever `state.get("review_comments")` is non-empty, else by pass/fail as today; `post_comments` must not flip the outcome — it already only posts.

Prompt additions to `_SYSTEM_PROMPT`:

```
- Prefix every finding with a severity: `- blocking: path:LINE: ...` for a
  defect that must be fixed before merge, `- nit: path:LINE: ...` for
  polish. Only blocking findings justify VERDICT: CHANGES_REQUESTED.
- Never comment on formatting or whitespace in generated files
  (project.pbxproj, *.gen.go, *_generated.*, package lockfiles).
- Before emitting a verdict, enumerate EVERY instance of each defect class
  you report (grep for the pattern); a second review round must never be
  needed for the same class.
```

- [ ] **Step 4: Run** — `uv run pytest tests/test_reviewer_graph.py -q` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(reviewer): blocking/nit severity protocol; nits no longer force rework"`

### Task 15: truncated diffs name the files that were cut

The 60k-char diff cap silently dropped three files from one Reflex review.

**Files:**
- Modify: `agent/src/clipse_agent/graphs/reviewer.py` (`make_load_diff`)
- Test: `agent/tests/test_reviewer_graph.py`

- [ ] **Step 1: Failing test** — a scripted diff exceeding the cap; assert the task text ends with a `DIFF TRUNCATED` section listing the file names whose diff content falls wholly or partly beyond the cap, and instructing per-file reads.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** — in `make_load_diff`, when truncating: also run `git diff --name-only <base>...HEAD`, determine which files' `diff --git` headers appear at or beyond the cut offset (or not at all in the kept prefix), and append:

```python
        omitted = [f for f in all_files if f"diff --git a/{f}" not in kept]
        if omitted:
            note = (
                "\n\nDIFF TRUNCATED: the diffs for these files were cut from "
                "the text above. You MUST read each one (e.g. `git diff "
                f"{base_ref}...HEAD -- <file>` or `cat <file>`) before "
                "emitting a verdict:\n" + "\n".join(f"- {f}" for f in omitted)
            )
```

- [ ] **Step 4: Run** → PASS. **Step 5: Commit** — `git commit -m "fix(reviewer): truncated diffs enumerate omitted files and require reading them"`

### Task 16: land the reviewer binary/image-read fix (clipse#22)

`fix/reviewer-skip-binary-images` (commit c75e083) is still an open draft —
the REF-6 abort (11× Anthropic `400 Could not process image`) reproduces on
any repo with image fixtures until it merges.

- [ ] **Step 1:** `git fetch origin && git log origin/main..fix/reviewer-skip-binary-images --oneline` — confirm the branch still applies; rebase onto main if behind.
- [ ] **Step 2:** `cd agent && uv run pytest -q` on the branch → PASS.
- [ ] **Step 3:** Merge PR #22 (squash, subject `fix(reviewer): never read binary/image files into the review context`).
- [ ] **Step 4:** Follow-up hardening noted in the PR body — DAC `media_utils` rejecting Anthropic-unsupported image formats — file as its own issue; do not scope-creep it here.

---

## Part D — handoff comments (write side + read side together)

### Task 17: `handoff` field through the worker contract

**Files:**
- Modify: `schema/worker-result.schema.json` (add optional `handoff` string property)
- Modify: `internal/contract/contract.go` (`WorkerResult` — follow the `PrUrl` optional-pointer pattern)
- Modify: `agent/src/clipse_agent/contract.py` (`WorkerResult` pydantic model — optional `handoff: str | None = None`)
- Test: `internal/contract/contract_test.go`, `agent/tests/test_worker.py`

- [ ] **Step 1: Failing tests** — Go: unmarshal a worker-result JSON carrying `"handoff": "did x"` and assert the field; marshal a result without it and assert the key is absent. Python: `WorkerResult(...).model_dump_json(exclude_none=True)` omits `handoff` when None, includes it when set.
- [ ] **Step 2: Run both** → FAIL.
- [ ] **Step 3: Implement** all three sources together:

```go
	// Structured handoff note for the issue's Linear thread: decisions,
	// interfaces, deviations, gotchas for dependents. Optional; present only
	// when the lane produced one.
	Handoff *string `json:"handoff,omitempty,omitzero" yaml:"handoff,omitempty" mapstructure:"handoff,omitempty"`
```

- [ ] **Step 4: Run** — `go test ./internal/contract/ -v` and `uv run pytest tests/test_worker.py -q` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(contract): optional handoff note on worker results"`

### Task 18: coder emits the HANDOFF tail as `WorkerResult.handoff`

**Files:**
- Modify: `agent/src/clipse_agent/graphs/coder.py` (`emit_result`)
- Test: `agent/tests/test_coder_graph.py`

- [ ] **Step 1: Failing test** — graph run whose `dac_last_text` carries a `HANDOFF:` section; assert the emitted `WorkerResult.handoff` equals the section body (and `is None` when absent).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** — in `emit_result`, `tail = parse_structured_tail(state.get("dac_last_text") or "")`; set `handoff=tail.handoff or None` on the result. Cap at 4000 chars (`tail.handoff[:4000]`) so a runaway section can't bloat the Linear comment.
- [ ] **Step 4: Run** → PASS. **Step 5: Commit** — `git commit -m "feat(coder): emit the handoff tail on worker results"`

### Task 19: dispatcher posts the handoff as a Linear comment

**Files:**
- Modify: `dispatcher/comment.go` (new `handoffComment`)
- Modify: `dispatcher/reconcile.go` (`applyTerminalWorkerOutcome` — where each outcome branch builds its `store.TransitionReq`)
- Test: `dispatcher/comment_test.go`, plus the existing terminal-outcome dispatcher test file

**Interfaces:**
- Produces: `handoffComment(lane, outcome, handoff string) string` →

```
### coder handoff — done

<handoff body>
```

- [ ] **Step 1: Failing tests** — `handoffComment("coder", "done", "- chose drop semantics")` renders header + body. Dispatcher: a done-outcome WorkerResult with `Handoff` set produces a Transition whose `Comment` contains the handoff comment; a nil `Handoff` leaves `Comment` as today (empty for done, `blockedComment` for blocked).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement:**

```go
// handoffComment renders a lane's structured handoff note as the Linear
// comment body posted on terminal outcomes. The header names the lane and
// outcome because every API-posted comment renders as the same Linear user.
func handoffComment(lane, outcome, handoff string) string {
	return fmt.Sprintf("### %s handoff — %s\n\n%s", lane, outcome, handoff)
}
```

In `applyTerminalWorkerOutcome`, where the TransitionReq is assembled for the done / needs_review / changes_requested / blocked outcomes: if `wr.Handoff != nil && *wr.Handoff != ""`, set (or append after a blank line to) `req.Comment` with `handoffComment(string(wr.Lane), string(wr.Outcome), *wr.Handoff)`. Never post on `continue` outcomes.

- [ ] **Step 4: Run** — `go test ./dispatcher/ -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(dispatcher): post lane handoff notes as linear comments on terminal outcomes"`

### Task 20: claim-time injection — issue + blocker comments reach the coder

The read side, without which Task 19 is write-only noise. The ticket template
already tells coders to "read the comments on dependency tickets"; make that
literally possible.

**Files:**
- Modify: `internal/linear/http_client.go` (+`linear.go` interface, `mock_client.go`): `IssueComments(ctx, issueID) ([]Comment, error)` with `type Comment struct { Body string; CreatedAt string }`
- Modify: `internal/store` — confirm the blocker-relation query used by promotion (find it via `grep -rn "blocker\|depends\|relation" internal/store/*.go`); expose `BlockerIssueIDs(ctx, issueID) ([]string, error)` if promotion's query isn't already callable in that shape
- Modify: `dispatcher/dispatcher.go` (new env var + builder), the spawn site that already injects `CLIPSE_REVIEW_FEEDBACK` (see `dispatcher/schedule.go`)
- Modify: `agent/src/clipse_agent/graphs/coder.py` (`load_context`)
- Test: `internal/linear/http_client_test.go`, `dispatcher/dispatcher_test.go`, `agent/tests/test_coder_graph.py`

**Interfaces:**
- Produces: env var `CLIPSE_DEPENDENCY_NOTES` — a markdown document:

```
## REF-5 (blocker) comments

### coder handoff — done
- schema uses integer epoch-ms timestamps
...

## REF-9 (this issue) comments

...
```

Built at coder-spawn time from the Linear comments of the issue itself plus each blocking issue. Total cap 16,000 chars — drop oldest comments first, then whole blockers, keeping the issue's own comments last-to-go. Fetch failure degrades to an unset var (log a warning; never fail the spawn).

- [ ] **Step 1: Failing tests:**
  - linear: `BuildIssueCommentsRequest` marshals the GraphQL query (`query($id: String!){ issue(id: $id){ comments(first: 50){ nodes{ body createdAt } } } }`) with the id variable; `IssueComments` decodes a canned response into `[]Comment` (mirror the `CandidateIssues` test's fixture style).
  - dispatcher: `dependencyNotes(issue)` with a mock client returning comments for the issue and one blocker renders both sections in order; a client error yields `""`.
  - coder: `load_context` appends the env var's content under a `## Dependency notes` heading in the task text when set (mirror the `CLIPSE_REVIEW_FEEDBACK` test).
- [ ] **Step 2: Run all three** → FAIL.
- [ ] **Step 3: Implement** — linear client method mirroring `Comment`'s existing request/do plumbing; `MockClient` records/returns scripted comments. Dispatcher:

```go
const clipseDependencyNotesEnvVar = "CLIPSE_DEPENDENCY_NOTES"

// dependencyNotes renders the Linear comment history a coder needs at claim
// time: its blockers' comments (the decisions the ticket template tells it
// to read) plus its own (rework/continuation context). Best-effort: any
// fetch error degrades to "" -- a spawn must never fail because Linear was
// slow. Capped at 16k chars, oldest-first eviction, issue-own comments
// evicted last.
func (d *Dispatcher) dependencyNotes(ctx context.Context, issue store.Issue) string
```

Wire it next to the `CLIPSE_REVIEW_FEEDBACK` injection for coder-lane spawns only (reviewers get the diff; they don't need the thread). `load_context` in `coder.py`:

```python
    dependency_notes = os.environ.get("CLIPSE_DEPENDENCY_NOTES", "").strip()
    if dependency_notes:
        task_text += "\n\n## Dependency notes (Linear comments)\n\n" + dependency_notes
```

- [ ] **Step 4: Run** — `go test ./internal/linear/ ./dispatcher/ -v` and `uv run pytest tests/test_coder_graph.py -q` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(dispatcher): inject issue + blocker linear comments into coder spawns"`

---

## Out of scope (deliberately)

Design changes needing their own plans: `waiting_ci` claim state (stop
modeling CI waits as attempt-inflating re-claims), kernel-run verification
loop (dispatcher executes the repo's CI command and feeds results to
review/rework), operator override CLI verbs, canary board, separate reviewer
GitHub identity, drain/hot-reload, cost rollups. `parkOrRetry`
identical-failure detection is intentionally dropped: Tasks 6 and 12 remove
both incident classes at their sources.

## Execution order

Tasks 1–9 (Go kernel) are independent of 10–20 and of each other except 5↔6
(share `Result.Retriable`; do 6 first or on one branch). 10 → 11 → 12 → 18
are a chain; 17 before 18/19; 20 last. Suggested batches: A (1–4, 6–9), B
(5), C (10–13), D (14–16), E (17–20).
