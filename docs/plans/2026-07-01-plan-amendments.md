# Clipse Plan Amendments — Path to 95

**Status:** Proposed · **Date:** 2026-07-01 · **Source:** external review of
[implementation plan](2026-07-01-clipse-implementation-plan.md),
[design doc](../design/2026-07-01-clipse-design.md), and the architecture canvas.

The review graded the plan per phase. This doc lists every change needed to
raise the weighted score from ~84 to ≥95. Each amendment states the problem,
the fix, and the exact edits (new work items, acceptance criteria, design-doc
sections). Per the implementer notes: apply design-doc edits first, then plan
edits, then code.

| Phase | Current | After amendments | Blocking amendments |
|---|---|---|---|
| 0 — scaffolding & contract | 92 | 96 | X1, X2 |
| 1 — Go kernel | 88 | 95 | A1, A2, A3 |
| 2 — Coder worker | 81 | 93 | B1, B2, B3, B4 |
| 3 — full pipeline | 76 | 94 | C1, C2, C3 |
| 4 — v2 deferred | 70 | 85 | D1 |
| **Weighted overall** | **~84** | **≥95** | |

---

## X. Cross-cutting doc fixes (Phase 0 → 96)

### X1 — Sync the Go version across docs and toolchain

**Problem:** The plan and design doc say Go 1.23+; `go.mod` and CI are on a
1.25.x floor (forced by `modernc.org/sqlite`), and the toolchain is 1.26. Three
different answers to "what Go version?".

**Change:** Make `go.mod` the single source of truth. Edit the plan's Tech
Stack line and the design doc to say "Go (version per `go.mod`)" and record the
1.25 module floor in a one-line note.

### X2 — Remove the nullable-enum wart from `worker-result.schema.json`

**Problem:** `block_kind` models "absent" as an enum member (`null`). Codegen
tools disagree on how to render an enum containing `null`; the Go and Pydantic
sides can drift on exactly the field that routes failures.

**Change:** Make `block_kind` an optional field with a string-only enum
(`needs_input | capability | transient`), absent when `outcome != "blocked"`.
Add a schema-level constraint (or a documented invariant plus tests on both
sides): `block_kind` present **iff** `outcome == "blocked"`.

**New Phase 0 work item:**
- [ ] Rework `block_kind` to an optional string enum; regenerate both sides;
      add a Go test and a Pydantic test asserting round-trip of blocked and
      non-blocked results.

---

## A. Phase 1 amendments — Go kernel (88 → 95)

### A1 — Orphaned workers after a dispatcher restart (double-run hole)

**Problem:** The local Spawner reads results from the worker's stdout and
`Wait()`s on it as a child process. If the dispatcher dies, live workers become
orphans: the pipe is gone, the result is unrecoverable, and `Wait()` is
impossible from the new process. Worse, `ReleaseStaleClaims` will later requeue
the issue while the orphan is still running and pushing — the exact double-run
the CAS claim exists to prevent.

**Change:** On dispatcher startup, before any claim release, kill every process
recorded in an open `runs` row, then close those runs and requeue their issues.

- **PID-reuse guard:** a PID alone can name an unrelated process after reboot.
  Record the process start time (or process group id) in `runs` at spawn;
  before killing, verify the live process matches. On mismatch, treat the run
  as already dead.
- **Requeue, don't Block:** decision H ("failures park in Blocked") governs
  *worker* failures. A dispatcher restart is infrastructure, not a worker
  failure — the safe default is requeue to `ready` with an incremented
  `attempt`, bounded by a small `max_attempts` (exceed → `Blocked`). The
  worktree persists and PR creation is idempotent, so a re-run is safe.
  Alternative (stricter H reading): park in `Blocked` with reason
  `orphaned by dispatcher restart`. Recommendation: requeue with attempt cap;
  record the choice in the decision log either way.

**New Phase 1 work items:**
- [ ] **spawn**: record `proc_started_at` (and/or pgid) in `runs` at spawn.
- [ ] **startup recovery** (`dispatcher`): on start, for each open run —
      verify PID identity, kill if alive, close run with
      `error='orphaned'`, requeue issue per the attempt cap. Test with a real
      spawned sleeper process: start "dispatcher recovery" against its
      recorded run row; assert the process dies and the issue returns to
      `ready`.
- [ ] **attempt cap**: `max_attempts` in config; exceeding it lands `Blocked`
      with a comment. Test the boundary.

**New acceptance criterion:**
- [ ] **Restart safety**: kill the dispatcher mid-run with a live worker;
      restart it; the orphan is killed before any claim release, the issue is
      requeued exactly once, and no duplicate PR or branch results.

### A2 — Linear write failures leave board and store divergent

**Problem:** The tick commits a transition to SQLite, then mirrors it to
Linear. If the Linear API call fails, SQLite and the board diverge and nothing
in the plan reconciles them. (Commit `677bdd8` protects `board_status` on
upsert, but no retry exists for the outbound write.)

**Change:** Treat Linear mirroring as an at-least-once outbox. Add a
`linear_writes` table (or a `pending_linear_write` column on `issues`):
transition commit and pending-write enqueue happen in one SQLite transaction;
each tick drains pending writes with retry. `SetState` is idempotent (setting
a card to a state it already holds is a no-op), so replays are safe.

**New Phase 1 work items:**
- [ ] **linear outbox** (`store` + `dispatcher`): enqueue the mirror write in
      the same transaction as the transition; drain queue each tick; retry on
      failure with the error logged. Test: mock Linear fails twice then
      succeeds — exactly one final `SetState`, no lost transition.
- [ ] **divergence rule** (design doc): for dispatcher-owned columns, SQLite
      wins and the outbox re-asserts; human moves are adopted per A3.

**New acceptance criterion:**
- [ ] With Linear down for N ticks, transitions keep committing locally and
      mirror correctly once Linear recovers; `clipse status` flags unmirrored
      issues.

### A3 — Human requeue contradicts "dispatcher is the only writer"

**Problem:** The design doc says the dispatcher is the only writer of board
state, and also says a human requeues `Blocked → Ready`. Both are true, but
the plan never says how the kernel tells an adopted human move from drift it
should re-assert.

**Change:** State the rule once in the design doc: *the dispatcher is the only
**automated** writer; humans may move cards in Linear, and the poll adopts
their moves.* Concretely: on poll, if Linear disagrees with SQLite and the
issue holds no active claim, adopt Linear's state (human move). If the issue
holds an active claim, SQLite wins and the outbox re-asserts (drift).

**New Phase 1 work items:**
- [ ] **poll adoption** (`dispatcher`): implement the adopt-vs-reassert rule
      above. Tests: (a) card manually moved `blocked → ready` gets adopted and
      claimed next tick; (b) card manually moved while a claim is active gets
      re-asserted to the SQLite state.

---

## B. Phase 2 amendments — Coder worker (81 → 93)

### B1 — Checkpointer lifecycle is unspecified

**Problem:** The plan wires `AsyncSqliteSaver` keyed by `thread_id` but never
says where its database lives or when it dies. A shared file leaks graph state
across issues; an unowned file survives issue completion forever.

**Change:** One checkpointer database per issue at
`<board>/checkpoints/<issue_id>.db` (outside the worktree, so the working tree
stays clean and no gitignore entry is needed). The existing
worktree-removal-on-terminal helper also deletes the checkpoint file.

**New Phase 2 work items:**
- [ ] **checkpointer path**: worker derives the path from `--workspace`
      conventions or an explicit `--checkpoint-db` arg (prefer explicit — the
      kernel owns paths). Test: two issues run concurrently, distinct files,
      no cross-talk.
- [ ] **cleanup**: terminal-state cleanup removes the checkpoint file with the
      worktree. Test: after `done`, neither worktree nor checkpoint remains.

### B2 — No per-run token ceiling until Phase 4

**Problem:** The turn cap bounds *turns*, not tokens within a turn. One
runaway DAC turn spends without bound. Deferring all budget work to Phase 4
leaves v1's real cost risk uncovered.

**Change:** Pull one narrow item forward from Phase 4: a worker-side
`max_tokens_per_run` (config, passed via env/arg). The worker tracks usage
from DAC callbacks and aborts over budget with
`outcome=blocked, block_kind=capability` and a summary naming the spend.
Kernel-side accounting stays in Phase 4.

**New Phase 2 work item:**
- [ ] **token ceiling**: enforce `max_tokens_per_run` in the worker; emit
      `blocked(capability)` on breach. Test with a mocked DAC token stream
      crossing the limit.

### B3 — Prompt injection via issue text is unacknowledged

**Problem:** Linear issue bodies feed a shell-enabled agent running
`auto_approve=True`. A crafted issue steers the agent into any allow-listed
command. For a personal tool on a private board the residual risk is
acceptable — but the design doc should say so, or the omission reads as an
oversight.

**Change:** Add a short **Threat model** section to the design doc:

- Linear issue content is untrusted input to the worker.
- Mitigations: per-lane shell allow-lists; worker env carries only the secrets
  that lane needs (`ANTHROPIC_API_KEY`, scoped `gh` token — never
  `LINEAR_API_KEY`, which is kernel-only); merge is gated by CI + branch
  protection, so injected code cannot land without passing required checks;
  the board is private and single-tenant.
- Non-goals v1: full sandboxing (containers/VMs) — revisit in Phase 4 if the
  board ever accepts external input.

**New Phase 2 work item:**
- [ ] **env scrubbing** (`spawn`): the Spawner passes an explicit env
      allow-list per lane, not the dispatcher's environment. Test: worker env
      contains only the allow-listed keys.

### B4 — External prerequisites are known but not written down

**Problem:** Phase 2 silently depends on a real Linear board, custom columns,
`agent:<lane>` labels, `ANTHROPIC_API_KEY`, `gh` auth, and a target repo with
branch protection. The review and the memory file know this; the plan doc
doesn't.

**Change:** Add a **Prerequisites** checklist at the top of Phase 2:

- [ ] Linear board with columns `Rework`, `Merging`, `Documentation` and
      labels `agent:coder|reviewer|git_operator|scribe`.
- [ ] Candidate-issue GraphQL query and branch-name auto-link verified against
      that board.
- [ ] `ANTHROPIC_API_KEY` available via `op run`; `gh` authenticated.
- [ ] Target (throwaway) repo with required checks + branch protection
      configured.
- [ ] DAC API spike findings recorded in the design doc "to verify" section.

---

## C. Phase 3 amendments — full pipeline (76 → 94)

### C1 — The review ↔ rework loop is unbounded

**Problem:** The turn cap bounds continuation *within* a run. Nothing bounds
`review → rework → review` cycles. A coder/reviewer disagreement ping-pongs
forever, burning tokens with no terminal state.

**Change:** Add `rework_cap` (config, small — default 3). The kernel counts
review↔rework cycles per issue (`issues.rework_count`, reset on `done`);
exceeding the cap lands `Blocked` with a comment linking the PR and the last
review. Mirrors the turn cap exactly: every loop in the system gets a bound.

**New Phase 3 work items:**
- [ ] **rework cap** (`store` + `board` + `dispatcher`): count cycles, block
      on breach. Table-driven test over the boundary (cap, cap+1).

**New acceptance criterion:**
- [ ] A permanently-disagreeing reviewer (mocked) drives the issue to
      `Blocked` after exactly `rework_cap` cycles — never an infinite loop.

### C2 — Git-operator puts an LLM where determinism matters most

**Problem:** Merging is `gh pr checks` → `gh pr merge` → tag → cleanup: pure
deterministic CLI work with exact success criteria. Running it through a DAC
agent adds cost, latency, and nondeterminism at the single most dangerous
transition in the pipeline. The guardrails (allow-list, CI gate) contain the
blast radius but don't justify the indirection.

**Change:** Implement merge/tag/cleanup as deterministic Go in the kernel
(`internal/gitops`), invoked by the dispatcher when a card enters `merging`.
The `git_operator` lane label remains as board semantics; its *executor* is
kernel code, not a worker. The Python `graphs/git_operator.py` items in Phase
3 are replaced by:

**Replacement Phase 3 work items:**
- [ ] **gitops** (`internal/gitops`): check required CI checks + branch
      protection (`gh pr checks`, protection API) → merge → optional tag →
      remove worktree + local branch. Test against a fake `gh` PATH shim:
      mergeable, failing-checks, absent-checks, protection-unsatisfied.
- [ ] **board wiring**: `merging` cards route to gitops instead of a spawned
      worker; outcomes map exactly as the lane's results did
      (merged → `documentation`; not mergeable → `rework`/`blocked`).
- [ ] **decision log**: amend J — "Git-operator lane executes as deterministic
      kernel code; the lane label is board semantics only." (If the DAC lane
      is deliberately kept instead, record *why* in the decision log; the
      uniformity argument must be stated, not implied.)

This also deletes work: `profiles/git_operator.py` and its prompt/guardrail
items disappear.

### C3 — Base-branch drift after each merge

**Problem:** Merge A lands; PR B in `review`/`merging` is now behind, and
"up-to-date branch" protection will refuse it. The design doc mentions
rebase-onto-main under the Git-operator's duties, but no plan work item
implements it, so the second merge of any batch fails with no route.

**Change:** When gitops finds a PR blocked only by a stale base, update it
(`gh pr update-branch`, or rebase-and-push) and re-check; on conflict, route
to `Rework` with a comment naming the conflicting files.

**New Phase 3 work items:**
- [ ] **stale-base handling** (`internal/gitops`): update-branch on stale
      base; conflict → `rework` with comment. Test both paths with the `gh`
      shim.

**New acceptance criterion:**
- [ ] Two issues merge back-to-back: the second PR is auto-updated after the
      first merge and lands without human help.

---

## D. Phase 4 amendments — deferred scope (70 → 85)

### D1 — Give each deferred item a scope line and a graduation trigger

**Problem:** Phase 4 items are one-liners. Fine for a parking lot, but nothing
says *when* an item earns promotion into active work, so the phase can't be
graded higher than its vaguest entry.

**Change:** For each item add one sentence of scope, one named risk, and a
**graduation trigger** — the observable condition that promotes it:

| Item | Graduation trigger (example) |
|---|---|
| Orchestrator / auto-decompose | manually decomposing issues costs more than one hour/week |
| Multi-repo | a second repo actually needs lanes |
| Remote Spawner | local caps saturate the machine (sustained cap-bound queue) |
| Web dashboard / OTel export | TUI insufficient for >1 observer, or debugging needs history Datadog would hold |
| Token budgets (kernel-side) | monthly spend exceeds a set dollar threshold |

Note that B2 moves the worker-side token ceiling *out* of Phase 4 into Phase 2;
Phase 4 keeps kernel-side accounting and per-lane budgets.

---

## E. Canvas touch-ups (no score weight — hygiene)

- Move the `Rework` node out of the **Exceptions** group; it is a normal
  pipeline state with a defined loop, unlike `Blocked`.
- The DAC config node pins `model="anthropic:claude-sonnet-4-6"` — the only
  place any doc names a model. Add "example — model set per lane profile" so
  the canvas doesn't read as a decision the docs never made.
- After C2 lands, relabel the Git-operator agent card to note it executes as
  kernel code.

---

## Ordering

1. **X1, X2** — doc/schema hygiene; do with the current Phase 0 tail.
2. **A1–A3** — fold into Phase 1 before it is declared green; A1 is the only
   correctness hole in the kernel.
3. **B1–B4** — fold into Phase 2 alongside the DAC spike.
4. **C1–C3** — fold into Phase 3; C2 first (it deletes work items).
5. **D1, E** — any time; no code.
