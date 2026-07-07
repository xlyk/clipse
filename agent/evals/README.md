# Agent evals

Live-model behavioral evals for clipse's three LLM surfaces: the coder turn,
the coder's docs sub-step, and the reviewer turn. Every case pins a
clipse-specific behavior (profiles, allow-lists, tail/verdict protocols,
sync_base conflict flow) or a real production incident — nothing here
re-tests DAC engine mechanics (upstream `langchain-ai/deepagents`
`libs/evals` covers those; run that suite when bumping the DAC pin).

## Running

    make eval                       # from the repo root; needs ANTHROPIC_API_KEY
    cd agent && uv run pytest evals -k token_discipline -v   # one case

- `make test` never runs these (pytest `testpaths` is scoped to `tests/`).
- Without `ANTHROPIC_API_KEY` the live cases skip; the harness self-test
  always runs. Credentials: `source ~/.secrets`.
- Cost: a full run is a handful of coder/reviewer turns on the default
  lane models — order of a few dollars.

## How it works

Fixture repos are real git: a local bare repo is the `origin` remote, so
fetch/merge/push in the graphs run for real, offline. GitHub is a fake `gh`
shim (`gh_shim/gh`) placed first on `PATH` — it answers `pr view` /
`pr create` / `api` / `pr comment` from per-test state in
`$CLIPSE_EVAL_GH_DIR` and logs every call, covering both the graph nodes'
CommandRunner and the DAC agent's own shell. Graders are deterministic:
outcome enums, git state (commits, merge parents, conflict markers), token
budgets, and shim logs.

Per-case metrics append to `results/run-<utc-ts>.jsonl` (one file per pytest
session, gitignored); `results/latest.jsonl` is a symlink to the newest run.
A status row per case (pass/fail/skip + wall time) is appended by a conftest
hook. Summarize the newest run with `make eval-report` (or
`uv run python evals/report.py path/to/run.jsonl` for an older one).

LangSmith: traces flow automatically when the standard `LANGSMITH_*` env
vars are set; no code here depends on it.

## Model matrix & cadence

`CLIPSE_EVAL_MODEL` overrides the lane model for the whole run:

    CLIPSE_EVAL_MODEL=openai_codex:gpt-5.1-codex make eval

Codex prerequisite (once per host, as the dispatcher's OS user): interactive
ChatGPT sign-in via `uv --project agent run dcode` -> `/auth` -> `openai_codex`;
the token lands at `~/.deepagents/.state/chatgpt-auth.json` and auto-refreshes
(see AGENTS.md). The anthropic skip guard does not apply to lane turns on a
codex run -- but D3's docs-accuracy judge is PINNED to
`anthropic:claude-haiku-4-5` so verdicts stay comparable across the matrix:
without `ANTHROPIC_API_KEY` in the env, D3 skips and everything else runs.

Recommended cadence (no automation infra -- deliberate):

- **Nightly local:** a cron/launchd line on the dev machine, e.g.
  `cd ~/Code/clipse && source ~/.secrets && make eval && make eval-report`
  once for the default lane models, optionally a second run with
  `CLIPSE_EVAL_MODEL` for the codex matrix. Runs are cheap enough to eyeball
  the next morning via `results/run-*.jsonl`.
- **Manual pre-release:** before tagging or bumping a lane model / the DAC
  pin, run the full suite on both the default models and each configured
  matrix model, and compare `rounds_to_done` / token totals against the last
  few run files.

Cost: full default-model run is on the order of $10-20 (the L2 convergence
cases dominate -- each is 2-6 live turns with reviewer rounds on opus).
Filter with `-k` when iterating on one case.

## Deferred

R5 as a pytest eval (placement is smoke-side: `scripts/smoke/check-placement.py`),
`langsmith[pytest]` judge feedback, nightly automation infra,
failure-archive->eval-case pipeline.
