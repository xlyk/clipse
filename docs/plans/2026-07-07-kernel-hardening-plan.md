# Kernel Hardening Implementation Plan (Workstream A)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the five reconciliation bugs the 2026-07-06 whole-project review found in the Go kernel (`dispatcher/`, `internal/store`, `internal/linear`), without touching the Python worker or the JSON-schema contract.

**Architecture:** No new packages, no new architecture — this is bugfix work inside the existing kernel. Every fix is proven the way this repo already proves kernel behavior: table-driven `testing` against a real SQLite-backed `*store.Store`, a fake `Spawner`/`Workspacer`, and `linear.MockClient` — zero LLM, zero real network, driven entirely through `Dispatcher.Tick`/`RecoverOrphans`. Two fixes reach for a technique not yet used elsewhere in this form: `dispatcher/fakes_test.go`'s `fakeSpawner` gains a one-shot `FailOnCall` (Task 1, to fail exactly one respawn instead of every spawn), and two tests reach through `store.DB()` (already an established escape hatch — see `dispatcher/recover_test.go`) to simulate a store-level failure with real SQL rather than a mock, since `Dispatcher.store` is a concrete `*store.Store`, not an interface.

**Tech Stack:** Go 1.x stdlib `testing` (table-driven, no testify), `modernc.org/sqlite`, `log/slog`. No new dependencies.

Base commit: `f801d46` (main). Branch: `fix/kernel-hardening`.

## Global Constraints

Kernel invariants from `AGENTS.md` that bind this work (verbatim):

- **`board_status='running'` is entered ONLY via the CAS claim** (`store.ClaimReady`, a `BEGIN IMMEDIATE` compare-and-swap on `board_status='ready' AND claim_lock IS NULL`). Never write `running` directly.
- **SQLite is runtime truth; Linear is task intent.** The dispatcher is the only *automated* writer of board state. A re-poll never clobbers a dispatcher-owned `board_status` (`UpsertIssue` preserves it on conflict); humans may move cards, and the poll adopts the move only when the issue holds no active claim (else SQLite wins and the outbox re-asserts).
- **Linear is written ONLY through the outbox.** Transitions enqueue a `linear_writes` row in the *same* transaction as the state change (`store.Transition`); `dispatcher.drainOutbox` mirrors pending rows each tick and retries on failure.
- **Transient failures auto-recover (bounded); everything else parks in `blocked`.** `dispatcher.parkOrRetry` is the single retry-vs-park decision point; the recovery re-queue goes through `store.Transition` (outbox-mirrored) like any other transition. `recover_attempts < recover_cap` bounds it; a card inside its `blocked_until` backoff window is invisible to every claim/peek.
- **`Tick` is single-goroutine and race-free.** Each spawn starts one goroutine that blocks on `RunHandle.Wait()` and sends the result on a buffered channel; `Tick` drains it non-blocking. The in-flight map is touched only by the `Tick` goroutine. The whole package passes `go test -race`.
- **Bare lane everywhere in the store.** `issues.lane_label` and `runs.lane` hold the bare lane (`coder`/`reviewer`/`git_operator`); the `agent:` prefix lives only in Linear-label parsing.

Plan-specific constraints:

- **No changes to `agent/` (the Python worker).** Every fix here is Go-side (`dispatcher/`, `internal/store`, `internal/linear`) or Makefile.
- **No `schema/*.schema.json` / codegen changes unless a finding truly requires it.** Finding 3 (cancelled dependencies) was the one candidate; see Task 3's design note for why it does **not** need one — `issues.board_status` is unconstrained `TEXT` (`internal/store/migrations.go:18`), and `dispatcher/promote.go` + `dispatcher/recover.go` already special-case the raw string `"cancelled"` (dead code today, made live by Task 3).
- **`go test -race ./...` must stay green after every task.** Task 0 makes `make test-go` match this exactly, so every later task's "run tests" step already carries the race detector.
- **TDD**: every task's first step is a test that fails against the current code, for the reason the finding describes — not a contrived reason. Two exceptions are called out explicitly where they arise (Task 3's end-to-end regression test and Task 7's ReadSnapshot refactor are covered by *existing* green tests, since they don't change observable behavior — noted inline rather than forcing an artificial red phase).
- **Table-driven stdlib `testing` only** (no testify), matching `dispatcher/*_test.go` conventions. Every error wrapped (`fmt.Errorf("...: %w", err)`). Runtime/daemon code uses `log/slog` only, no `fmt.Println` (unaffected here — no CLI command output is touched).
- **Commits**: Conventional Commits, casual/lowercase, no trailing period, no AI signature. One concern per commit — Task 7 is the one place this plan bundles two independently-small fixes in one task container, and it makes two commits rather than force a false single-concern message (see Task 7's intro).
- **Never `git add -A` / `git add .`.**

## Findings index (severity order = task order)

| Task | Finding | Severity | Root cause | Fix |
|---|---|---|---|---|
| 0 | (infra) `make test-go` lacks `-race` | — | Local gate weaker than CI | Match CI exactly |
| 1 | Inflight-map leak | Critical | `applyContinue`'s respawn failure clears the store claim but never deletes `d.inflight[runID]`; next `Heartbeat` errors "no active claim" and aborts `reconcile` (and everything after it in `Tick`) forever | `delete(d.inflight, runID)` in `spawnAttempt`'s two failure branches |
| 2 | `adoptLinearMove` adopts `running` | Major | An unclaimed issue observed as Linear `"running"` (human drag, or a restart-requeue race) gets `board_status='running'` written with no claim — unclaimable, unreleasable | Route `linearStatus=="running"` through the existing re-assert path instead of adopt, whenever there's no active claim backing it |
| 3 | Cancelled dependencies never terminal | Major | The candidate query excludes Linear `canceled` issues entirely, so a cancelled blocker's store row is frozen pre-cancellation forever; `promote.go`/`recover.go`'s `"cancelled"` checks are dead code with no producer | Stop excluding `canceled` from the poll; map it to the raw string `"cancelled"` via the state's `type` (not its renamable `name`) |
| 4 | Destructive result consumption | Major | A `GetIssue` failure inside `applyResult` (after draining the channel) drops the worker result on the floor; the run occupies its lane-cap slot forever with no result ever applied | Send the result back onto `d.results` before returning the error, so the next tick retries it |
| 5 | Merging-column re-claims inflate `run.Attempt` | Minor–Moderate | `claimAndRunGitops`'s CI-pending recheck cadence (every `mergingTTL`) inserts a fresh run row each cycle via `nextAttempt`'s issue-global max+1; a restart mid-CI-wait can read that inflated count as N real failures | Exempt the `git_operator` lane from `RecoverOrphans`'s `MaxAttempts` check — it has its own, separate, `RecoverCap`-bounded retry path for genuine failures |
| 6 | Human requeue from Blocked resets `rework_count` but not `recover_attempts` | Minor | `adoptLinearMove` only sets `ResetReworkCount`, never `ResetRecoverAttempts` | Reset both together, same condition |
| 7a | `LaneLabelPrefix` config knob is dead | Minor | `internal/linear/status.go` hardcodes `"agent:"`; `cfg.LaneLabelPrefix` is parsed/defaulted/validated but never read | Thread it through `HTTPClient` → `NormalizeCandidateIssues` → `laneFromLabels` |
| 7b | `ReadSnapshot` N+1 | Minor (perf) | A per-issue `latestRun` query runs despite `runsByIssue` already loading every run, ordered such that the last element per issue *is* the latest | Derive `LatestRun` from `runsByIssue`'s result; delete `latestRun` |

## File touch summary

No new production packages or files. Touched:

```
Makefile                                    # Task 0
dispatcher/spawn.go                         # Task 1
dispatcher/fakes_test.go                    # Task 1 (fakeSpawner.FailOnCall)
dispatcher/reconcile_test.go                # Tasks 1, 4
dispatcher/poll.go                          # Task 2
dispatcher/poll_test.go                     # Tasks 2, 6
AGENTS.md                                   # Task 2 (invariant clarification)
internal/linear/http_client.go              # Tasks 3, 7a
internal/linear/normalize.go                # Tasks 3, 7a
internal/linear/status.go                   # Tasks 3, 7a
internal/linear/normalize_test.go           # Tasks 3, 7a
internal/linear/http_client_test.go         # Task 3
internal/linear/http_client_loopback_test.go # Task 7a
dispatcher/promote.go                       # Task 3 (comment only)
dispatcher/recover.go                       # Tasks 3 (comment), 5
dispatcher/promote_test.go                  # Task 3 (new file)
dispatcher/recover_test.go                  # Task 5
internal/store/outbox.go                    # Task 6 (doc comment)
internal/store/types.go                     # Task 6 (doc comment)
cli/dispatch.go                             # Task 7a
internal/store/crud.go                      # Task 7b
```

---

### Task 0: Branch + match CI's `-race` in `make test-go`

`make test-go` currently runs `go test ./...`; CI (`.github/workflows/ci.yml:17`) runs `go test -race ./...`. Every later task in this plan touches `dispatcher/` concurrency (the inflight map, goroutines, channels) — running the whole suite under `-race` from the very first task, locally, closes the gap between "passes `make test`" and "passes CI" for the rest of this work.

- [ ] **Step 1: Create the working branch**

```bash
git checkout -b fix/kernel-hardening
```

- [ ] **Step 2: Measure current wall time (informational, not a gate)**

```bash
go clean -testcache && time go test ./... 2>&1 | tail -3
go clean -testcache && time go test -race ./... 2>&1 | tail -3
```

Measured on this machine at `f801d46`: plain `go test ./...` ≈ 10.9s wall; `go test -race ./...` ≈ 13.3s wall (≈ +22%, not the doubling `-race` sometimes causes — this suite's slowest packages, `internal/spawn` and `internal/gitops`, are already dominated by real subprocess spawns, not goroutine-heavy code the race detector instruments heavily). Expect low-teens seconds either way; not worth a CI timeout change.

- [ ] **Step 3: Update the Makefile**

In `Makefile`, replace:

```makefile
## test-go: run Go tests
test-go:
	go test ./...
```

with:

```makefile
## test-go: run Go tests (matches CI's `go test -race ./...` exactly, so a
## green `make test` locally means a green CI race build too -- adds modest
## wall time, ~10s -> ~13s on this repo; see AGENTS.md/this plan for the
## measured numbers if that ever needs re-justifying)
test-go:
	go test -race ./...
```

- [ ] **Step 4: Verify**

Run: `make test-go`
Expected: PASS, all packages, no race reports.

- [ ] **Step 5: Commit**

```bash
git add Makefile
git commit -m "chore(build): run go test -race in make test-go, matching ci"
```

---

### Task 1: [Critical] Fix the inflight-map leak on a failed continue-respawn

**Root cause.** `dispatcher/reconcile.go`'s `applyContinue` re-spawns the same `runID` for another turn via `spawnAttempt` (`dispatcher/spawn.go`). If that respawn fails — `d.ws.Ensure` or `d.spawner.Spawn` returns an error — `spawnAttempt` routes through `parkOrRetry`, which calls `store.Transition` to clear the claim and close the run (either `scheduleRetry` or `blockOnSpawnFailure`). The **store** is now correct. But neither failure branch ever calls `delete(d.inflight, runID)`, and `runID` is the *same* key the run has held since its first turn — so the stale `inflightRun` entry from the turn that just finished survives. On the very next `reconcile()` call, `d.inflight`'s heartbeat loop (`dispatcher/reconcile.go:26-30`) tries `store.Heartbeat` for that `runID`, finds `claim_lock` already cleared, and returns `"no active claim"`. `reconcile` returns that error; `Tick` returns it immediately (`dispatcher/dispatcher.go`'s `Tick` is a strict `if err != nil { return }` chain) — so `promote`, `selectAndClaim`, and `drainOutbox` never run for that tick, or any tick after it, because the same stale entry is still there next time. Only a dispatcher restart clears it.

**Fix.** `delete(d.inflight, runID)` in both of `spawnAttempt`'s failure branches. It's a no-op for a *fresh* claim's first spawn (`spawnClaim` → `spawnAttempt`, which never has a pre-existing `d.inflight[runID]` for a brand-new `runID`), and it correctly evicts the stale entry for a `continue`-respawn failure (the only case where `runID` was already a key).

**Files:**
- Modify: `dispatcher/spawn.go`
- Modify: `dispatcher/fakes_test.go` (extend `fakeSpawner` with a one-shot failure)
- Modify: `dispatcher/reconcile_test.go`

- [ ] **Step 1: Extend `fakeSpawner` to fail exactly one call**

`spawner.SpawnErr` currently fails *every* `Spawn` call once set — useless for this test, which needs turn 1 to succeed (to get a `continue` outcome inflight) and only turn 2's respawn to fail. Add a one-shot variant.

In `dispatcher/fakes_test.go`, in the `fakeSpawner` struct, add a field after `SpawnErr`:

```go
	// SpawnErr, if set, is returned by Spawn instead of a handle (for every
	// issue), simulating a Spawner-level failure (not a worker failure).
	SpawnErr error

	// FailOnCall, if > 0, makes exactly the Nth Spawn call (1-indexed, across
	// every issue) fail with SpawnErr, succeeding normally before and after
	// it -- unlike SpawnErr alone, which fails every call from the first.
	// Lets a test force one deterministic mid-sequence Spawn failure (e.g. a
	// "continue" outcome's respawn) while leaving an earlier turn's spawn (or
	// a later, unrelated issue's fresh claim) unaffected.
	FailOnCall int
```

Then rewrite `Spawn`'s body (currently uses a discarded `atomic.AddInt32(&f.spawnCount, 1)` and an unconditional `f.SpawnErr != nil` check):

```go
func (f *fakeSpawner) Spawn(ctx context.Context, spec spawn.WorkerSpec) (spawn.RunHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := int(atomic.AddInt32(&f.spawnCount, 1))
	f.specs = append(f.specs, spec)

	if f.SpawnErr != nil && (f.FailOnCall == 0 || n == f.FailOnCall) {
		return nil, f.SpawnErr
	}

	var res spawn.Result
	if q := f.ResultsQueue[spec.Issue]; len(q) > 0 {
		res = q[0]
		f.ResultsQueue[spec.Issue] = q[1:]
	} else if r, ok := f.Results[spec.Issue]; ok {
		res = r
	} else {
		res = spawn.Result{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeDone}}
	}

	return &fakeHandle{ctx: ctx, res: res, pid: n + 1000}, nil
}
```

(`FailOnCall == 0` preserves every existing test's behavior exactly — `SpawnErr` alone still fails every call. `pid: n + 1000` replaces the old redundant `atomic.AddInt32(&f.spawnCount, 0)` read with the count already in hand.)

- [ ] **Step 2: Write the failing test**

Append to `dispatcher/reconcile_test.go`. It needs `"errors"` added to the import block.

```go
// TestTick_ContinueRespawnFailureDoesNotLeakInflight asserts the fix for the
// Critical inflight-map leak: a "continue" outcome's respawn failure must not
// leave a stale d.inflight entry behind. Before the fix, the next tick's
// Heartbeat loop finds the store claim already cleared (by the respawn
// failure's own park/retry transition) and errors "no active claim",
// aborting reconcile -- and with it promote/selectAndClaim/drainOutbox --
// permanently (the SAME stale entry keeps tripping it on every later tick,
// not just once).
func TestTick_ContinueRespawnFailureDoesNotLeakInflight(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	// Turn 1 (spawn call 1) succeeds and reports "continue". Turn 2's
	// respawn (spawn call 2, triggered by applyContinue) fails at the
	// Spawner level -- exactly the path that used to leak d.inflight.
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeContinue, ThreadId: "thread-continue"},
	}
	spawner.SpawnErr = errors.New("fake exec failure: no such file or directory")
	spawner.FailOnCall = 2

	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig() // RecoverCap defaults to 0 (zero value): parkOrRetry always parks.
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil { // tick 1: claim + spawn turn 1
		t.Fatalf("tick 1: unexpected error: %v", err)
	}

	// Tick 2 drains turn 1's continue result (applyContinue), whose respawn
	// (spawn call 2) fails and parks issue-1 via blockOnSpawnFailure -- all
	// within reconcile's drainResults step. The SAME reconcile() call's
	// heartbeat loop runs immediately after: this is where the leak used to
	// surface, in this very tick, not a later one.
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 2: unexpected error: %v (a leaked inflight record must not wedge the heartbeat loop in the same tick the respawn failed)", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != string(contract.ColumnBlocked) {
		t.Fatalf("BoardStatus = %q, want blocked (the respawn failure parked it, RecoverCap=0)", got.BoardStatus)
	}

	// A second, independent ready issue proves the tick loop is still fully
	// alive on LATER ticks too, not just spared once: pre-fix, the leaked
	// entry keeps failing Heartbeat every tick, aborting reconcile before
	// promote/selectAndClaim/drainOutbox ever run again.
	seedReadyIssue(t, s, "issue-2", "coder", 1, 1000)
	spawner.Results["issue-2"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "PR opened"},
	}

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 3: unexpected error: %v", err)
	}
	if spawner.SpawnCount() != 3 {
		t.Fatalf("SpawnCount = %d, want 3 (turn 1, the failed respawn, and issue-2's fresh claim)", spawner.SpawnCount())
	}

	// And progress keeps happening on tick 4: issue-2's needs_review result
	// gets drained and applied normally.
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 4: unexpected error: %v", err)
	}
	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	var issue2 *store.IssueSnapshot
	for i := range snap.Issues {
		if snap.Issues[i].ID == "issue-2" {
			issue2 = &snap.Issues[i]
		}
	}
	if issue2 == nil {
		t.Fatalf("issue-2 missing from snapshot")
	}
	if issue2.BoardStatus != string(contract.ColumnReview) {
		t.Errorf("issue-2 BoardStatus = %q, want review", issue2.BoardStatus)
	}
}
```

Add `"github.com/xlyk/clipse/internal/store"` to `dispatcher/reconcile_test.go`'s import block if not already present (it isn't — check first).

- [ ] **Step 3: Run the test, verify it fails**

Run: `go test ./dispatcher/... -run TestTick_ContinueRespawnFailureDoesNotLeakInflight -v`
Expected: FAILS at "tick 2: unexpected error" — the wrapped error reads roughly `tick: reconcile: heartbeating run run-1: heartbeat for run run-1: no active claim`.

- [ ] **Step 4: Implement the fix**

In `dispatcher/spawn.go`, `spawnAttempt`'s two failure branches become:

```go
	workspace, err := d.ws.Ensure(issue)
	if err != nil {
		// A workspace/spawn failure is transient by nature, so it is eligible
		// for bounded auto-retry (auto-unblock layer 1); parkOrRetry falls back
		// to blockOnSpawnFailure once the budget is spent (or RecoverCap is 0).
		// Either way the store's claim on runID is being cleared right now, so
		// any stale d.inflight[runID] left over from a "continue" respawn
		// (this is the SAME runID as the turn that just finished -- see
		// applyContinue) must go with it: otherwise the next tick's Heartbeat
		// finds no active claim for a run this dispatcher still thinks is
		// inflight, errors, and aborts reconcile before promote/claim/outbox
		// ever run again. A no-op for a fresh claim's first spawn
		// (spawnClaim), which never has a pre-existing entry for runID.
		delete(d.inflight, runID)
		cause := fmt.Errorf("preparing workspace: %w", err)
		return d.parkOrRetry(ctx, issue, runID, lane, cause.Error(), contract.BlockKindTransient, d.now(), retryPayload{}, func() error {
			return d.blockOnSpawnFailure(ctx, issue.ID, runID, lane, cause)
		})
	}
```

and, further down (after `d.spawner.Spawn`):

```go
	handle, err := d.spawner.Spawn(spawnCtx, spec)
	if err != nil {
		cancel()
		// See the matching delete() in the Ensure error branch above: this
		// spawn attempt may be a "continue" respawn reusing runID, and its
		// failure clears the store claim, so the stale inflight record must
		// not survive it either.
		delete(d.inflight, runID)
		cause := fmt.Errorf("spawning worker: %w", err)
		return d.parkOrRetry(ctx, issue, runID, lane, cause.Error(), contract.BlockKindTransient, d.now(), retryPayload{}, func() error {
			return d.blockOnSpawnFailure(ctx, issue.ID, runID, lane, cause)
		})
	}
```

- [ ] **Step 5: Run the test, verify it passes**

Run: `go test -race ./dispatcher/... -run TestTick_ContinueRespawnFailureDoesNotLeakInflight -v`
Expected: PASS.

- [ ] **Step 6: Full suite**

Run: `make test-go`
Expected: PASS, no races.

- [ ] **Step 7: Commit**

```bash
git add dispatcher/spawn.go dispatcher/fakes_test.go dispatcher/reconcile_test.go
git commit -m "fix(dispatcher): delete the inflight record on a failed continue respawn"
```

---

### Task 2: [Major] Never adopt an unclaimed `running` status from Linear

**Root cause.** `dispatcher/poll.go`'s `reconcileLinearDivergence` calls `adoptLinearMove` whenever the polled Linear status differs from the store's `board_status` **and** the issue holds no active claim. Nothing stops `linearStatus` from being `"running"` in that branch — a human dragging the card to a "Running" column in Linear, or a stale label lingering through the restart-requeue window, both surface here. `adoptLinearMove` writes `board_status='running'` via a plain `store.Transition` with `ClearClaim` unset (there is no claim to clear) — producing a `running` row with `claim_lock IS NULL`. That row is now a ghost: `store.ClaimReady`'s CAS only claims from `board_status='ready'`, so it can never be claimed again; `store.ReleaseStaleClaims` only looks at `claim_expires`, which stays `NULL` forever on this row, so it can never be released either. This directly violates the kernel invariant that `running` is entered **only** via the CAS claim.

**Fix.** `reconcileLinearDivergence` already has the right tool for "Linear's view can't be trusted right now": `reassertOwnedState`, used today when the issue holds an active claim. Extend that condition: also re-assert (rather than adopt) whenever the observed `linearStatus` is `"running"`, regardless of claim state — since reaching this function at all means `issue.BoardStatus != linearStatus`, an unclaimed `"running"` observation can *never* be backed by a real claim on this issue. This is a two-line change with no new function and no new `TransitionReq` field.

**Files:**
- Modify: `dispatcher/poll.go`
- Modify: `dispatcher/poll_test.go`
- Modify: `AGENTS.md` (the invariant this fixes is documented there; keep it accurate)

- [ ] **Step 1: Write the failing test**

Append to `dispatcher/poll_test.go`:

```go
// TestTick_PollNeverAdoptsRunningWithoutClaim asserts the fix for adopting an
// unclaimed "running" status from Linear: a human dragging a card to Running
// (or a restart-requeue race observing a stale label) must not be adopted --
// board_status='running' is entered ONLY via the CAS claim (store.ClaimReady/
// ClaimColumn). Adopting it here would write claim_lock=NULL, board_status=
// 'running': unclaimable by ClaimReady's CAS (which requires board_status=
// 'ready') and unreleasable by ReleaseStaleClaims (which only looks at
// claim_expires, permanently NULL on this row).
func TestTick_PollNeverAdoptsRunningWithoutClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// issue-1 sits at 'ready' with no active claim.
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	// Linear reports "running" for this issue with no backing claim.
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "running", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Fatalf("BoardStatus = %q, want unchanged ready (an unclaimed running status must never be adopted)", got.BoardStatus)
	}
	if got.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want still unclaimed")
	}

	// Instead of adopting, the dispatcher corrects Linear's stale view: a
	// setstate mirror pushing the store's real status (ready) back.
	var sawReadyReassert bool
	for _, c := range lc.SetStateCalls {
		if c.IssueID == "issue-1" && c.TargetColumn == "ready" {
			sawReadyReassert = true
		}
	}
	if !sawReadyReassert {
		t.Errorf("SetStateCalls = %+v, want a setstate -> ready reassertion correcting Linear's stray running", lc.SetStateCalls)
	}

	// The issue remains genuinely claimable on a later tick with real caps.
	cfg := testConfig()
	d2 := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(1000))
	if err := d2.Tick(ctx); err != nil {
		t.Fatalf("second Tick: unexpected error: %v", err)
	}
	got2, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue after second tick: unexpected error: %v", err)
	}
	if got2.BoardStatus != "running" {
		t.Errorf("BoardStatus after second tick = %q, want running (claimed for real this time)", got2.BoardStatus)
	}
	if !got2.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = false after second tick, want a real claim backing running")
	}
}
```

- [ ] **Step 2: Run the test, verify it fails**

Run: `go test ./dispatcher/... -run TestTick_PollNeverAdoptsRunningWithoutClaim -v`
Expected: FAILS — `got.BoardStatus` is `"running"` (adopted), not `"ready"`.

- [ ] **Step 3: Implement the fix**

In `dispatcher/poll.go`, `reconcileLinearDivergence` becomes:

```go
func (d *Dispatcher) reconcileLinearDivergence(ctx context.Context, issueID, linearStatus string, now int64) error {
	issue, err := d.store.GetIssue(ctx, issueID)
	if err != nil {
		return fmt.Errorf("loading issue %s: %w", issueID, err)
	}

	if issue.BoardStatus == linearStatus {
		return nil
	}

	// board_status="running" is entered ONLY via the CAS claim
	// (store.ClaimReady/ClaimColumn) -- see AGENTS.md's kernel invariant.
	// Reaching here at all already means issue.BoardStatus != linearStatus,
	// so an observed linearStatus=="running" can NEVER be backed by a real
	// claim on THIS issue's own row: either a human dragged the card to
	// Running by hand, or Linear's own mirror of a prior (now-released)
	// claim hasn't caught up yet (a restart-requeue race). Adopting it would
	// write board_status='running' with claim_lock left NULL -- a row
	// ClaimReady's CAS can never claim again (it requires board_status=
	// 'ready') and ReleaseStaleClaims can never release (it only looks at
	// claim_expires, permanently NULL here). Treat it exactly like the
	// dispatcher-owns-this-claim case below: re-assert the store's real
	// status instead of trusting Linear's.
	if !issue.ClaimLock.Valid && linearStatus != string(contract.ColumnRunning) {
		return d.adoptLinearMove(ctx, issueID, issue.BoardStatus, linearStatus, now)
	}
	return d.reassertOwnedState(ctx, issueID, issue.BoardStatus, now)
}
```

(`contract` is already imported in `dispatcher/poll.go`.)

- [ ] **Step 4: Run the full poll test file, then the full suite**

Run: `go test -race ./dispatcher/... -run TestTick_Poll -v`
Expected: ALL PASS — including the existing `TestTick_PollAdoptsHumanMoveWhenUnclaimed`, `TestTick_PollReassertsDispatcherOwnedStateWhenClaimed`, and the two rework-count tests, all of which script `linearStatus` values other than `"running"` and are unaffected by this change (verified: no existing test scripts `Status: "running"` in a `linear.Issue` literal).

Run: `make test-go`
Expected: PASS.

- [ ] **Step 5: Keep AGENTS.md's invariant accurate**

In `AGENTS.md`, the `SQLite is runtime truth; Linear is task intent` bullet currently ends:

```
humans may move cards, and the poll adopts the move only when the issue holds no active claim (else SQLite wins and the outbox re-asserts).
```

Replace with:

```
humans may move cards, and the poll adopts the move only when the issue holds no active claim **and** the observed Linear status isn't `running` (else SQLite wins and the outbox re-asserts — `running` is entered ONLY via the CAS claim, so an unclaimed `running` observed from Linear can never be genuine).
```

- [ ] **Step 6: Commit**

```bash
git add dispatcher/poll.go dispatcher/poll_test.go AGENTS.md
git commit -m "fix(dispatcher): never adopt an unclaimed running status from linear"
```

---

### Task 3: [Major] Make cancelled Linear dependencies observable and terminal

**Root cause.** Three things compound:

1. `internal/linear/http_client.go`'s `CandidateIssuesQuery` filters `state: { type: { nin: ["completed", "canceled", "duplicate"] } }` — once a Linear issue is cancelled, it's excluded from every future poll. Its store row is never `UpsertIssue`'d again, so `board_status` is frozen at whatever it was pre-cancellation.
2. `internal/linear/status.go`'s `statusFromWorkflowName` has no `canceled`/`cancelled` mapping; an unrecognized name falls back to `"todo"`.
3. `contract.Column` has no `cancelled` value at all.

Meanwhile `dispatcher/promote.go`'s `terminalStatuses` map **already** contains `"cancelled": true`, and `dispatcher/recover.go`'s orphan-recovery **already** special-cases `issue.BoardStatus == "cancelled"` — both dead code, since nothing ever produces that string.

**Design decision: store-level fix, no contract/schema change.** `issues.board_status` is unconstrained `TEXT` (`internal/store/migrations.go:18`, no CHECK constraint) — the dispatcher already treats it as plain string comparisons throughout (`"done"`, `"blocked"`, `"cancelled"` in `promote.go`/`recover.go` today). `contract.Column` is used by `board.Next`'s transition table and the worker-result schema, but a cancelled issue is **never claimed** (verified: `store.ClaimColumn` is only ever invoked with the literal columns `"review"`, `"rework"`, `"merging"`; `store.ClaimReady` requires `board_status='ready'`) — so `board.Next` never sees `"cancelled"` as a `current` column, and the TUI's `switch is.BoardStatus` (`cli/tui/model.go`) has no `default` panic, it just leaves an unrecognized status out of its bucketed views (cosmetic, out of scope for this kernel-hardening pass). Given `promote.go`/`recover.go` were **already written** to expect this exact raw string, completing that wiring — rather than adding a `contract.ColumnCancelled` enum value and running `make codegen` — is the smallest change that makes the existing (dead) logic correct, and avoids touching the Python side entirely (confirmed: `schema/worker-result.schema.json` references `board.schema.json`'s `Lane` and `BlockKind` `$defs`, never `Column`, so `datamodel-code-generator`'s Python output wouldn't even change if `Column` gained a value — but the plan constraint is "avoid unless truly required," and it isn't).

The remaining design choice is **how** to detect cancellation robustly: matching on the Linear state's `name` (as every other column here does) is fragile specifically for this case, because an unrecognized name silently falls back to `"todo"` — which would make a cancelled blocker look *active* instead of terminal, the opposite of correct. Linear's state `type` field is a small, fixed, unrenameable vocabulary (`backlog`/`unstarted`/`started`/`completed`/`canceled`/`triage`), unlike its team-configurable `name`. So: fetch `state.type` alongside `state.name`, and let `type=="canceled"` short-circuit the mapping regardless of what the team calls the state.

**Files:**
- Modify: `internal/linear/http_client.go` (query text + doc comment)
- Modify: `internal/linear/normalize.go` (`State.Type` field, threading)
- Modify: `internal/linear/status.go` (`statusFromWorkflowName` signature)
- Modify: `internal/linear/normalize_test.go`
- Modify: `internal/linear/http_client_test.go`
- Modify: `dispatcher/promote.go`, `dispatcher/recover.go` (doc comments only — the logic is already correct and unchanged)
- Create: `dispatcher/promote_test.go` (end-to-end regression demo)

- [ ] **Step 1: Write the failing tests**

Append to `internal/linear/normalize_test.go` (uses an inline JSON literal rather than the shared `testdata/candidate_issues.json` fixture, so this task's diff doesn't ripple into `http_client_loopback_test.go`'s fixture-based assertions):

```go
func TestNormalizeCandidateIssues_CancelledStateTypeOverridesName(t *testing.T) {
	// The state's NAME is deliberately something a name-based lookup would
	// never recognize ("Won't Fix") -- proving the mapping is driven by the
	// fixed, unrenameable state TYPE, not the team-configurable display name.
	const raw = `{
		"data": {
			"issues": {
				"nodes": [
					{
						"id": "cancelled-1",
						"identifier": "CLP-16",
						"title": "Abandoned thing",
						"description": "",
						"priority": 3,
						"branchName": "clp-16-abandoned",
						"updatedAt": "2026-07-01T16:00:00.000Z",
						"state": { "name": "Won't Fix", "type": "canceled" },
						"labels": { "nodes": [] },
						"inverseRelations": { "nodes": [] }
					}
				]
			}
		}
	}`

	issues, err := linear.NormalizeCandidateIssues([]byte(raw))
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("len(issues) = %d, want 1", len(issues))
	}
	if issues[0].Status != "cancelled" {
		t.Errorf("Status = %q, want %q (type-driven, not name-driven)", issues[0].Status, "cancelled")
	}
}
```

Append to `internal/linear/http_client_test.go`:

```go
func TestCandidateIssuesQuery_KeepsCancelledIssuesInScope(t *testing.T) {
	// "canceled" must stay OUT of the state-type exclusion filter: it is the
	// dispatcher's only signal that a Linear-cancelled issue exists at all.
	// Unlike "completed" (the dispatcher learns about a merge from its OWN
	// action, before Linear's state even changes) and "duplicate" (never a
	// candidate to begin with), cancellation is a human-only Linear event.
	// Excluding it left a cancelled blocker's stale store row frozen forever,
	// permanently stalling any dependent still waiting on it in Todo.
	if strings.Contains(linear.CandidateIssuesQuery, `"canceled"`) {
		t.Errorf("candidate query excludes canceled issues, want them included so cancellation is observable")
	}
	for _, excluded := range []string{"completed", "duplicate"} {
		if !strings.Contains(linear.CandidateIssuesQuery, `"`+excluded+`"`) {
			t.Errorf("candidate query must still exclude %q", excluded)
		}
	}
	// state.type must be fetched so normalize can detect a cancelled state
	// regardless of its (per-team-configurable) display name.
	if !strings.Contains(linear.CandidateIssuesQuery, "type") {
		t.Errorf("candidate query must fetch state.type for cancellation detection")
	}
}
```

- [ ] **Step 2: Run the tests, verify they fail**

Run: `go test ./internal/linear/... -run "TestNormalizeCandidateIssues_CancelledStateTypeOverridesName|TestCandidateIssuesQuery_KeepsCancelledIssuesInScope" -v`
Expected: both FAIL — the first with a compile error is not expected (signature unchanged this task), but `Status` will be `"todo"` (the fallback), not `"cancelled"`; the second fails both the "canceled" exclusion check and the "type" fetch check.

- [ ] **Step 3: Implement**

In `internal/linear/http_client.go`, replace `CandidateIssuesQuery` and its doc comment:

```go
// CandidateIssuesQuery fetches active-state issues on the configured team
// along with the fields NormalizeCandidateIssues needs: title, description,
// workflow state name AND type, agent:<lane> labels, inverse blocking
// relations (the issues that block this one — see below), priority, branch
// name, and updatedAt.
//
// It fetches inverseRelations, NOT relations: a dependency of an issue is an
// issue that blocks it, and Linear records a blocking relationship once, on
// the blocker's source side (type "blocks"). The blocked issue therefore sees
// it in inverseRelations (issue = the blocker). Fetching source-side relations
// instead inverted the dependency graph — a dependent issue looked
// dependency-free and promoted immediately while its blocker waited on it.
//
// title/description are the task text a Coder-lane worker actually needs to
// do the work (the dispatcher injects them into the worker's environment as
// CLIPSE_ISSUE_TEXT) -- without them here, that env var is always empty
// regardless of anything downstream.
//
// Excluding "completed" and "duplicate" (Linear has no "active" type; the
// real types are backlog/unstarted/started/completed/canceled/triage) keeps
// that work out of the candidate set; the dispatcher decides dispatchability
// from Status/Deps, not from this query. "canceled" is DELIBERATELY left in
// scope, unlike the other two: a completed issue's board_status is set by the
// dispatcher's OWN action (the git-operator lane merges it, then the
// dispatcher writes board_status="done" itself, before Linear's state even
// changes) and a duplicate was never a candidate to begin with, but
// cancellation is a human-only Linear event the dispatcher has no other way
// to learn about — excluding it here left a Linear-cancelled blocker's stale
// store row frozen at its pre-cancellation status forever, permanently
// stalling any dependent still waiting on it in Todo (see
// dispatcher/promote.go's dependency-gating, and status.go's cancelled-type
// mapping). Filtering to team.key scopes the candidate set to the single team
// clipse is configured against (config.Config.TeamKey), so a workspace with
// other teams' issues never surfaces them as candidates.
const CandidateIssuesQuery = `query CandidateIssues($teamKey: String!) {
  issues(filter: { state: { type: { nin: ["completed", "duplicate"] } }, team: { key: { eq: $teamKey } } }) {
    nodes {
      id
      identifier
      title
      description
      priority
      branchName
      updatedAt
      state {
        name
        type
      }
      labels {
        nodes {
          name
        }
      }
      inverseRelations {
        nodes {
          type
          issue {
            id
          }
        }
      }
    }
  }
}`
```

In `internal/linear/normalize.go`, add `Type` to `issueNode.State`:

```go
	State       struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
```

and update `normalizeIssueNode`'s `Status` line:

```go
		Status:      statusFromWorkflowName(n.State.Name, n.State.Type),
```

In `internal/linear/status.go`, replace `statusFromWorkflowName`:

```go
// statusFromWorkflowName maps a Linear workflow-state name (and its fixed
// state TYPE) to our board status. stateType is checked first and takes
// priority: Linear's six state types (backlog/unstarted/started/completed/
// canceled/triage) are a closed, unrenameable vocabulary, unlike a state's
// display NAME, which a team can call anything ("Won't Fix", "Abandoned",
// ...). Name-matching alone (as every other column here still does) would be
// fragile specifically for cancellation: an unrecognized name falls back to
// "todo" (see below), which would make a cancelled blocker look ACTIVE
// instead of terminal -- the opposite of what dispatcher/promote.go's
// dependency gating needs. "cancelled" (double-l) is deliberately not a
// contract.Column value; issues.board_status is unconstrained TEXT (see
// internal/store/migrations.go), and dispatcher/promote.go +
// dispatcher/recover.go already special-case this exact string as terminal.
//
// Names not present in statusByWorkflowName fall back to "todo" so an
// unrecognized/renamed Linear state doesn't crash normalization; it just
// won't be picked up as ready/running/etc until the mapping is fixed.
func statusFromWorkflowName(name, stateType string) string {
	if stateType == "canceled" {
		return "cancelled"
	}
	if col, ok := statusByWorkflowName[strings.ToLower(name)]; ok {
		return col
	}
	return "todo"
}
```

In `dispatcher/promote.go`, update `terminalStatuses`'s doc comment (logic unchanged):

```go
// terminalStatuses are the board columns board.Promote treats as "this
// dependency will never re-enter an active column" (see board.DepState.
// Terminal). "cancelled" (double-l) is not a contract.Column value -- Linear
// cancellation is a human-only event with no dispatcher-owned transition, so
// it's written as a raw board_status string by adoptLinearMove once
// internal/linear observes it (status.go's statusFromWorkflowName, driven by
// the state's TYPE; http_client.go's CandidateIssuesQuery used to exclude
// cancelled issues from the poll entirely, which is why this was dead code
// until both were fixed together).
var terminalStatuses = map[string]bool{
	string(contract.ColumnDone): true,
	"cancelled":                 true,
}
```

In `dispatcher/recover.go`, add a short comment above the existing check (logic unchanged):

```go
	// "cancelled" (like "done") is a genuinely terminal issue whose leftover
	// run row is restart debris, not a real orphan -- see promote.go's
	// terminalStatuses for how a store row actually reaches this string.
	if issue.BoardStatus == "done" || issue.BoardStatus == "cancelled" {
```

- [ ] **Step 4: Run the linear package tests, verify green**

Run: `go test ./internal/linear/... -v`
Expected: ALL PASS, including the two new tests and every existing test (the 4-node shared fixture has no `state.type` field, which unmarshals to `""` for all of them — `statusFromWorkflowName(name, "")` falls straight through to the unchanged name-based lookup, so `TestNormalizeCandidateIssues_FromFixture` and `TestHTTPClient_CandidateIssues_ParsesLoopbackResponse` are unaffected).

- [ ] **Step 5: Add the end-to-end regression test**

This test does **not** fail on unfixed code — `dispatcher.promote`/`reconcileLinearDivergence` already handle the raw string `"cancelled"` correctly today; the bug was entirely upstream (no producer). `linear.MockClient` returns pre-normalized `Issue` values directly, bypassing the query/normalize layer this task just fixed, so scripting `Status: "cancelled"` on it exercises `promote.go`'s dependency-gating logic in isolation from the fix above. It's included anyway as a regression guard locking in the full pipeline's behavior once both halves (this task's producer + the kernel's already-correct consumer) are combined — run it now to confirm it's green, and it stays a permanent regression test for the whole path.

Create `dispatcher/promote_test.go`:

```go
package dispatcher_test

import (
	"context"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/store"
)

// TestTick_PollCancelledDependencyUnblocksPromotion is an end-to-end
// regression guard for the finding-3 fix: a Linear-cancelled blocker, once
// adopted into SQLite as board_status="cancelled" (Task 3's producer fix in
// internal/linear), counts as terminal for board.Promote — so a dependent
// sitting in Todo waiting on it is no longer stuck forever. This assertion
// already holds against dispatcher/promote.go's pre-existing (previously
// dead) logic; it's here to lock in the full pipeline, not because this
// task changed any dispatcher code.
func TestTick_PollCancelledDependencyUnblocksPromotion(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// issue-2 (the blocker) sits at "review", unclaimed -- as if a human
	// cancelled the ticket mid-review instead of letting it finish.
	seedColumnIssue(t, s, "issue-2", "review", 1, 100)
	// issue-1 (the dependent) sits in Todo, waiting on issue-2.
	issue1 := store.Issue{
		ID: "issue-1", Identifier: "CLP-1", LaneLabel: "coder", BoardStatus: "todo",
		Deps: `["issue-2"]`, Priority: 1, BranchName: "issue-1-branch",
		UpdatedAt: 100, LastSeen: 100, CreatedAt: 100,
	}
	if err := s.UpsertIssue(ctx, issue1); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	// Linear now reports issue-2 as cancelled.
	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-2", Identifier: "CLP-2", Status: "cancelled", Lane: "coder", Priority: 1, BranchName: "issue-2-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got2, err := s.GetIssue(ctx, "issue-2")
	if err != nil {
		t.Fatalf("GetIssue(issue-2): unexpected error: %v", err)
	}
	if got2.BoardStatus != "cancelled" {
		t.Fatalf("issue-2 BoardStatus = %q, want cancelled (adopted from Linear)", got2.BoardStatus)
	}

	got1, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue(issue-1): unexpected error: %v", err)
	}
	if got1.BoardStatus != "ready" {
		t.Errorf("issue-1 BoardStatus = %q, want ready (promoted once its cancelled dependency counted as terminal)", got1.BoardStatus)
	}
}
```

- [ ] **Step 6: Full suite**

Run: `make test-go`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/linear/http_client.go internal/linear/normalize.go internal/linear/status.go \
        internal/linear/normalize_test.go internal/linear/http_client_test.go \
        dispatcher/promote.go dispatcher/recover.go dispatcher/promote_test.go
git commit -m "fix(linear): keep cancelled issues in the candidate poll so promote sees them as terminal"
```

---

### Task 4: [Major] Don't lose a worker result on a transient `GetIssue` failure

**Root cause.** `dispatcher/reconcile.go`'s `applyResult` receives one `runResult` already popped off `d.results` by `drainResults`'s `select` loop. If `d.store.GetIssue(ctx, rr.issueID)` fails, `applyResult` returns the wrapped error immediately — but `rr` was a local variable inside `drainResults`'s `case rr := <-d.results:` arm; once the call stack unwinds via the error return, `rr` is gone forever. The run's `Wait`-goroutine has already sent its one and only result and exited, so nothing will ever produce this result again. `d.inflight[rr.runID]` is untouched (this is a different code path from Task 1 — no spawn ever failed here), so `Heartbeat` keeps renewing its still-valid store claim on every later tick, silently and permanently: the issue's `board_status` never advances past `running`, and the lane-cap slot it occupies is never freed.

**Fix.** `d.results` has exactly one reader — the `Tick` goroutine — so it's race-free for that same goroutine to push `rr` back onto it before propagating the error. The next tick's `drainResults` picks `rr` up again and retries `applyResult` from the top.

**Files:**
- Modify: `dispatcher/reconcile.go`
- Modify: `dispatcher/reconcile_test.go`

- [ ] **Step 1: Write the failing test**

Append to `dispatcher/reconcile_test.go`. Add `"github.com/xlyk/clipse/internal/store"` to the import block if Task 1 didn't already add it.

```go
// TestTick_GetIssueFailureDuringReconcile_DoesNotLoseResult asserts the fix
// for the destructive-result-consumption bug: a GetIssue failure inside
// applyResult must not drop the worker's result on the floor. Dispatcher.store
// is a concrete *store.Store (no interface seam to mock a targeted failure),
// so this simulates a real, store-level GetIssue failure the only way
// available: deleting the row out from under the live run via a raw SQL
// DELETE against the same file (store.DB() is an established test escape
// hatch -- see dispatcher/recover_test.go's UPDATE-based adversarial-state
// setup). There is no production code path that deletes an issue row; this
// stands in for the realistic cause, a transient store hiccup.
func TestTick_GetIssueFailureDuringReconcile_DoesNotLoseResult(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	spawner := newFakeSpawner()
	spawner.Results["issue-1"] = spawn.Result{
		Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "PR opened"},
	}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	d := newTestDispatcher(t, testConfig(), s, lc, spawner, ws, fixedClock(1000))

	// Tick 1: claim + spawn issue-1 (now inflight, running).
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 1: unexpected error: %v", err)
	}

	// Snapshot the full row so it can be restored byte-for-byte, then delete
	// it out from under the live run.
	before, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue (snapshot): unexpected error: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM issues WHERE id = ?`, "issue-1"); err != nil {
		t.Fatalf("simulating store failure (DELETE): unexpected error: %v", err)
	}

	// Tick 2 drains the spawned run's needs_review result; applyResult's
	// GetIssue fails against the deleted row, so this tick genuinely errors
	// -- expected, and unrelated to the bug under test.
	if err := d.Tick(ctx); err == nil {
		t.Fatalf("tick 2: want an error surfaced from the simulated GetIssue failure")
	}

	// The simulated hiccup clears (a real one would too, eventually):
	// restore the row exactly as it was, claim and all.
	if err := s.UpsertIssue(ctx, *before); err != nil {
		t.Fatalf("restoring issue-1: unexpected error: %v", err)
	}

	// Tick 3: the needs_review result must not have been silently dropped on
	// tick 2's failure -- it must still be applied now that the store is
	// healthy again.
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 3: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue after tick 3: unexpected error: %v", err)
	}
	if got.BoardStatus != string(contract.ColumnReview) {
		t.Errorf("BoardStatus = %q, want review (the needs_review result must survive a transient GetIssue failure, not be lost)", got.BoardStatus)
	}
	if got.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared (run-1 closed out normally)")
	}
}
```

- [ ] **Step 2: Run the test, verify it fails**

Run: `go test ./dispatcher/... -run TestTick_GetIssueFailureDuringReconcile_DoesNotLoseResult -v`
Expected: FAILS at the tick-3 `BoardStatus` assertion — it's still `"running"` (the result was dropped on tick 2 and never retried).

- [ ] **Step 3: Implement the fix**

In `dispatcher/reconcile.go`, `applyResult` becomes:

```go
func (d *Dispatcher) applyResult(ctx context.Context, rr runResult) error {
	inf, ok := d.inflight[rr.runID]
	if !ok {
		// No inflight record (e.g. reconciled already via some other path);
		// nothing left to do for this result.
		return nil
	}
	inf.cancel()

	issue, err := d.store.GetIssue(ctx, rr.issueID)
	if err != nil {
		// Put rr back on the channel rather than drop it: rr's Wait-goroutine
		// has already exited (a one-shot send), so if this error propagates
		// without holding onto rr somehow, that run's actual outcome is lost
		// forever -- inf stays in d.inflight, Heartbeat keeps renewing its
		// still-valid store claim every tick, and the lane-cap slot it
		// occupies never frees. d.results has exactly one reader (this Tick
		// goroutine), so sending back into it here is race-free; the next
		// tick's drainResults retries applyResult for rr from the top. A
		// GetIssue failure has no other realistic cause than a transient
		// store hiccup (nothing in production deletes issue rows), so this
		// self-heals within a tick or two.
		d.results <- rr
		return fmt.Errorf("loading issue %s for run %s: %w", rr.issueID, rr.runID, err)
	}

	if rr.res.Err != nil {
		delete(d.inflight, rr.runID)
		reason := blockReasonFor(rr.res.Err)
		// A run-level failure (crash / malformed result / timeout) is transient
		// by nature, so it is eligible for bounded auto-retry (auto-unblock
		// layer 1); parkOrRetry falls back to the plain blockRun park once the
		// budget is spent (or when RecoverCap is 0).
		return d.parkOrRetry(ctx, *issue, rr.runID, inf.lane, reason, contract.BlockKindTransient, d.now(), retryPayload{}, func() error {
			return d.blockRun(ctx, *issue, rr.runID, inf.lane, reason)
		})
	}

	outcome := string(rr.res.Worker.Outcome)

	if outcome == string(contract.WorkerResultOutcomeContinue) {
		return d.applyContinue(ctx, *issue, rr, inf)
	}

	delete(d.inflight, rr.runID)
	return d.applyTerminalWorkerOutcome(ctx, *issue, rr.runID, inf.lane, rr.res.Worker)
}
```

(Only the `GetIssue` error branch changes; the rest of the function is unchanged — reproduced in full per the plan's "COMPLETE code" convention.)

- [ ] **Step 4: Run the test, verify it passes**

Run: `go test -race ./dispatcher/... -run TestTick_GetIssueFailureDuringReconcile_DoesNotLoseResult -v`
Expected: PASS.

- [ ] **Step 5: Full suite**

Run: `make test-go`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add dispatcher/reconcile.go dispatcher/reconcile_test.go
git commit -m "fix(dispatcher): requeue a run result instead of dropping it on a getissue failure"
```

---

### Task 5: [Minor–Moderate] Exempt the git-operator lane from the orphan attempt cap

**Root cause.** `dispatcher/gitops.go`'s `mergingTTL` is deliberately short (`cfg.PollIntervalS`, not `cfg.MaxRuntimeS`): a "merging" claim's own natural expiry is what drives `claimAndRunGitops`'s CI-pending recheck cadence (`applyGitopsResult`'s `OutcomeCIPending` branch does nothing, letting `ReleaseStaleClaims` free the claim by the next poll). Every recheck cycle re-claims via `store.ClaimColumn`, which computes `attempt` as `nextAttempt`'s issue-global `MAX(attempt)+1` across every run this issue has ever had — a genuine "how many times has this failed" counter for the coder/reviewer lanes, but for git-operator it just counts "how many times have we re-polled CI," which has nothing to do with failure. A CI run pending for 30 minutes at a 30s poll interval accumulates roughly 60 "attempts" with zero real failures. `dispatcher.RecoverOrphans` (run once at daemon startup) checks `run.Attempt >= d.cfg.MaxAttempts` on whatever run is `status='running'` at restart time — for a merging card mid-CI-wait, that's this same inflated counter, so a restart during a long CI run can park an otherwise-healthy card.

**Fix.** Exempt `run.Lane == "git_operator"` from this specific check in `recoverOrphanRun`, always requeueing it back to `merging` on restart. This doesn't remove all bounds on git-operator retries: `applyGitopsResult`'s `OutcomeNotMergeable` branch already routes a genuine, deterministic failure through `parkOrRetry` (needs_input for a non-retriable reason — parks immediately; transient for a retriable one — bounded by `cfg.RecoverCap`, a **separate**, persisted-across-restarts counter (`issues.recover_attempts` is a store column, unaffected by a dispatcher restart) that still eventually parks a genuinely-broken merge regardless of how many times the dispatcher restarts in the meantime.

**Files:**
- Modify: `dispatcher/recover.go`
- Modify: `dispatcher/recover_test.go`

- [ ] **Step 1: Write the failing test**

Append to `dispatcher/recover_test.go` (mirrors the existing `TestRecoverOrphans_DownstreamColumnBlocksWhenAttemptAtMax`, which asserts the cap correctly *still* applies for the reviewer lane — this test asserts it correctly does *not* for git_operator):

```go
// TestRecoverOrphans_GitOperatorLaneIgnoresAttemptCap asserts the fix for
// finding 5: the git-operator lane's "attempt" counter inflates on every
// CI-pending recheck cycle (claimAndRunGitops re-claims "merging" roughly
// every PollIntervalS -- see mergingTTL's doc comment), not on a genuine
// failure, so a restart mid-CI-wait must not mistake a high accumulated
// attempt count for N real failures and park an otherwise-healthy card.
// Unlike TestRecoverOrphans_DownstreamColumnBlocksWhenAttemptAtMax (the
// reviewer lane, where the cap correctly still applies), a git_operator
// orphan always requeues back to merging regardless of Attempt.
func TestRecoverOrphans_GitOperatorLaneIgnoresAttemptCap(t *testing.T) {
	s := openTestStore(t)
	boardDir := t.TempDir()

	handle := spawnRealOrphan(t, boardDir, "issue-1", "git_operator")
	pid := handle.PID()
	startedAt := handle.ProcStartedAt()

	cfg := testConfig()
	cfg.MaxAttempts = 2
	const runID = "orphan-run"
	// Attempt is far past MaxAttempts -- exactly what dozens of CI-pending
	// recheck cycles would accumulate on a slow CI run, not a real failure.
	seedOrphanColumnRun(t, s, "issue-1", "merging", "git_operator", runID, pid, startedAt, 50, 1000)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, cfg, s, lc, spawner, ws, fixedClock(2000))

	if err := d.RecoverOrphans(context.Background()); err != nil {
		t.Fatalf("RecoverOrphans: unexpected error: %v", err)
	}
	_, _ = handle.Wait()

	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if issue.BoardStatus != string(contract.ColumnMerging) {
		t.Errorf("BoardStatus = %q, want merging (git-operator orphan requeues regardless of attempt)", issue.BoardStatus)
	}
	if issue.ClaimLock.Valid {
		t.Errorf("ClaimLock.Valid = true, want cleared")
	}

	run := getRunStatus(t, s, runID)
	if run != "orphaned" {
		t.Errorf("run status = %q, want orphaned", run)
	}
}
```

- [ ] **Step 2: Run the test, verify it fails**

Run: `go test ./dispatcher/... -run TestRecoverOrphans_GitOperatorLaneIgnoresAttemptCap -v`
Expected: FAILS — `BoardStatus` is `"blocked"` (the cap wrongly tripped), not `"merging"`.

- [ ] **Step 3: Implement**

In `dispatcher/recover.go`, add `"github.com/xlyk/clipse/internal/contract"` to the imports, and change `recoverOrphanRun`:

```go
func (d *Dispatcher) recoverOrphanRun(ctx context.Context, run store.Run) error {
	reaped, err := d.reapRunProcess(run)
	if err != nil {
		return fmt.Errorf("reaping process: %w", err)
	}
	d.logger.Info("orphan run recovery: process check", "run_id", run.RunID, "issue_id", run.IssueID, "outcome", reaped.String())

	issue, err := d.store.GetIssue(ctx, run.IssueID)
	if err != nil {
		return fmt.Errorf("loading issue %s: %w", run.IssueID, err)
	}

	// "cancelled" (like "done") is a genuinely terminal issue whose leftover
	// run row is restart debris, not a real orphan -- see promote.go's
	// terminalStatuses for how a store row actually reaches this string.
	if issue.BoardStatus == "done" || issue.BoardStatus == "cancelled" {
		// The issue already finished; this run row is just restart debris.
		// Blocking here would flap a terminal ticket back to blocked and
		// mirror that to Linear (Reflex retro: done tickets un-done by every
		// restart). Close the run and leave the issue alone. CloseRun's extra
		// args (resultJSON/errStr/tokens) are empty here -- there is no worker
		// result to record for leftover debris.
		if err := d.store.CloseRun(ctx, run.RunID, "terminalized", "", "", 0, 0); err != nil {
			return fmt.Errorf("terminalizing leftover run %s on %s issue %s: %w", run.RunID, issue.BoardStatus, issue.ID, err)
		}
		d.logger.Info("orphan run terminalized (issue already terminal)", "issue_id", issue.ID, "run_id", run.RunID, "board_status", issue.BoardStatus)
		return nil
	}

	if run.Lane == string(contract.LaneGitOperator) {
		// The git-operator lane's "attempt" counter inflates on every
		// CI-pending recheck cycle (claimAndRunGitops re-claims "merging"
		// roughly every PollIntervalS via mergingTTL's short natural expiry
		// -- see mergingTTL's doc comment), NOT on a genuine failure: gitops
		// itself decides retriability per outcome (applyGitopsResult's
		// OutcomeNotMergeable branch, bounded separately and persistently by
		// issues.recover_attempts via parkOrRetry -- a store column, unlike
		// this run-row-derived MaxAttempts check, so it survives a restart
		// intact). A restart mid-CI-wait must not mistake "still waiting" for
		// "N failed attempts" and park an otherwise-healthy card -- always
		// requeue it back to merging regardless of Attempt.
		return d.requeueOrphan(ctx, *issue, run)
	}

	if run.Attempt >= d.cfg.MaxAttempts {
		return d.blockOrphan(ctx, *issue, run)
	}
	return d.requeueOrphan(ctx, *issue, run)
}
```

- [ ] **Step 4: Run the test, verify it passes**

Run: `go test -race ./dispatcher/... -run TestRecoverOrphans -v`
Expected: ALL PASS, including every existing orphan-recovery test (the new branch only fires for `lane == "git_operator"`, which no existing test uses in combination with a high `Attempt`).

- [ ] **Step 5: Full suite**

Run: `make test-go`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add dispatcher/recover.go dispatcher/recover_test.go
git commit -m "fix(dispatcher): exempt the git-operator lane from the orphan attempt cap"
```

---

### Task 6: [Minor] Reset `recover_attempts` on a human requeue from Blocked

**Root cause.** `dispatcher/poll.go`'s `adoptLinearMove` already resets `issues.rework_count` when a human moves a card out of Blocked back into `ready`/`todo` (`ResetReworkCount`), on the reasoning that whatever count it accumulated on its prior cycle "no longer bounds anything relevant" once a human has intervened. The exact same reasoning applies to `issues.recover_attempts` (and its paired `blocked_until` backoff deadline) — auto-unblock layer 1's budget — but `adoptLinearMove` never sets `TransitionReq.ResetRecoverAttempts`. A card that was auto-retried close to `RecoverCap` before eventually parking (e.g. via a `capability`/`needs_input` block, or a `rework_cap` exhaustion) keeps that near-exhausted budget after a human's fresh requeue, so the very next independent transient failure has far less runway than a genuinely fresh issue would.

**Fix.** Set `ResetRecoverAttempts` on the same condition as `ResetReworkCount` — `TransitionReq` and `applyIssueTransition` (`internal/store/outbox.go`) already fully support this field (it also clears `blocked_until` in the same `UPDATE`); this is a call-site-only change.

**Files:**
- Modify: `dispatcher/poll.go`
- Modify: `dispatcher/poll_test.go`
- Modify: `internal/store/outbox.go`, `internal/store/types.go` (doc comments only)

- [ ] **Step 1: Write the failing test**

Append to `dispatcher/poll_test.go` (mirrors `TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsReworkCount`):

```go
// TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsRecoverAttemptsToo asserts
// the fix for finding 5(b): a human requeue out of Blocked resets
// recover_attempts and clears blocked_until, the same way it already resets
// rework_count -- otherwise a card auto-retried close to RecoverCap before
// parking keeps that near-exhausted budget after a human's fresh requeue.
func TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsRecoverAttemptsToo(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	issue := store.Issue{
		ID: "issue-1", Identifier: "CLP-1", LaneLabel: "coder", BoardStatus: "blocked",
		RecoverAttempts: 2, BlockedUntil: 5000,
		Deps: `[]`, Priority: 1, BranchName: "issue-1-branch",
		UpdatedAt: 100, LastSeen: 100, CreatedAt: 100,
	}
	if err := s.UpsertIssue(ctx, issue); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

	lc := &linear.MockClient{
		Issues: []linear.Issue{
			{ID: "issue-1", Identifier: "CLP-1", Status: "ready", Lane: "coder", Priority: 1, BranchName: "issue-1-branch", UpdatedAt: 200},
		},
	}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	d := newTestDispatcher(t, zeroCapConfig(), s, lc, spawner, ws, fixedClock(1000))

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	got, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: unexpected error: %v", err)
	}
	if got.BoardStatus != "ready" {
		t.Fatalf("BoardStatus = %q, want ready (adopted human move)", got.BoardStatus)
	}
	if got.RecoverAttempts != 0 {
		t.Errorf("RecoverAttempts = %d, want reset to 0 on human requeue from blocked", got.RecoverAttempts)
	}
	if got.BlockedUntil != 0 {
		t.Errorf("BlockedUntil = %d, want cleared to 0 on human requeue from blocked", got.BlockedUntil)
	}
}
```

- [ ] **Step 2: Run the test, verify it fails**

Run: `go test ./dispatcher/... -run TestTick_PollAdoptsHumanRequeueFromBlocked_ResetsRecoverAttemptsToo -v`
Expected: FAILS — `RecoverAttempts` stays `2`, `BlockedUntil` stays `5000`.

- [ ] **Step 3: Implement**

In `dispatcher/poll.go`, `adoptLinearMove` becomes:

```go
func (d *Dispatcher) adoptLinearMove(ctx context.Context, issueID, priorStatus, linearStatus string, now int64) error {
	// A human requeuing a card OUT of Blocked means "give this a fresh
	// start": whatever rework_count/recover_attempts/blocked_until it
	// accumulated on its prior review/rework or auto-retry cycle no longer
	// bounds anything relevant to the fresh attempt the human just asked for.
	humanRequeueFromBlocked := priorStatus == string(contract.ColumnBlocked) && isHumanRequeueTarget(linearStatus)
	req := store.TransitionReq{
		IssueID:              issueID,
		NewStatus:            linearStatus,
		ResetReworkCount:     humanRequeueFromBlocked,
		ResetRecoverAttempts: humanRequeueFromBlocked,
		Event: store.Event{
			Ts:      now,
			IssueID: nullString(issueID),
			Kind:    "adopted",
			Detail:  fmt.Sprintf("adopted human move in linear: board_status -> %s", linearStatus),
		},
	}
	if err := d.store.Transition(ctx, req); err != nil {
		return fmt.Errorf("adopting linear move for issue %s: %w", issueID, err)
	}
	return nil
}
```

Update `internal/store/outbox.go`'s `ResetRecoverAttempts` doc comment to match `ResetReworkCount`'s (mention the human-requeue path):

```go
	// ResetRecoverAttempts forces recover_attempts back to 0 AND clears
	// blocked_until to 0 (a clean recovery slate). Set on any normal
	// (non-block) terminal advance (dispatcher.applyTerminalWorkerOutcome), so
	// a later, independent transient failure gets a full retry budget rather
	// than inheriting a spent one -- and also set by
	// dispatcher.adoptLinearMove's blocked->{ready,todo} human-requeue path,
	// for the same reason ResetReworkCount is: a human intervening means the
	// prior cycle's budget shouldn't follow the issue around forever. Takes
	// priority over BumpRecoverAttempts.
	ResetRecoverAttempts bool
```

Update `internal/store/types.go`'s `RecoverAttempts` field doc comment similarly (mirroring `ReworkCount`'s existing comment style):

```go
	// RecoverAttempts is dispatcher-owned runtime state (like ReworkCount and
	// the claim fields): it counts how many times auto-unblock layer 1 has
	// deterministically re-queued this issue after a *transient* failure (a
	// worker block_kind=transient, a run-level crash/malformed/timeout, or a
	// spawn/workspace failure -- see dispatcher.parkOrRetry). Once it reaches
	// cfg.RecoverCap the issue parks in Blocked for good. It resets to 0 the
	// next time the card advances on a normal (non-block) terminal transition
	// (TransitionReq.ResetRecoverAttempts), or once a human requeues it out of
	// Blocked back to ready/todo (dispatcher.adoptLinearMove), mirroring
	// ReworkCount's own reset there. A Linear re-poll (UpsertIssue's conflict
	// path) never touches it.
	RecoverAttempts int
```

- [ ] **Step 4: Run the full poll test file, verify green**

Run: `go test -race ./dispatcher/... -run TestTick_Poll -v`
Expected: ALL PASS, including `TestTick_PollAdoptsHumanMove_FromNonBlocked_DoesNotResetReworkCount` — that test's issue starts at `todo` (not `blocked`), so `humanRequeueFromBlocked` is `false` and neither reset fires, unchanged.

- [ ] **Step 5: Full suite**

Run: `make test-go`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add dispatcher/poll.go dispatcher/poll_test.go internal/store/outbox.go internal/store/types.go
git commit -m "fix(dispatcher): reset recover_attempts on a human requeue from blocked"
```

---

### Task 7: Wire `LaneLabelPrefix` through + collapse `ReadSnapshot`'s per-issue N+1

Both are small, independent, mechanical fixes (5c and 5d from the review) with no shared code path — bundled into one task container per the review's "one task each or batched, your call," but kept as **two separate commits** to honor AGENTS.md's "one concern per commit" rather than force a misleading combined commit message.

#### Part A — 5(c): thread `cfg.LaneLabelPrefix` through instead of hardcoding `"agent:"`

**Root cause.** `internal/config`'s `LaneLabelPrefix` (YAML `lane_label_prefix`, defaults to `"agent:"`) is parsed, defaulted, and readable off `config.Config` — but nothing ever reads it. `internal/linear/status.go`'s `laneFromLabels` hardcodes a package-level `laneLabelPrefix = "agent:"` constant instead, so a deployment that configures a different prefix silently keeps using `"agent:"` for label parsing while the rest of the config accepts and validates the override with no effect.

**Fix.** Thread the prefix as an explicit parameter: `HTTPClient` (constructed once, at `cli/dispatch.go`'s startup, with `cfg.LaneLabelPrefix`) → `NormalizeCandidateIssues` → `normalizeIssueNode` → `laneFromLabels`. `internal/linear` stays free of an `internal/config` import (matching its existing documented convention), since the caller resolves the config and passes a plain string.

**Files:**
- Modify: `internal/linear/status.go`, `internal/linear/normalize.go`, `internal/linear/http_client.go`
- Modify: `internal/linear/normalize_test.go`, `internal/linear/http_client_loopback_test.go`
- Modify: `cli/dispatch.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/linear/normalize_test.go`:

```go
func TestNormalizeCandidateIssues_CustomLabelPrefix(t *testing.T) {
	const raw = `{"data":{"issues":{"nodes":[{"id":"id-1","identifier":"CLI-1","title":"t","description":"","priority":0,"branchName":"b","updatedAt":"2026-07-01T00:00:00.000Z","state":{"name":"Todo"},"labels":{"nodes":[{"name":"clipse:reviewer"}]},"inverseRelations":{"nodes":[]}}]}}}`

	issues, err := linear.NormalizeCandidateIssues([]byte(raw), "clipse:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("len(issues) = %d, want 1", len(issues))
	}
	if issues[0].Lane != string(contract.LaneReviewer) {
		t.Errorf("Lane = %q, want %q (a configured prefix must be honored, not the hardcoded \"agent:\")", issues[0].Lane, contract.LaneReviewer)
	}

	// The SAME label spelling under the default "agent:" prefix must NOT
	// parse when a different prefix is configured -- proves this isn't
	// falling back to a hardcoded default under the hood.
	const rawAgent = `{"data":{"issues":{"nodes":[{"id":"id-2","identifier":"CLI-2","title":"t","description":"","priority":0,"branchName":"b","updatedAt":"2026-07-01T00:00:00.000Z","state":{"name":"Todo"},"labels":{"nodes":[{"name":"agent:coder"}]},"inverseRelations":{"nodes":[]}}]}}}`
	issuesAgent, err := linear.NormalizeCandidateIssues([]byte(rawAgent), "clipse:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: unexpected error: %v", err)
	}
	if issuesAgent[0].Lane != "" {
		t.Errorf("Lane = %q, want empty (an \"agent:\" label must not match a configured \"clipse:\" prefix)", issuesAgent[0].Lane)
	}
}
```

Also update the existing call in the same file — `TestNormalizeCandidateIssues_FromFixture`'s:

```go
	issues, err := linear.NormalizeCandidateIssues(data)
```

to:

```go
	issues, err := linear.NormalizeCandidateIssues(data, "agent:")
```

(This makes the file fail to *compile* until Step 3's signature change lands — an accepted, common form of "red" for a pure parameter-threading change. Confirm this is the only other call site in the file before moving on.)

Update `internal/linear/http_client_loopback_test.go`'s two constructor calls:

```go
	c, err := linear.NewHTTPClientWithBaseURL(srv.URL, testTeamKey, testTeamID, "agent:")
```

and

```go
	_, err := linear.NewHTTPClient(testTeamKey, testTeamID, "agent:")
```

- [ ] **Step 2: Confirm the compile-time red**

Run: `go build ./... 2>&1 | head -20`
Expected: build FAILS — `too many arguments in call to linear.NormalizeCandidateIssues` / `linear.NewHTTPClient` / `linear.NewHTTPClientWithBaseURL`.

- [ ] **Step 3: Implement**

In `internal/linear/status.go`, delete the `laneLabelPrefix` constant and its doc comment, and rewrite `laneFromLabels`:

```go
// laneFromLabels scans Linear label names for a "<labelPrefix><lane>" label
// (e.g. "agent:coder") and returns the bare lane with the prefix stripped.
// labelPrefix comes from config.Config.LaneLabelPrefix, threaded through from
// HTTPClient's construction (cli/dispatch.go) -- this package stays
// dependency-free of internal/config (no import), so it takes the resolved
// string rather than re-deriving config's own default. Returns "" if no such
// label is present; callers must treat that as "no lane assigned" rather
// than an error.
func laneFromLabels(labelNames []string, labelPrefix string) string {
	for _, name := range labelNames {
		if rest, ok := strings.CutPrefix(name, labelPrefix); ok && rest != "" {
			return rest
		}
	}
	return ""
}
```

In `internal/linear/normalize.go`, thread the parameter through:

```go
// NormalizeCandidateIssues parses a candidate-issues GraphQL response body
// and maps it to Clipse's normalized Issue slice: lane labels are stripped
// to their bare lane (via labelPrefix, e.g. "agent:"), workflow-state names
// are mapped to our Column enum, and "blocks"/"blocked-by" relations are
// folded into a single Deps list.
func NormalizeCandidateIssues(body []byte, labelPrefix string) ([]Issue, error) {
	var resp candidateIssuesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("normalizing candidate issues: %w", err)
	}

	nodes := resp.Data.Issues.Nodes
	issues := make([]Issue, 0, len(nodes))
	for _, n := range nodes {
		issue, err := normalizeIssueNode(n, labelPrefix)
		if err != nil {
			return nil, fmt.Errorf("normalizing issue %s: %w", n.Identifier, err)
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// normalizeIssueNode maps a single raw issue node to a normalized Issue.
func normalizeIssueNode(n issueNode, labelPrefix string) (Issue, error) {
	labelNames := make([]string, 0, len(n.Labels.Nodes))
	for _, l := range n.Labels.Nodes {
		labelNames = append(labelNames, l.Name)
	}

	// Deps = the issues that block this one. Only a "blocks" inverse relation
	// is a dependency; "related"/"duplicate"/"similar" links are not and must
	// not gate promotion. r.Issue is the blocker (the source of the blocks
	// relation), which is exactly the issue this one must wait on.
	deps := make([]string, 0, len(n.InverseRelations.Nodes))
	for _, r := range n.InverseRelations.Nodes {
		if r.Type == "blocks" {
			deps = append(deps, r.Issue.ID)
		}
	}

	updatedAt, err := time.Parse(time.RFC3339, n.UpdatedAt)
	if err != nil {
		return Issue{}, fmt.Errorf("parsing updatedAt %q: %w", n.UpdatedAt, err)
	}

	return Issue{
		ID:          n.ID,
		Identifier:  n.Identifier,
		Title:       n.Title,
		Description: n.Description,
		Status:      statusFromWorkflowName(n.State.Name, n.State.Type),
		Lane:        laneFromLabels(labelNames, labelPrefix),
		Deps:        deps,
		Priority:    n.Priority,
		BranchName:  n.BranchName,
		UpdatedAt:   updatedAt.Unix(),
	}, nil
}
```

In `internal/linear/http_client.go`, add a `labelPrefix` field and thread it through the constructors and `CandidateIssues`:

```go
// HTTPClient is the real Client implementation: it talks to Linear's
// GraphQL API over net/http, authenticating with the API key read from
// the LINEAR_API_KEY environment variable, scoped to a single configured
// team.
type HTTPClient struct {
	apiKey      string
	baseURL     string
	teamKey     string
	teamID      string
	labelPrefix string
	httpClient  *http.Client

	// mu guards stateIDs, the lazily-resolved and cached name(lowercase)->id
	// map for teamID (see state_resolver.go). The dispatch loop is
	// single-goroutine (AGENTS.md), so this is defense in depth rather than
	// a load-bearing requirement.
	mu       sync.Mutex
	stateIDs map[string]string
}

// NewHTTPClient builds an HTTPClient using the API key from LINEAR_API_KEY,
// pointed at Linear's real GraphQL endpoint and scoped to the Linear team
// identified by teamKey (candidate-issues filter) and teamID (workflow-state
// resolution for SetState). labelPrefix is config.Config.LaneLabelPrefix,
// threaded through for Linear label parsing (see status.go's laneFromLabels).
// Returns an error if the environment variable is unset or empty.
func NewHTTPClient(teamKey, teamID, labelPrefix string) (*HTTPClient, error) {
	return newHTTPClient(apiURL, teamKey, teamID, labelPrefix)
}

// NewHTTPClientWithBaseURL builds an HTTPClient like NewHTTPClient, but
// against baseURL instead of Linear's real API. Intended for tests that
// point the client at a local httptest.Server; production code should use
// NewHTTPClient.
func NewHTTPClientWithBaseURL(baseURL, teamKey, teamID, labelPrefix string) (*HTTPClient, error) {
	return newHTTPClient(baseURL, teamKey, teamID, labelPrefix)
}

func newHTTPClient(baseURL, teamKey, teamID, labelPrefix string) (*HTTPClient, error) {
	apiKey := os.Getenv(apiKeyEnvVar)
	if apiKey == "" {
		return nil, fmt.Errorf("building linear http client: %s is not set", apiKeyEnvVar)
	}
	return &HTTPClient{
		apiKey:      apiKey,
		baseURL:     baseURL,
		teamKey:     teamKey,
		teamID:      teamID,
		labelPrefix: labelPrefix,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// CandidateIssues runs CandidateIssuesQuery, scoped to c's configured team,
// and normalizes the response.
func (c *HTTPClient) CandidateIssues(ctx context.Context) ([]Issue, error) {
	reqBody, err := BuildCandidateIssuesRequest(c.teamKey)
	if err != nil {
		return nil, fmt.Errorf("candidate issues: %w", err)
	}

	respBody, err := c.do(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("candidate issues: %w", err)
	}

	issues, err := NormalizeCandidateIssues(respBody, c.labelPrefix)
	if err != nil {
		return nil, fmt.Errorf("candidate issues: %w", err)
	}
	return issues, nil
}
```

In `cli/dispatch.go`, update the construction call:

```go
	lc, err := linear.NewHTTPClient(cfg.TeamKey, cfg.TeamID, cfg.LaneLabelPrefix)
```

- [ ] **Step 4: Run the linear package tests, verify green**

Run: `go test ./internal/linear/... -v`
Expected: ALL PASS.

- [ ] **Step 5: Full build + suite**

Run: `go build ./... && make test-go`
Expected: PASS (this also confirms `cli/dispatch.go` compiles against the new signature).

- [ ] **Step 6: Commit**

```bash
git add internal/linear/status.go internal/linear/normalize.go internal/linear/http_client.go \
        internal/linear/normalize_test.go internal/linear/http_client_loopback_test.go \
        cli/dispatch.go
git commit -m "feat(linear): thread lane_label_prefix through from config"
```

#### Part B — 5(d): collapse `ReadSnapshot`'s per-issue N+1

**Root cause.** `internal/store/crud.go`'s `ReadSnapshot` calls `s.latestRun(ctx, issue.ID)` in a loop, once per issue — an N+1 query pattern — and then, separately, calls `s.runsByIssue(ctx)` once, which already loads **every** run for **every** issue in a single query, ordered `ORDER BY issue_id, started_at, run_id` ascending. Within one issue's group, the last element of that ordered slice has the maximum `(started_at, run_id)` pair — exactly what `latestRun`'s own query (`ORDER BY started_at DESC, run_id DESC LIMIT 1`) computes. The N per-issue queries are redundant with data `ReadSnapshot` was already about to load anyway.

**Fix.** Derive `LatestRun` from `runsByIssue`'s already-loaded result instead of a separate query per issue. Pure internal refactor — observable behavior is identical, and is already fully covered by `internal/store/crud_test.go`'s `TestReadSnapshot_IssueRuns` (multiple issues, multiple runs each, inserted out of chronological order to prove ordering comes from `started_at` not insertion order, asserting both `Runs` and `LatestRun` together, plus a run-less issue). Per this plan's TDD constraint: there is no new "red" phase here because there is no new observable behavior — this step is proven by keeping the existing suite green throughout, not by writing a new failing test.

**Files:**
- Modify: `internal/store/crud.go`

- [ ] **Step 1: Confirm the regression net is already green**

Run: `go test ./internal/store/... -run TestReadSnapshot -v`
Expected: ALL PASS (baseline, before touching anything).

- [ ] **Step 2: Implement**

In `internal/store/crud.go`'s `ReadSnapshot`, replace:

```go
	for i := range snap.Issues {
		latest, err := s.latestRun(ctx, snap.Issues[i].ID)
		if err != nil {
			return Snapshot{}, err
		}
		snap.Issues[i].LatestRun = latest
	}

	tokensIn, tokensOut, err := s.tokenTotalsByIssue(ctx)
```

with:

```go
	// A single runsByIssue query loads every run for every issue, already
	// ordered oldest-first per issue (ORDER BY issue_id, started_at, run_id).
	// LatestRun is just that per-issue slice's last element, so deriving it
	// here replaces what used to be a separate latestRun(ctx, id) query PER
	// ISSUE -- an N+1 on top of data this function was already loading -- with
	// zero extra queries.
	runs, err := s.runsByIssue(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	for i := range snap.Issues {
		issueRuns := runs[snap.Issues[i].ID]
		snap.Issues[i].Runs = issueRuns
		if n := len(issueRuns); n > 0 {
			latest := issueRuns[n-1]
			snap.Issues[i].LatestRun = &latest
		}
	}

	tokensIn, tokensOut, err := s.tokenTotalsByIssue(ctx)
```

and delete the now-duplicate later block:

```go
	runs, err := s.runsByIssue(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	for i := range snap.Issues {
		snap.Issues[i].Runs = runs[snap.Issues[i].ID]
	}
```

(This second block sat right before the `recentEvents` call, after `unmirroredIssueIDs`; remove it entirely — both `Runs` and `LatestRun` are now set together, earlier, from the single `runsByIssue` call above.)

Finally, delete the now-dead `latestRun` method entirely (confirmed zero remaining callers):

```go
// latestRun returns the most recently started run for issueID, or nil if
// none exists.
func (s *Store) latestRun(ctx context.Context, issueID string) (*Run, error) {
	const q = `
		SELECT run_id, issue_id, lane, worker_pid, proc_started_at, status, started_at, heartbeat_at,
			attempt, turn_count, thread_id, result_json, error, tokens_in, tokens_out
		FROM runs
		WHERE issue_id = ?
		ORDER BY started_at DESC, run_id DESC
		LIMIT 1
	`
	var r Run
	err := s.db.QueryRowContext(ctx, q, issueID).Scan(
		&r.RunID, &r.IssueID, &r.Lane, &r.WorkerPID, &r.ProcStartedAt, &r.Status, &r.StartedAt, &r.HeartbeatAt,
		&r.Attempt, &r.TurnCount, &r.ThreadID, &r.ResultJSON, &r.Error, &r.TokensIn, &r.TokensOut,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading latest run for issue %s: %w", issueID, err)
	}
	return &r, nil
}
```

- [ ] **Step 3: Run the store package tests, verify green**

Run: `go test -race ./internal/store/... -v`
Expected: ALL PASS, unchanged — in particular `TestReadSnapshot_IssueRuns` (both `Runs` order and `LatestRun == r3` for issue-1, empty for run-less issue-2), `TestSetRunProcess_RoundTrip` (`LatestRun` still carries `WorkerPID`/`ProcStartedAt`), and `TestReadSnapshot_CumulativeTokensAcrossRuns` (unaffected — different code path).

Run: `go vet ./...`
Expected: clean (confirms `latestRun` has no other references left).

- [ ] **Step 4: Full suite**

Run: `make test-go`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/crud.go
git commit -m "perf(store): drop readsnapshot's per-issue latestrun n+1 query"
```

---

## Final verification

- [ ] Run: `make lint`
  Expected: `go vet` clean, `gofmt -l .` empty, `ruff check` clean on `agent/` (untouched by this plan, should already be clean).
- [ ] Run: `make test`
  Expected: PASS (`test-go` now races, `test-py` untouched and green).
- [ ] Run: `make build`
  Expected: builds `./bin/clipse` cleanly (confirms `cli/dispatch.go`'s call-site change compiles in the full binary, not just `go build ./...`).
- [ ] Run: `git log --oneline fix/kernel-hardening ^main`
  Expected: 9 commits (Task 0 through Task 7, with Task 7 contributing two) — matches the 7 numbered tasks plus the Task 0 setup commit and Task 7's extra commit.

---

## Self-Review

**Root cause vs. symptom.** All five findings are fixed at the point where the wrong invariant is actually violated, not papered over downstream:
- Finding 1: fixed where the stale map entry is created (the failure branches of `spawnAttempt`), not by making `reconcile`'s heartbeat loop tolerate a missing claim (which would silently mask a real accounting bug elsewhere).
- Finding 2: fixed by strengthening the *decision* of whether to trust Linear's observed state (`reconcileLinearDivergence`), not by adding a special case somewhere downstream that tries to recover from an already-written ghost `running` row.
- Finding 3: fixed by supplying the missing *producer* (poll + normalize), leaving the already-correct *consumers* (`promote.go`, `recover.go`) untouched — the review's own phrasing ("dead code") pointed straight at this.
- Finding 4: fixed by not losing data at the point of failure (`applyResult`), rather than adding a periodic sweep to find and re-heartbeat/re-park abandoned inflight runs after the fact.
- Finding 5(a): fixed by recognizing that "attempt" means two different things for two different lanes, and stopping the orphan-recovery check from conflating them — not by inflating `MaxAttempts` globally (which would just mask a *real* coder/reviewer failure loop for longer) or by lengthening `mergingTTL` (which would slow down every CI-pending recheck, including healthy fast ones, to paper over a restart-timing edge case).

**TDD discipline.** Every finding with an observable behavior change (1, 2, 3's linear-layer half, 4, 5a, 6, 7a) has a test that fails against `f801d46` for the reason the finding describes, verified in this plan's own "run and confirm it fails" steps before the fix lands. Two tasks are explicit, documented exceptions rather than forced red phases: Task 3's dispatcher-level regression test (the consumer logic was already correct; only the producer was missing) and Task 7 Part B (a pure internal refactor with identical observable behavior, proven by keeping existing coverage green rather than inventing an artificial assertion).

**Testing-technique note.** Tasks 1 and 4 both needed to force a failure at a point `Dispatcher.store *store.Store` (a concrete type, not an interface) offers no seam for. Rather than introduce a store interface — a much larger, riskier change than "kernel hardening" warrants, and one that would ripple through every `dispatcher/*.go` file's dozens of `d.store.X(...)` call sites — both tests use `store.DB()` (already an established test-only escape hatch, per `dispatcher/recover_test.go`'s existing `UPDATE ... WHERE run_id = ?` adversarial-state setup) to reach real SQL: `fakeSpawner.FailOnCall` for a one-shot spawn failure (Task 1), and a raw `DELETE FROM issues` + restore-by-`UpsertIssue` round-trip to simulate a transient store read failure (Task 4). Both are real, production-code-triggerable failure modes exercised through the real `*store.Store`, not a mocked stand-in for one.

**Blast radius.** No task changes `agent/` (Python), `schema/*.schema.json`, or generated code. Every diff is additive-or-corrective inside an existing function; no new production types, no new packages, no new dependencies. The two largest diffs (Task 3's query/normalize threading and Task 7 Part A's `labelPrefix` threading) are both mechanical parameter-passing changes with zero behavior change for the existing default (`"agent:"`, no `state.type` present in old fixtures).

**Deliberately out of scope / open questions:**
1. **Finding 3, concurrently-claimed-and-cancelled edge case.** If a human cancels an issue in Linear while the dispatcher currently holds an active claim on it, `reconcileLinearDivergence` still takes the `reassertOwnedState` branch (claim is valid) and pushes the dispatcher's own status back to Linear — the cancellation is effectively ignored until the claim naturally resolves to a terminal state on its own. This is consistent with the *existing*, pre-this-plan policy for every other kind of concurrent human move while claimed (not a gap this plan introduces), but it means a cancel-while-running doesn't take effect immediately. Worth a product decision (should a claimed run be interrupted on cancellation?) but out of scope for a kernel-hardening pass whose brief is fixing reconciliation bugs, not changing claim semantics.
2. **Finding 5(a)'s chosen bound.** Exempting `git_operator` from `MaxAttempts` entirely (rather than, say, giving it a much larger or CI-aware cap) relies on `applyGitopsResult`'s `OutcomeNotMergeable` → `parkOrRetry` → `cfg.RecoverCap` path as the *real* bound on genuine git-operator failures. I traced this and confirmed `issues.recover_attempts` is a store column, untouched by a dispatcher restart, so that bound holds even across repeated restarts — but this plan does not add a *test* proving the cross-restart persistence of that specific counter (it's exercised indirectly by existing `parkOrRetry`/`RecoverCap` tests, just not in combination with a restart). If that's a concern, it's a one-task follow-up: seed a high `recover_attempts` on a merging-column issue, restart via `RecoverOrphans`, and confirm a subsequent `OutcomeNotMergeable` still parks it.
3. **TUI/`clipse status` rendering of `"cancelled"`.** Confirmed non-crashing (`cli/tui/model.go`'s `switch is.BoardStatus` has no default panic) but a cancelled card doesn't get its own bucket/visual treatment — it just doesn't appear in `running`/`blocked`/`queued`/etc. Cosmetic, deliberately left out of a kernel-hardening pass.
4. **Finding 3's fixture choice.** The new cancelled-type normalize test uses an inline JSON literal rather than extending the shared `internal/linear/testdata/candidate_issues.json`, specifically to avoid this task's diff rippling into Task 7's `http_client_loopback_test.go` assertions (`len(issues) != 4`). If a future change wants a single canonical fixture covering cancellation too, that's a deliberate, separate cleanup — not bundled here to keep each task's diff self-contained.

**Constraint compliance:** no `agent/` changes; no `schema/*.schema.json` changes (Task 3's design note justifies why one wasn't needed); `go test -race ./...` is the standing gate from Task 0 onward; every new test is table-driven-idiom stdlib `testing` (no testify); every new/touched error path wraps with `%w`; no `log/slog`/`fmt.Println` changes needed (no new runtime log lines were warranted by these fixes beyond what's already logged at the existing park/retry/requeue call sites).
