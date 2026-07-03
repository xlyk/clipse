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
  -> documentation -> done, ending in a squash-merged PR on the target repo.
- Concurrency: the greet DAG fans out and fans back in, so multiple issues
  run in parallel under the configured caps.

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
2. pushes a baseline commit (README + a no-op CI workflow) and tags it
   `smoke-baseline`,
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
   title prefix).

It prints a per-ticket table (status, merged?, wall-clock seconds, tokens)
and a total token count, then a `SMOKE PASS` / `SMOKE FAIL` banner. The
process exits `0` only if every assertion passed, `1` otherwise (including a
run-phase timeout).

## How reset gets a true clean slate

`reset` wipes every side of the pipeline:

- **Linear**: deletes every issue on team `CLI` whose title starts with
  `[smoke]` (the smoke marker).
- **GitHub**: closes open PRs (deleting their head branches), force-resets
  `main` to the `smoke-baseline` tag, and deletes every remaining non-main
  branch (merged or abandoned PR heads).
- **Local**: removes the board dir (SQLite db, singleton lock, worktrees) and
  the checkpoints dir, then re-clones the primary from the freshly-reset
  target repo.

Because the target is a dedicated throwaway repo, resetting it to a fixed
baseline is safe and total -- your real project is never touched.

## Cost & time notes

- Each ticket is one worker run that creates one small markdown file and opens
  a PR; the DAC coder + reviewer + scribe lanes each consume Anthropic tokens.
- `--fast` (3 tickets) is the cheapest way to confirm the pipeline end-to-end.
- The full greet DAG is 10 runs plus review/merge/doc lanes; token spend is
  reported per-ticket and in total by `verify`. `MAX_TOKENS_PER_RUN` in
  `smoke.env` caps per-run spend.
- Wall clock depends on Anthropic + GitHub Actions latency; the watcher prints
  a status line every `WATCH_INTERVAL_S` and gives up after `TIMEOUT_S`.

## Files

- `smoke.sh` -- the harness.
- `smoke.env.example` -- documented config template (copy to `smoke.env`).
- `../../configs/clipse.smoke.example.yaml` -- shows the generated dispatcher
  config shape (documentation only; the harness generates the live one).
