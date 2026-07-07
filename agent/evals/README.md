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

## Model matrix

`CLIPSE_EVAL_MODEL` overrides the lane model for the whole run:

    CLIPSE_EVAL_MODEL=openai_codex:gpt-5-codex make eval

(codex manages its own OAuth credential at `~/.deepagents/.state/`; the
anthropic key skip-guard does not apply.)

## How it works

Fixture repos are real git: a local bare repo is the `origin` remote, so
fetch/merge/push in the graphs run for real, offline. GitHub is a fake `gh`
shim (`gh_shim/gh`) placed first on `PATH` — it answers `pr view` /
`pr create` / `api` / `pr comment` from per-test state in
`$CLIPSE_EVAL_GH_DIR` and logs every call, covering both the graph nodes'
CommandRunner and the DAC agent's own shell. Graders are deterministic:
outcome enums, git state (commits, merge parents, conflict markers), token
budgets, and shim logs.

Per-case metrics (outcome, tokens, wall time) append to
`results/latest.jsonl` (gitignored) via the `record_result` fixture.

LangSmith: traces flow automatically when the standard `LANGSMITH_*` env
vars are set; no code here depends on it.

## Deferred (v2)

Inline-comment placement validity (needs live GitHub), reviewer-summary
actionability + coder↔reviewer convergence loops, docs-accuracy LLM judge,
nightly runs, failure-archive→eval-case pipeline.
