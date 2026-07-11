---
name: clipse-board-bootstrap
description: Use when bootstrapping a Linear board for a new clipse project from a prose plan or spec — turning a project description into a schema-valid board.yaml (issues, blocked-by DAG, agent labels, human tickets) that `clipse board apply` then reconciles onto Linear. Triggers include "set up the Linear board for this project", "turn this plan into clipse tickets", "bootstrap the board", or any prose-plan → dispatch-ready-board request.
---

# clipse board bootstrap

Turn a prose project plan into a `board.yaml` that `clipse board apply` reconciles onto a Linear team. You (the model) do the decomposition; the deterministic Go tool does the Linear writes. **Never call Linear directly** — your only output is the spec files. The human reviews them, then runs `clipse board plan` / `apply`.

## The pipeline

```
prose plan  ──(this skill)──▶  board.yaml + body_file markdowns  ──(clipse board apply)──▶  Linear board
```

## What you produce

A `board.yaml` validated against `schema/board-spec.schema.json`, plus one markdown file per issue body under a `specs/` dir beside it. Do NOT run any `linear` command or MCP tool — stop at the files and hand off.

## Decomposition rules (clipse conventions)

- **Small, single-concern issues.** One coherent unit of work each — the size a coder agent can land in one PR. Split "build the CLI" into parser, config, wiring, etc.
- **Dependency DAG via `deps`.** If issue B builds on A's code, `B.deps: [A.ref]`. clipse gates each issue on its blockers and dispatches in dependency order. Keep it acyclic.
- **Stable `ref` per issue.** A short, tracker-agnostic id (`core-1`, `cli-parser`). This is the immutable identity clipse matches on across re-runs — never reuse or renumber one after apply.
- **Lane labels.** Agent-worked issues carry an `agent:<lane>` label (almost always `agent:coder`). Set `default_labels: [agent:coder]` at the top and omit per-issue `labels` unless an issue needs a different lane.
- **Human tickets.** Work a person must do (design calls, credential setup, external approvals) gets `human: true` — it is labeled `human`, left in Todo, and never picked up by an agent. Downstream agent issues can still `deps:` on it.
- **Bodies are the task spec the coder reads.** Write each body as a clear, self-contained task: context, what to build, acceptance criteria. Put anything longer than a couple lines in a `body_file`.

## Spec shape

```yaml
team: CLI                     # Linear team key (ask if unknown)
base_branch: main
default_labels: [agent:coder]
issues:
  - ref: core-1
    title: Greeter core
    deps: []
    body_file: specs/core-1.md
  - ref: cfg-1
    title: Config dataclass
    deps: [core-1]
    body: |
      Short inline body is fine for small tickets.
  - ref: docs-1
    title: Write the README
    human: true
    deps: [core-1, cfg-1]
    body: |
      Human-authored once the API stabilizes.
```

See `configs/board.example.yaml` for a complete example and `schema/board-spec.schema.json` for the full field reference.

## Procedure

1. Read the prose plan. If the Linear team key isn't given, ask for it.
2. Decompose into issues with refs, deps, labels, and human flags per the rules above.
3. Write `board.yaml` and the `specs/<ref>.md` body files.
4. Validate mentally against the schema (required: `team`, and per issue `ref`+`title`+ exactly one of `body`/`body_file`; deps must reference defined refs; no cycles).
5. Hand off: tell the human to review, then run `clipse board plan board.yaml` to preview and `clipse board apply board.yaml` to reconcile. Re-runs are idempotent — safe to amend the spec and re-apply.

## Do not

- Do not create, edit, or delete Linear issues yourself (no `linear` CLI, no Linear MCP tools). The Go applier owns every write.
- Do not invent a team key or fabricate refs for existing issues — a ref is the identity clipse reconciles on.
