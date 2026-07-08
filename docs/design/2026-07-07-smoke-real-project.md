# Smoke test v2: real Python project target

Date: 2026-07-07. Status: approved design, pre-implementation.

## Goal

Replace the markdown-only smoke DAG with a real Python project built
ticket-by-ticket, so a smoke run exercises the recent pipeline changes live:

- `sync_base` stale-base merge + coder conflict-marker resolution (the
  newest feature; currently has zero live coverage — the old DAG was
  deliberately conflict-free).
- Per-ticket transcript logging (`<board_dir>/logs/<ISSUE>.transcript.jsonl`,
  coder + coder_docs turn pairs).
- Reviewer + rework feedback on real code diffs (inline-comment placement
  check becomes meaningful).
- The per-lane config surface (`models:` / `model_params:` /
  `shell_allow_list:`) via optional passthroughs.

The smoke stays pure external orchestration (Linear + GitHub + binary +
SQLite); `make test` still owns kernel internals, `make eval` still owns
agent behavior.

## Non-goals

- Real CI on the target repo. The required checks stay echo-only jobs
  (`go`, `python`, `codegen-drift` — protection contexts unchanged).
  Correctness is proven by the new final integration check instead
  (explicitly chosen: fast PR turnaround over CI realism).
- Provoking coder failure/rework with intentionally-broken specs
  (nondeterministic; not worth the flake).

## Target project: `greet`

A small CLI that prints configurable greetings. Baseline lives as **real
files** in `scripts/smoke/baseline/` (no heredocs); `setup` copies them into
a temp clone, commits, force-pushes `main`, and tags the new baseline.

```
scripts/smoke/baseline/
  pyproject.toml            # name=greet 0.1.0, hatchling, [project.scripts] greet=greet.cli:main,
                            # dev group: pytest>=8, requires-python >=3.11
  .gitignore                # __pycache__/, .venv/, *.egg-info/
  README.md                 # intro + "sections below added by smoke tickets" marker
  src/greet/__init__.py     # __version__ = "0.1.0"
  tests/test_baseline.py    # import greet; assert __version__ — baseline itself is green
  .github/workflows/ci.yml  # unchanged echo jobs go/python/codegen-drift
```

The `greet` console script referencing not-yet-existing `greet.cli` is fine:
entry points resolve lazily, and nothing invokes the CLI before T7 exists.

**Migration**: `BASELINE_TAG` default changes to `smoke-baseline-py`.
Re-running `setup` sees the tag missing and force-pushes the new baseline
(existing behavior). The old `smoke-baseline` tag is left behind, harmless.

## App DAG (default, 10 tickets)

Specs live as data files:

```
scripts/smoke/dag/
  manifest.tsv     # idx<TAB>title<TAB>files<TAB>deps(space-sep idx)<TAB>tags
  T01.md .. T10.md # full ticket descriptions (the Linear issue body)
```

| T | work | files | deps |
|---|------|-------|------|
| 1 | `greet(name: str) -> str` → `"Hello, {name}!"` | `src/greet/core.py`, `tests/test_core.py` | — |
| 2 | `Config` dataclass: `template`, `default_name="world"`, `locale="en"` | `src/greet/config.py`, `tests/test_config.py` | 1 |
| 3 | catalog `{"en": "Hello, {name}!", "es": "¡Hola, {name}!", "fr": "Bonjour, {name}!"}` + `template_for(locale)` (unknown → en) | `src/greet/messages.py`, `tests/test_messages.py` | 1 |
| 4 | argparse `build_parser()`: `--name`, `--locale`, `--loud` (parser only, no main wiring) | `src/greet/cli.py`, `tests/test_cli.py` | 1 |
| 5 | extend `greet(name, locale="en")` to render via catalog + config defaults | `src/greet/core.py`, `tests/test_core.py` | 2, 3 |
| 6 | `render(text, mode)`: `plain` (as-is), `loud` (`text.upper()`), `json` (`{"greeting": text}`) | `src/greet/format.py`, `tests/test_format.py` | 3 |
| 7 | wire `cli.main()`: parse → greet → render → print; integration tests | `src/greet/cli.py`, `tests/test_cli.py` | 4, 5, 6 |
| 8 | append `## Usage` section at end of README | `README.md` | 7 |
| 9 | append `## Examples` section at end of README | `README.md` | 7 |
| 10 | `CHANGELOG.md`; bump version to `0.2.0` in `pyproject.toml` + `__init__.py`; test asserts `greet.__version__ == "0.2.0"` | `CHANGELOG.md`, `pyproject.toml`, `src/greet/__init__.py`, `tests/test_baseline.py` | 8, 9 |

**T8/T9 are the conflict pair** (tag `conflict-pair` in the manifest): both
unblock in the same tick when T7 goes done, both are claimed in parallel
(coder cap ≥ 2), both worktrees branch from the same `main`, both append at
the end of README. Whichever merges second necessarily hits the stale-base
conflict → `OutcomeStaleBaseConflict` → rework → `sync_base` merges
`origin/main`, the coder resolves the markers, `make_commit`'s
unresolved-marker guard protects the push. This is deterministic as long as
both are in flight before either merges — guaranteed in practice since claim
happens in one tick and review+merge takes minutes.

Ticket-spec conventions (every `TNN.md`):

- Exact file paths, function signatures, and pinned literal strings (the
  catalog strings, the `--loud` uppercase rule, the T7 expected outputs) so
  `verify` can assert exact behavior.
- "Modify ONLY these files: …" (replaces the old "create only one file"
  guardrail — tickets now legitimately edit shared files, e.g. T5 extends
  T1's `core.py`).
- "Run `uv run pytest` and make it pass before committing."
- T7 pins the two CLI behaviors verify asserts:
  `greet --name smoke --loud` → exactly `HELLO, SMOKE!`;
  `greet --name smoke --locale es` → exactly `¡Hola, smoke!`.

## Chain mode (`--fast` = 3, `--tickets N`)

Generated formulaic real-code chain, stays in bash (parametric, so not data
files). Step 1 creates `src/greet/steps/__init__.py` +
`src/greet/steps/step_01.py` with `run(value: str) -> str` returning
`"step-01:" + value`, plus `tests/test_step_01.py`. Step i > 1 creates
`step_%02d.py` whose `run` **calls `step_{i-1}.run`** and prepends its own
tag, plus a test asserting the full composed string
(`run("x") == "step-03:step-02:step-01:x"`).

The dependency is semantic, not just ordering: a child claimed before its
blocker merged cannot pass its own tests (the module it imports does not
exist). The markdown chain proved ordering by timestamps; this proves it by
construction.

## Runtime manifest

`smoke-manifest.tsv` gains a first-line header `#mode=app` or `#mode=chain`
(readers skip `#` lines). Columns become
`idx  identifier  files  blockers  tags`. `verify` uses
the mode to pick integration asserts and the `conflict-pair` tag to find
T8/T9 without hardcoding indices.

## Harness changes (`smoke.sh`)

- **setup**: push baseline from `scripts/smoke/baseline/` (rsync/cp into the
  temp clone) instead of heredocs. Protection block unchanged.
- **seed**: app mode reads `dag/manifest.tsv` + `dag/TNN.md`
  (`--description-file` already takes a file); chain mode generates as
  today. Mode selection: default → app DAG; `--fast` / `--tickets N` →
  chain (N=10 no longer means "the greet DAG"; the app DAG is simply the
  default when neither flag is given).
- **generate_config**: drop the dead `scribe` cap (lane removed);
  optional passthroughs — `MODEL_CODER` / `MODEL_CODER_DOCS` /
  `MODEL_REVIEWER` envs emit a `models:` block (per-key, only when set), and
  `EXTRA_CONFIG_YAML` (path) is appended verbatim for nested blocks
  (`model_params:`, `shell_allow_list:`). All omitted → generated config
  identical in meaning to today.
- **defaults**: `MAX_RUNTIME_S` 900 → 1800, `TIMEOUT_S` 3600 → 5400 (code
  turns run longer than markdown turns). `CAP_SCRIBE` removed from env +
  example.
- **verify** (new assertions, on top of existing done/order/merged):
  1. **Transcripts (fatal)**: for every ticket,
     `$BOARD_DIR/logs/<ID>.transcript.jsonl` exists and its `turn_start`
     events include both lane `coder` and lane `coder_docs`.
  2. **Integration (fatal)**: fresh clone of merged `main` →
     `uv run --directory <clone> pytest -q` exits 0; no
     `<<<<<<< ` conflict markers anywhere in the tree. App mode
     additionally: the two pinned CLI invocations produce their exact
     outputs, and README contains both `## Usage` and `## Examples`.
  3. **Conflict evidence (report-only)**: for the `conflict-pair` tickets,
     print coder-run counts / stale-base rework signals from the board DB.
     Informational — the timing detail is not asserted, the merged outcome
     is (via 2).
- **check-placement.py**: unchanged, still report-only — now checks
  placement against real code diffs.

## Docs

- Rewrite `scripts/smoke/README.md` (what it exercises, new DAG, new
  asserts, migration note: re-run `setup` once after pulling).
- Update `scripts/smoke/smoke.env.example` (new defaults, new optional
  envs, drop `CAP_SCRIBE`).
- Update `configs/clipse.smoke.example.yaml` (drop scribe, add commented
  `models:` / `model_params:` example).

## Risks

- **LLM nondeterminism vs pinned asserts**: the coder could satisfy tests
  but drift on exact strings. Mitigated by pinning literals in the ticket
  spec *and* requiring the ticket's own pytest to assert them — the coder's
  local test run enforces the contract before the PR ever opens.
- **Conflict-pair timing**: if T8 fully merges before T9 is claimed, no
  conflict occurs and the rework path goes unexercised (outcome asserts
  still pass; evidence line reports it). Accepted — report-only by design.
- **Token cost**: real code + tests per ticket costs more than one markdown
  file. `--fast` (3-step chain) remains the cheap pipeline check.

## Acceptance

- `./scripts/smoke/smoke.sh setup` (once) then `./scripts/smoke/smoke.sh`
  ends `SMOKE PASS` with all 10 tickets done/merged, transcripts asserted,
  integration clone green, both README sections present.
- `./scripts/smoke/smoke.sh --fast` passes with the 3-step semantic chain.
- Conflict-evidence line reports the stale-base rework fired on one of
  T8/T9 (expected in practice, not asserted).
