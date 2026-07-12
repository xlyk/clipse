# Linear board bootstrap — design

Date: 2026-07-11
Status: approved (brainstorm), pending spec review
Topic: a deterministic tool + authoring skill to turn a project plan into a dispatch-ready Linear board.

## Problem

Starting clipse on a new project requires manual Linear board prep: create the issues, wire their `blocked-by` dependency DAG, apply `agent:<lane>` labels, and set each to a starting workflow state. Today this exists only as `scripts/smoke/smoke.sh`'s `seed()` — a throwaway seeder that shells out to the external `linear` CLI, screen-scrapes issue IDs out of stdout (`grep -oE 'CLI-[0-9]+'`), and is keyed on a `[smoke]` title prefix so it can nuke-and-repave. That model is fine for a disposable smoke board but wrong for a real one: a live board can't be wiped, re-runs must be idempotent, and the `linear` CLI + stdout scraping are fragile dependencies clipse doesn't otherwise need.

We have run several smoke and demo cycles successfully. It is time to promote board prep from a smoke side-effect to a first-class, repeatable operation.

## Goal

Productize the "reflex flow" (a prose project plan became a Linear board) into two artifacts:

1. A **deterministic Go applier** — `clipse board plan|apply` — that consumes a structured board spec and reconciles it onto a Linear team idempotently.
2. A **new clipse-specific authoring skill** that turns a prose project plan into a schema-valid board spec for a human to review before applying.

Non-goals (this spec): managing clipse's own SPA/M3-M4 dev board; deleting/pruning issues; milestone/project structure (deferred, see Phasing); resuming in-flight dispatch.

## Pipeline

```
prose project plan
   │  (authoring skill — LLM, outside Go)
   ▼
board.yaml + body_file markdowns   ← git-committed, human-reviewed
   │  (clipse board apply — deterministic Go)
   ▼
Linear board: issues + blocked-by DAG + agent labels + initial state
```

Two stages, one clean seam. The LLM half lives entirely in the skill; the Go half never sees prose. Kernel LLM-free invariant intact.

## Kernel-invariant guardrails

- **The dispatcher must never gain the ability to create or delete Linear issues.** The board mutations (`issueCreate`, `issueRelationCreate`, label ensure) live in a new package, not on `internal/linear.HTTPClient` (the dispatcher's client, which stays limited to `CandidateIssues` / `SetState` / `Comment` / `IssueComments`). Least-privilege preserved.
- **`clipse board` is an operator command, not the daemon.** It reads `LINEAR_API_KEY` directly (same as `clipse dispatch`), runs to completion, and exits. It is never invoked by a worker, so the `env_allowlist` prohibition on `LINEAR_API_KEY` (workers only) is unaffected.
- **Kernel stays LLM-free and deterministic.** The applier is pure I/O + diffing; no model call anywhere.

## Component 1 — the board spec (`board.yaml`)

The contract between the skill (producer) and the applier (consumer). YAML, parsed by the existing `gopkg.in/yaml.v3` dependency into a hand-written, `yaml`-tagged Go struct. A `schema/board-spec.schema.json` ships as the **authoring reference** the skill targets and as documentation — it is *not* codegen'd and *not* loaded at runtime (clipse has no runtime JSON-Schema validator dependency, and codegen emits json-tagged types that do not cleanly unmarshal YAML). Validation is hand-written Go (see below). A test asserts the bundled `board.example.yaml` parses and passes validation, guarding struct/schema drift.

### Shape

```yaml
team: CLI                     # required — Linear team key
base_branch: main             # optional — informational; clipse.yaml owns the real value
default_labels: [agent:coder] # optional — applied to every issue lacking explicit labels
issues:
  - ref: core-1               # required — stable, tracker-agnostic idempotency key (unique)
    title: Greeter core       # required
    milestone: core           # optional — reserved, ignored in v1 (see Phasing)
    labels: [agent:coder]     # optional — overrides default_labels for this issue
    deps: []                  # optional — list of refs this issue is blocked-by
    human: false              # optional — human ticket: labeled human, left in Todo, no agent label
    body: |                   # body: inline...
      Short description.
  - ref: cfg-1
    title: Config dataclass
    deps: [core-1]
    body_file: specs/cfg-1.md # ...or a pointer to a markdown file (relative to the spec)
```

- `ref` is the immutable requirement id — the tracker-agnostic identity that survives across runs and decouples from Linear's `CLI-N`.
- `body` xor `body_file` (exactly one). `body_file` paths resolve relative to the spec file's directory.
- `human: true` → issue gets a `human` label, stays in the start state, and receives **no** `agent:*` label, so the dispatcher's lane-label opt-in gate skips it.

### Validation (hand-written Go, before any network call)

- `team` and every `ref`/`title` non-empty.
- `ref` unique across issues.
- Every entry in `deps` refers to a defined `ref`.
- The `deps` graph is acyclic (topological sort; report the cycle).
- Exactly one of `body`/`body_file` per issue; every `body_file` exists and is readable.
- Every label is non-empty.

## Component 2 — the applier (`clipse board plan|apply`)

A new cobra subcommand group under `clipse`, backed by a new package (working name `internal/boardspec` for parse+validate and `internal/linear/bootstrap` for the Linear mutations — final split decided in the plan). Shared GraphQL transport (auth header, POST, error parsing, request marshalling) is extracted from `HTTPClient` into a small internal transport reused by both clients, so no transport code is duplicated and the dispatcher client's surface does not widen.

### The marker — stateless idempotency

Each issue the applier creates carries a hidden trailer appended to its Linear description:

```
<!-- clipse-ref: core-1 sha:ab12cd34 -->
```

- `clipse-ref` = the spec `ref`. This is how a re-run recognizes "spec `core-1` == this Linear issue" with no local state file.
- `sha` = short hash over the issue's rendered content (title + body + sorted labels + sorted deps). Lets a re-run detect that a spec entry changed without re-diffing the whole body.

Linear is the single source of truth; nothing local can drift.

### `plan` (read-only)

1. Parse + validate the spec.
2. Query the team's issues (reuse/extend the candidate query, but unfiltered by label — we need to see all clipse-managed issues). Parse `clipse-ref` + `sha` markers from descriptions.
3. Diff:
   - spec `ref` with no matching marker → **create**
   - marker present, `sha` differs → **update**
   - marker present, `sha` matches → **skip**
   - `blocked-by` relation in spec but absent on board → **relation** (add)
   - marker on board whose `ref` is absent from the spec → **orphan** (report only; never deleted)
4. Print the plan table and a summary line. Mutate nothing.

```
$ clipse board plan board.yaml
  + create   core-1  Greeter core
  + create   cfg-1   Config dataclass   (blocked-by core-1)
  ~ update   cli-1   CLI parser         (body changed)
  = skip     rel-1   Release 0.2.0      (unchanged)
  + relation cfg-1 blocked-by core-1
  ! orphan   old-7   Deprecated task    (on board, not in spec — left alone)

plan: 2 create, 1 update, 1 skip, 1 relation, 1 orphan
run with --apply to execute
```

### `apply`

Executes the plan (or `plan --apply`). Order:

1. Ensure required labels exist on the team (create missing `agent:*` / `human` labels).
2. Resolve the start workflow state (the state that maps to the `ready`/`Todo` column) via the existing state resolver.
3. Create issues (in dependency order so blockers exist first), writing the marker into each description; set the start state; set labels.
4. Update changed issues (title/body/labels), refresh the marker `sha`.
5. Wire missing `blocked-by` relations.

Partial-failure safety: because every created issue carries its marker, an apply that dies halfway is resumable — a re-run sees the already-created issues as `skip`/`update` and continues. No compensation logic needed.

### Error handling

- Validation errors abort before any network call, listing every problem at once.
- Unknown workflow state / missing team → actionable error naming the missing entity.
- GraphQL / transport errors during `apply` → wrapped with the `ref` being processed and returned; the marker guarantees a safe re-run.
- Orphans are never deleted in v1 (no `--prune`). Real boards are not wiped.

## Component 3 — the authoring skill (new, clipse-specific)

A new skill (working name `clipse-board-bootstrap`) versioned inside the clipse repo so the skill and applier evolve together. Exact path (`skills/` vs `.claude/skills/`) resolved in the plan, since `.claude/` is currently untracked here.

Responsibilities:
- Take a prose project plan (pasted or a file path).
- Decompose it into discrete, dependency-ordered issues following clipse conventions: `agent:coder` lane labeling, `blocked-by` DAG, `human` tickets for manual work, small single-concern issues.
- Emit a schema-valid `board.yaml` + `body_file` markdowns for the user to review.
- Explicitly stop at the spec: the human reviews, then runs `clipse board plan` / `apply`. The skill never touches Linear directly.

## Component 4 — smoke.sh migration

Refactor `scripts/smoke/smoke.sh`'s `seed()` to emit a `board.yaml` (from its existing `manifest.tsv` + `TNN.md`) and call `clipse board apply`, deleting the `linear issue create | grep -oE 'CLI-[0-9]+'` scraping and dropping the external `linear` CLI as a *seed-time* dependency. smoke keeps its own `reset` / `run` / `verify` bash. This gives the applier a built-in second consumer and a live regression signal every smoke run.

`reset` still needs delete (to wipe the disposable smoke board) — that stays in smoke's bash via the `linear` CLI, or moves behind an explicit, smoke-only flag. Not part of the reusable applier.

## Testing

Mirrors clipse's existing philosophy: standard-library `testing`, table-driven, zero real network (only `httptest` loopback), no LLM.

- **Spec parse/validate**: table-driven valid/invalid specs (dup ref, cycle, undefined dep, both/neither body, missing body_file).
- **Planner**: table-driven — given a spec and a fake board state (list of issues+markers), assert the expected plan (create/update/skip/relation/orphan). Golden-style.
- **Applier**: against an `httptest` GraphQL server (mirrors `internal/linear/http_client_loopback_test.go`); assert the exact mutations issued and marker contents.
- **Idempotency**: apply against a fake board, feed the result back, assert the second plan is all-`skip`.
- **Marker round-trip**: render → parse → equal; `sha` changes iff content changes.
- **example.yaml**: parses + validates (drift guard vs the JSON Schema).

## Phasing

1. **Spec core** — `board.yaml` struct + parser + validator + `schema/board-spec.schema.json` + `board.example.yaml`. Pure, fully unit-tested.
2. **Transport extract + read + plan** — factor the shared GraphQL transport; add the board client's team-issue read + marker parsing; implement the diff/planner; wire `clipse board plan`.
3. **Apply** — label ensure, create/update/relation/state mutations, marker write; `httptest` loopback + idempotency tests; wire `clipse board apply` / `plan --apply`.
4. **smoke.sh migration** — `seed()` emits `board.yaml` and calls `clipse board apply`; retire the create/relation scraping.
5. **Milestones/projects** (optional, later) — honor `milestone:` by creating a Linear Project + milestones and linking issues.
6. **Authoring skill** — the clipse-specific prose→spec skill.

## Decisions locked in brainstorm

- Purpose: onboard a new project (spec → dispatch-ready board).
- Boundary: deterministic Go applier + separate authoring skill; no LLM in Go.
- Spec format: `board.yaml` + `schema/board-spec.schema.json` (bodies inline or `body_file`).
- Re-run model: marker in Linear + plan/apply; stateless; orphans never auto-deleted.
- Milestones: deferred to phase 5.
- Spec types: hand-written yaml-tagged struct + Go validation; schema as authoring reference, not codegen'd, not runtime-loaded.
- Skill: new, clipse-specific, repo-versioned.
