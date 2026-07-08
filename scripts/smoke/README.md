# clipse clean-slate smoke test

End-to-end smoke test that drives the whole clipse pipeline against real
external systems: it seeds a dependency DAG of Linear issues, runs the
dispatcher, and asserts the issues turn into merged PRs in the right order.

This is pure external orchestration (Linear + GitHub + the clipse binary +
SQLite). It never touches the Go/Python kernel internals -- for that, use
`make test`.

## What it exercises

- Dependency gating: an issue stays in Todo until its Linear "blocks"
  dependencies are Done, then is promoted to Ready, claimed, and worked.
- The full board pipeline per issue: ready -> running -> review -> merging
  -> done, ending in a squash-merged PR on the target repo. (Documentation is
  a step inside the coder graph, not its own column -- see AGENTS.md.)
- The default 10-ticket **greet app DAG**: real Python modules + pytest tests
  assembled into a working `greet` CLI, fanning out and back in so multiple
  issues run in parallel under the configured caps.
- The **README conflict pair** (tickets T8/T9, tagged `conflict-pair`): two
  tickets that both edit `README.md`, deliberately exercising `sync_base`'s
  stale-base merge and the coder's conflict-marker resolution.
- **Semantic code chains** for `--fast` / `--tickets N`: a generated N-step
  chain where step i's module imports step i-1's, so a child claimed before
  its blocker merged fails its own tests -- dependency order is enforced by
  construction, not just asserted after the fact.
- Per-ticket **transcript** assertions and a final **integration clone**
  check (see "What pass means" below).

## Prerequisites

- Tools on PATH: `linear`, `gh` (authenticated -- `gh auth status`),
  `sqlite3`, `uv`, `git`, `make`, `jq`.
- `LINEAR_API_KEY` for the `clipse-development` workspace (default sourced
  from `~/.secrets`).
- `ANTHROPIC_API_KEY` for the worker (default: already exported; or point
  `ANTHROPIC_KEY_SOURCE` at a file to source).
- A GitHub account that can create the throwaway target repo and apply branch
  protection to it (see the note on private repos below).

## One-time setup

```sh
cp scripts/smoke/smoke.env.example scripts/smoke/smoke.env
$EDITOR scripts/smoke/smoke.env          # confirm TARGET_REPO, TEAM_*, paths
./scripts/smoke/smoke.sh setup
```

`setup` is idempotent. It:

1. creates the private `TARGET_REPO` if it does not exist,
2. pushes the baseline project from `scripts/smoke/baseline/` (a
   pyproject.toml/hatchling `greet` project, source + tests + a no-op CI
   workflow) and tags it `BASELINE_TAG` (default `smoke-baseline-py`),
3. applies branch protection on `main` matching clipse's gitops merge gate:
   strict required status checks (`go`, `python`, `codegen-drift`), 0 required
   approvals, admins not enforced, force-pushes allowed (so `reset` can
   force `main` back to the baseline).

The CI workflow's three jobs are named exactly `go`, `python`,
`codegen-drift` so they satisfy the required status checks and go green in
seconds -- doc-only PRs pass CI fast.

> Branch protection on a **private** repo may require a paid GitHub plan. If
> `setup` reports that protection did not apply, set `REPO_VISIBILITY=public`
> in `smoke.env`, delete the repo (`gh repo delete "$TARGET_REPO"`), and
> re-run `setup`. The merge gate needs `main` to be protected.

## Usage

```sh
# Quick smoke: 3-ticket linear chain (~10 min, ~small token spend).
./scripts/smoke/smoke.sh --fast

# Full smoke: the 10-ticket greet DAG (default).
./scripts/smoke/smoke.sh

# Arbitrary size (linear chain of N).
./scripts/smoke/smoke.sh --tickets 5

# Seed only, then inspect / drive manually.
./scripts/smoke/smoke.sh --no-run
./bin/clipse tui --board <BOARD_DIR>

# Keep all artifacts + board after the run for inspection.
./scripts/smoke/smoke.sh --fast --keep
```

With no subcommand it runs the full pipeline:
`reset -> build -> seed -> run -> verify -> teardown`.

Individual phases are also subcommands: `setup`, `reset`, `build`, `seed`,
`generate` (write the config only), `run` (launch + poll + verify), `verify`.

## What "pass" means

`verify` asserts, for every seeded ticket:

1. its final board status is `done`;
2. dependency order held -- for each `child <- blocker` edge, the blocker
   reached `done` no later than the child (a child cannot start before its
   blockers are done, so it necessarily finishes after them);
3. it has a squash-merged PR on the target repo (matched by the `CLI-N:` PR
   title prefix);
4. it has a per-ticket transcript (`$BOARD_DIR/logs/<ID>.transcript.jsonl`)
   with a `turn_start` event for both the `coder` lane and the `coder_docs`
   lane -- proof the coder graph's docs sub-turn actually ran;
5. a fresh clone of the merged target repo's `main` passes `uv run pytest`
   with no leftover conflict markers, and (app mode only) the `greet` CLI
   produces the exact pinned outputs (`greet --name smoke --loud` ->
   `HELLO, SMOKE!`; `--locale es` -> `¡Hola, smoke!`) and the README has both
   a `## Usage` and an `## Examples` section.

`verify` also reports (but never fails on) whether the conflict-pair
ticket's coder ran more than once -- evidence the stale-base rework path
actually fired this run; it's timing-dependent, so assertion 5's clean
integration clone is what decides pass/fail regardless.

It prints a per-ticket table (status, merged?, wall-clock seconds, tokens)
and a total token count, then a `SMOKE PASS` / `SMOKE FAIL` banner. The
process exits `0` only if every assertion passed, `1` otherwise (including a
run-phase timeout).

## How reset gets a true clean slate

`reset` wipes every side of the pipeline:

- **Linear**: deletes every issue on team `CLI` whose title starts with
  `[smoke]` (the smoke marker).
- **GitHub**: closes open PRs (deleting their head branches), force-resets
  `main` to the `BASELINE_TAG` tag, and deletes every remaining non-main
  branch (merged or abandoned PR heads).
- **Local**: removes the board dir (SQLite db, singleton lock, worktrees) and
  the checkpoints dir, then re-clones the primary from the freshly-reset
  target repo.

Because the target is a dedicated throwaway repo, resetting it to a fixed
baseline is safe and total -- your real project is never touched.

## Cost & time notes

- Each ticket is a real module + a pytest test, not one small markdown file --
  it costs more tokens than the old markdown-ticket smoke. `--fast` (3-step
  chain) is the cheap way to confirm the pipeline end-to-end.
- The full greet DAG is 10 runs plus the review/merge lanes and the coder's
  docs sub-turn; token spend is reported per-ticket and in total by `verify`.
  `MAX_TOKENS_PER_RUN` in `smoke.env` caps per-run spend.
- Defaults are sized for real code turns: `MAX_RUNTIME_S=1800` (per-worker
  wall-clock kill) and `TIMEOUT_S=5400` (overall run-phase watcher timeout).
- Wall clock depends on Anthropic + GitHub Actions latency; the watcher prints
  a status line every `WATCH_INTERVAL_S` and gives up after `TIMEOUT_S`.

## Migration from the markdown smoke (pre-2026-07-07)

The baseline target project changed from a bare README to a real
`pyproject.toml`/hatchling `greet` project, and `BASELINE_TAG` now defaults
to `smoke-baseline-py` (was `smoke-baseline`). Re-run
`./scripts/smoke/smoke.sh setup` once after pulling: it sees the new tag
missing on the target repo and force-pushes the new baseline. The old
`smoke-baseline` tag is left behind on the target repo -- harmless, just
unused.

## Files

- `smoke.sh` -- the harness.
- `smoke.env.example` -- documented config template (copy to `smoke.env`).
- `baseline/` -- the real `greet` project (pyproject.toml, src, tests, CI
  workflow) pushed to the target repo as its baseline commit.
- `dag/` -- the default app DAG: `manifest.tsv` (index/title/files/deps/tags)
  plus one `TNN.md` spec file per ticket (the verbatim Linear issue body).
- `../../configs/clipse.smoke.example.yaml` -- shows the generated dispatcher
  config shape (documentation only; the harness generates the live one).
