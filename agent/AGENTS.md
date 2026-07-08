# AGENTS.md — agent/

Scope: `agent/` — the Python worker package that wraps LangGraph and Deep Agents Code for per-issue coder, docs, and reviewer turns.

Inherits the root [AGENTS.md](../AGENTS.md). This file only lists worker-specific overrides.

<!-- managed:readme-agents-doc:section=SCOPED_OVERRIDES:BEGIN -->
## What's different here

- Python version floor is 3.13, managed by `uv`.
- `clipse-worker` is the console entrypoint (`clipse_agent.worker:main`).
- The worker emits one schema-valid `WorkerResult` JSON object on stdout; stderr is for logs/debug output.
- `contract.py` is generated from `../schema/worker-result.schema.json`; never edit it by hand.
<!-- managed:readme-agents-doc:section=SCOPED_OVERRIDES:END -->

<!-- managed:readme-agents-doc:section=SCOPED_COMMANDS:BEGIN -->
## Local commands

```sh
uv sync
uv run pytest
uv run pytest tests/test_file.py::test_name
uv run ruff check .
```

Run these from `agent/`. From the repo root, `make test-py`, `make lint`, and `make eval` call into this package.
<!-- managed:readme-agents-doc:section=SCOPED_COMMANDS:END -->

<!-- managed:readme-agents-doc:section=SCOPED_GOTCHAS:BEGIN -->
## Local gotchas

- **DAC engine behavior is upstream's job** — local evals pin Clipse-specific behavior and production incidents, not every Deep Agents Code mechanic.
- **`openai_codex` needs `create_model` routing** — a bare `openai_codex:*` string cannot go straight to `create_cli_agent`.
- **Transcript writes are best-effort** — failures are logged to stderr and swallowed; transcripts are debug aids, not run outcomes.
- **Docs run as a coder sub-turn** — a successful coder run can append both `coder` and `coder_docs` turns to the same transcript file.
<!-- managed:readme-agents-doc:section=SCOPED_GOTCHAS:END -->
