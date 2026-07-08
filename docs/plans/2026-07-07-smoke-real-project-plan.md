# Smoke Test v2 (Real Python Project) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the markdown-only smoke DAG with a real Python `greet` CLI built ticket-by-ticket, exercising `sync_base` conflict resolution, transcripts, and the per-lane config surface live.

**Architecture:** The baseline target project becomes real files in `scripts/smoke/baseline/` (pushed by `setup`); the curated 10-ticket app DAG becomes data files in `scripts/smoke/dag/` (TSV manifest + per-ticket markdown specs); `--fast`/`--tickets N` becomes a generated *semantic* code chain (each step imports the previous step's module). `verify` gains fatal transcript + integration-clone assertions and a report-only conflict-evidence section.

**Tech Stack:** bash (`scripts/smoke/smoke.sh`), Python 3.11+ target project (hatchling, pytest, uv), `gh`/`linear`/`jq`/`sqlite3` CLIs.

**Spec:** `docs/design/2026-07-07-smoke-real-project.md` (approved).

## Global Constraints

- All work on branch `feat/smoke-real-project` in the clipse repo.
- No Go/Python kernel changes — this touches only `scripts/smoke/`, `configs/clipse.smoke.example.yaml`, and docs.
- Commits: conventional, lowercase, no trailing period, no AI signature, one concern per commit. Never `git add -A` / `git add .` in the clipse repo (explicit paths only).
- `[smoke]` title prefix is the reset marker — every seeded ticket title MUST keep it.
- Branch-protection contexts stay exactly `go`, `python`, `codegen-drift` (echo CI unchanged).
- `BASELINE_TAG` default becomes `smoke-baseline-py`.
- Pinned literals (asserted by verify, byte-for-byte): catalog `"en": "Hello, {name}!"`, `"es": "¡Hola, {name}!"`, `"fr": "Bonjour, {name}!"`; CLI outputs `HELLO, SMOKE!` and `¡Hola, smoke!`; README headings `## Usage`, `## Examples`.
- After every smoke.sh edit: `bash -n scripts/smoke/smoke.sh` must pass (and `shellcheck scripts/smoke/smoke.sh` if installed).
- `set -euo pipefail` is active in smoke.sh: never end a `{ …; }` group or function with a bare `[[ … ]] && …` (a false condition returns 1 and kills the script) — use `if` statements.

---

### Task 1: Baseline target project (`scripts/smoke/baseline/`)

**Files:**
- Create: `scripts/smoke/baseline/pyproject.toml`
- Create: `scripts/smoke/baseline/.gitignore`
- Create: `scripts/smoke/baseline/README.md`
- Create: `scripts/smoke/baseline/src/greet/__init__.py`
- Create: `scripts/smoke/baseline/tests/test_baseline.py`
- Create: `scripts/smoke/baseline/.github/workflows/ci.yml`

**Interfaces:**
- Produces: the on-disk baseline that Task 3's `push_baseline` tars and pushes. The nested `.gitignore` also keeps `.venv/`, `uv.lock`, `__pycache__/` out of the *clipse* repo (nested gitignores apply to the subtree), so no top-level `.gitignore` change is needed.

- [ ] **Step 1: Write the baseline files**

`scripts/smoke/baseline/pyproject.toml`:

```toml
[project]
name = "greet"
version = "0.1.0"
description = "sample CLI built ticket-by-ticket by the clipse smoke test"
requires-python = ">=3.11"
dependencies = []

[project.scripts]
greet = "greet.cli:main"

[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"

[tool.hatch.build.targets.wheel]
packages = ["src/greet"]

[dependency-groups]
dev = ["pytest>=8"]
```

(The `greet.cli:main` entry point does not exist until ticket T7 — entry points resolve lazily, nothing invokes the CLI before then.)

`scripts/smoke/baseline/.gitignore`:

```gitignore
__pycache__/
.venv/
.pytest_cache/
*.egg-info/
uv.lock
```

(`uv.lock` is ignored so a coder running `uv run pytest` in its worktree never has an untracked lockfile to accidentally commit.)

`scripts/smoke/baseline/README.md`:

```markdown
# greet

Sample command-line tool that prints a configurable greeting. Built
ticket-by-ticket by the clipse smoke test (`scripts/smoke/` in the clipse
repo); `main` is force-reset to the baseline tag on every `smoke.sh reset`.
Do not store anything here you want to keep.

<!-- sections below this line are added by smoke tickets -->
```

`scripts/smoke/baseline/src/greet/__init__.py`:

```python
__version__ = "0.1.0"
```

`scripts/smoke/baseline/tests/test_baseline.py`:

```python
import greet


def test_version() -> None:
    assert greet.__version__ == "0.1.0"
```

`scripts/smoke/baseline/.github/workflows/ci.yml` (identical to the current `ci_workflow()` heredoc — echo-only, job names match the protection contexts):

```yaml
name: ci
on:
  push:
    branches: [main]
  pull_request:
jobs:
  go:
    name: go
    runs-on: ubuntu-latest
    steps:
      - run: echo "go ok"
  python:
    name: python
    runs-on: ubuntu-latest
    steps:
      - run: echo "python ok"
  codegen-drift:
    name: codegen-drift
    runs-on: ubuntu-latest
    steps:
      - run: echo "codegen-drift ok"
```

- [ ] **Step 2: Verify the baseline is green (in a temp copy, never in place)**

Run:

```bash
tmp="$(mktemp -d)" && tar -C scripts/smoke/baseline -cf - . | tar -C "$tmp" -xf - \
  && (cd "$tmp" && uv run pytest -q); rm -rf "$tmp"
```

Expected: `1 passed`.

- [ ] **Step 3: Commit**

```bash
git add scripts/smoke/baseline
git commit -m "feat(smoke): real greet baseline project as data files"
```

---

### Task 2: App-DAG data files (`scripts/smoke/dag/`)

**Files:**
- Create: `scripts/smoke/dag/manifest.tsv`
- Create: `scripts/smoke/dag/T01.md` … `scripts/smoke/dag/T10.md`

**Interfaces:**
- Produces: `manifest.tsv` columns `idx<TAB>title<TAB>files<TAB>deps<TAB>tags` (files comma-separated, deps space-separated indices, tags space-separated or empty; exactly 5 tab-separated fields per row). Task 5's `_dag_app` reads it; `TNN.md` is the verbatim Linear issue body.

- [ ] **Step 1: Generate `manifest.tsv`** (via printf so the tabs are real):

```bash
mkdir -p scripts/smoke/dag
{
  printf '1\t[smoke] Greeter core\tsrc/greet/core.py,tests/test_core.py\t\t\n'
  printf '2\t[smoke] Config dataclass\tsrc/greet/config.py,tests/test_config.py\t1\t\n'
  printf '3\t[smoke] Message catalog (i18n)\tsrc/greet/messages.py,tests/test_messages.py\t1\t\n'
  printf '4\t[smoke] CLI parser\tsrc/greet/cli.py,tests/test_cli.py\t1\t\n'
  printf '5\t[smoke] Locale-aware core\tsrc/greet/core.py,tests/test_core.py\t2 3\t\n'
  printf '6\t[smoke] Output formatter\tsrc/greet/format.py,tests/test_format.py\t3\t\n'
  printf '7\t[smoke] Wire CLI end-to-end\tsrc/greet/cli.py,tests/test_cli.py\t4 5 6\t\n'
  printf '8\t[smoke] README usage section\tREADME.md\t7\tconflict-pair\n'
  printf '9\t[smoke] README examples section\tREADME.md\t7\tconflict-pair\n'
  printf '10\t[smoke] Release 0.2.0\tCHANGELOG.md,pyproject.toml,src/greet/__init__.py,tests/test_baseline.py\t8 9\t\n'
} > scripts/smoke/dag/manifest.tsv
```

- [ ] **Step 2: Write the ten ticket specs**

Every spec ends with the same constraints block (adjusted file list per ticket). Write each file exactly as below.

`scripts/smoke/dag/T01.md`:

```markdown
Implement the greeter core.

Create `src/greet/core.py` with exactly one public function:

    def greet(name: str) -> str

It returns the string `Hello, {name}!` — e.g. `greet("smoke")` returns
`"Hello, smoke!"`.

Create `tests/test_core.py` with pytest tests covering at least:

- `greet("smoke") == "Hello, smoke!"`
- `greet("world") == "Hello, world!"`

Constraints:

- Modify ONLY these files: `src/greet/core.py`, `tests/test_core.py`.
- Run `uv run pytest` from the repo root and make it pass before committing.
- Do not add dependencies and do not touch `pyproject.toml` or CI files.
```

`scripts/smoke/dag/T02.md`:

```markdown
Add the configuration model.

Create `src/greet/config.py` defining a frozen dataclass:

    from dataclasses import dataclass

    @dataclass(frozen=True)
    class Config:
        template: str = "Hello, {name}!"
        default_name: str = "world"
        locale: str = "en"

Create `tests/test_config.py` with pytest tests covering at least:

- default construction yields exactly the three defaults above
- constructing with a custom `locale` and `default_name` round-trips

Constraints:

- Modify ONLY these files: `src/greet/config.py`, `tests/test_config.py`.
- Run `uv run pytest` from the repo root and make it pass before committing.
- Do not add dependencies and do not touch `pyproject.toml` or CI files.
```

`scripts/smoke/dag/T03.md`:

```markdown
Add the i18n message catalog.

Create `src/greet/messages.py` with a module-level catalog and one function:

    CATALOG: dict[str, str] = {
        "en": "Hello, {name}!",
        "es": "¡Hola, {name}!",
        "fr": "Bonjour, {name}!",
    }

    def template_for(locale: str) -> str

`template_for` returns the catalog entry for `locale`, falling back to the
`"en"` entry for any unknown locale. The three catalog strings must be
EXACTLY as written above, byte for byte — other tooling asserts them.

Create `tests/test_messages.py` with pytest tests covering at least:

- each of the three locales returns its exact string above
- an unknown locale (e.g. `"de"`) falls back to the `"en"` template

Constraints:

- Modify ONLY these files: `src/greet/messages.py`, `tests/test_messages.py`.
- Run `uv run pytest` from the repo root and make it pass before committing.
- Do not add dependencies and do not touch `pyproject.toml` or CI files.
```

`scripts/smoke/dag/T04.md`:

```markdown
Add the CLI argument parser (parser only — no program wiring yet; a later
ticket wires the pipeline).

Create `src/greet/cli.py` with:

    import argparse

    def build_parser() -> argparse.ArgumentParser

The parser accepts:

- `--name` (string, default `"world"`)
- `--locale` (string, default `"en"`)
- `--loud` (boolean flag via `action="store_true"`, default off)

Also define the entry point stub (a later ticket fills it in):

    def main(argv: list[str] | None = None) -> int

For now `main` only parses the arguments and returns 0 — it must not print
anything yet.

Create `tests/test_cli.py` with pytest tests covering at least:

- parsing no arguments yields the three defaults
- parsing `--name smoke --locale es --loud` yields those values

Constraints:

- Modify ONLY these files: `src/greet/cli.py`, `tests/test_cli.py`.
- Run `uv run pytest` from the repo root and make it pass before committing.
- Do not add dependencies and do not touch `pyproject.toml` or CI files.
```

`scripts/smoke/dag/T05.md`:

```markdown
Make the greeter locale-aware.

Modify `src/greet/core.py` so the public function becomes:

    def greet(name: str | None = None, locale: str = "en") -> str

Behavior:

- The template comes from `greet.messages.template_for(locale)`.
- When `name` is None, use `greet.config.Config().default_name`
  (i.e. `"world"`).
- `greet("smoke")` still returns `"Hello, smoke!"`.
- `greet("smoke", locale="es")` returns `"¡Hola, smoke!"` exactly.

Update `tests/test_core.py` to cover at least: the en default, es, fr, an
unknown locale falling back to en, and the default name when called with
no arguments (`greet() == "Hello, world!"`).

Constraints:

- Modify ONLY these files: `src/greet/core.py`, `tests/test_core.py`.
- Run `uv run pytest` from the repo root and make it pass before committing.
- Do not add dependencies and do not touch `pyproject.toml` or CI files.
```

`scripts/smoke/dag/T06.md`:

```markdown
Add the output formatter.

Create `src/greet/format.py` with exactly one public function:

    def render(text: str, mode: str = "plain") -> str

Modes:

- `"plain"`: return `text` unchanged.
- `"loud"`: return `text.upper()`.
- `"json"`: return `json.dumps({"greeting": text})`.
- any other mode: raise `ValueError`.

Create `tests/test_format.py` with pytest tests covering at least:

- `render("Hello, smoke!", "plain") == "Hello, smoke!"`
- `render("Hello, smoke!", "loud") == "HELLO, SMOKE!"`
- the json mode round-trips through `json.loads` to `{"greeting": "Hello, smoke!"}`
- an unknown mode raises `ValueError`

Constraints:

- Modify ONLY these files: `src/greet/format.py`, `tests/test_format.py`.
- Run `uv run pytest` from the repo root and make it pass before committing.
- Do not add dependencies and do not touch `pyproject.toml` or CI files.
```

`scripts/smoke/dag/T07.md`:

```markdown
Wire the CLI end-to-end.

Modify `src/greet/cli.py` so `main(argv)` parses the flags with
`build_parser()`, computes the greeting via
`greet.core.greet(args.name, locale=args.locale)`, renders it via
`greet.format.render` (mode `"loud"` when `--loud` is set, otherwise
`"plain"`), prints the result to stdout, and returns 0.

These exact behaviors are asserted by external tooling — pin them in tests:

- `greet --name smoke --loud` prints exactly `HELLO, SMOKE!`
- `greet --name smoke --locale es` prints exactly `¡Hola, smoke!`
- `greet` with no arguments prints exactly `Hello, world!`

Update `tests/test_cli.py` with integration tests that call `main([...])`
directly and assert the captured stdout (pytest `capsys`) for all three
lines above.

Constraints:

- Modify ONLY these files: `src/greet/cli.py`, `tests/test_cli.py`.
- Run `uv run pytest` from the repo root and make it pass before committing.
- Do not add dependencies and do not touch `pyproject.toml` or CI files.
```

`scripts/smoke/dag/T08.md`:

```markdown
Document usage.

Modify `README.md` ONLY, and only by APPENDING at the very end of the
file: add a new section whose heading is exactly

    ## Usage

followed by a short description of the CLI flags (`--name`, `--locale`,
`--loud`) and one example invocation with its output.

Constraints:

- Modify ONLY `README.md`, appending at the end — do not reorder or edit
  existing content.
- Keep the heading text exactly `## Usage`.
- Run `uv run pytest` from the repo root before committing — the suite
  must still be green (this ticket changes no code).
```

`scripts/smoke/dag/T09.md`:

```markdown
Add usage examples.

Modify `README.md` ONLY, and only by APPENDING at the very end of the
file: add a new section whose heading is exactly

    ## Examples

followed by two or three example invocations of the `greet` CLI with
their expected outputs (cover at least one non-English locale and the
`--loud` flag).

Constraints:

- Modify ONLY `README.md`, appending at the end — do not reorder or edit
  existing content.
- Keep the heading text exactly `## Examples`.
- Run `uv run pytest` from the repo root before committing — the suite
  must still be green (this ticket changes no code).
```

`scripts/smoke/dag/T10.md`:

```markdown
Cut the 0.2.0 release.

- Create `CHANGELOG.md` with a `# Changelog` heading and a `## 0.2.0`
  section briefly summarizing the shipped features (core greeting, i18n
  catalog, output formatter, CLI).
- Bump the version to `0.2.0` in BOTH places:
  - `pyproject.toml`: `version = "0.2.0"`
  - `src/greet/__init__.py`: `__version__ = "0.2.0"`
- Update `tests/test_baseline.py` so it asserts
  `greet.__version__ == "0.2.0"`.

Constraints:

- Modify ONLY these files: `CHANGELOG.md`, `pyproject.toml`,
  `src/greet/__init__.py`, `tests/test_baseline.py`.
- Run `uv run pytest` from the repo root and make it pass before committing.
- Do not add dependencies and do not touch CI files.
```

- [ ] **Step 3: Sanity-check the manifest and spec files**

Run:

```bash
awk -F'\t' 'NF!=5 {print "bad row " NR ": " $0; bad=1} END {exit bad}' scripts/smoke/dag/manifest.tsv \
  && for i in $(seq -w 1 10); do [[ -f "scripts/smoke/dag/T$i.md" ]] || echo "missing T$i.md"; done \
  && echo manifest-ok
```

Expected: `manifest-ok` and no `missing` lines.

- [ ] **Step 4: Commit**

```bash
git add scripts/smoke/dag
git commit -m "feat(smoke): 10-ticket greet app dag as data files"
```

---

### Task 3: `setup` pushes the baseline dir; new tag default

**Files:**
- Modify: `scripts/smoke/smoke.sh` (`load_env` default at ~line 92; delete `ci_workflow()` + `baseline_readme()` at lines 243–279; rewrite the baseline-push block inside `setup()` at lines 316–338)

**Interfaces:**
- Consumes: `scripts/smoke/baseline/` from Task 1.
- Produces: `push_baseline()`; `BASELINE_DIR` global; `BASELINE_TAG` default `smoke-baseline-py`.

- [ ] **Step 1: Change the tag default in `load_env`**

```bash
# old
: "${BASELINE_TAG:=smoke-baseline}"
# new
: "${BASELINE_TAG:=smoke-baseline-py}"
```

- [ ] **Step 2: Add `BASELINE_DIR` next to the other path globals in `load_env`** (after the `CLIPSE_REPO` default):

```bash
BASELINE_DIR="$SCRIPT_DIR/baseline"
```

- [ ] **Step 3: Delete `ci_workflow()` and `baseline_readme()` entirely; add `push_baseline()` in their place**

```bash
# push_baseline publishes scripts/smoke/baseline/ as the target repo's
# baseline commit and tags it. The tar copy excludes local dev droppings
# (.venv, caches, uv.lock) so only the intended files are pushed.
push_baseline() {
  local tmp
  tmp="$(mktemp -d)"
  tar -C "$BASELINE_DIR" \
    --exclude .venv --exclude __pycache__ --exclude .pytest_cache \
    --exclude uv.lock --exclude '*.egg-info' \
    -cf - . | tar -C "$tmp" -xf -
  (
    cd "$tmp"
    git init -q -b "$BASE_BRANCH"
    git add pyproject.toml .gitignore README.md src tests .github
    # Local identity so the commit succeeds regardless of global git config.
    git -c user.name="clipse-smoke" -c user.email="smoke@clipse.local" \
      commit -q -m "chore: smoke baseline (greet project)"
    git remote add origin "$REMOTE_URL"
    git push -q --force -u origin "$BASE_BRANCH"
    git tag "$BASELINE_TAG"
    git push -q --force origin "refs/tags/$BASELINE_TAG"
  )
  rm -rf "$tmp"
}
```

- [ ] **Step 4: Replace the baseline-push block inside `setup()`** — the whole `else` arm (the `tmp`/`git init`/heredoc dance) becomes:

```bash
  if gh api "repos/$TARGET_REPO/git/ref/tags/$BASELINE_TAG" >/dev/null 2>&1; then
    info "baseline tag $BASELINE_TAG already present -- skipping baseline push"
  else
    info "pushing baseline project + $BASELINE_TAG tag"
    push_baseline
  fi
```

- [ ] **Step 5: Verify**

Run: `bash -n scripts/smoke/smoke.sh && grep -c 'ci_workflow\|baseline_readme' scripts/smoke/smoke.sh`
Expected: syntax OK and `0` (both heredoc functions and their call sites gone).

- [ ] **Step 6: Commit**

```bash
git add scripts/smoke/smoke.sh
git commit -m "feat(smoke): setup pushes baseline dir, smoke-baseline-py tag"
```

---

### Task 4: Mode plumbing, semantic code chain, manifest v2

**Files:**
- Modify: `scripts/smoke/smoke.sh` (globals ~line 50; flag parsing ~line 841; `build_dag` lines 468–485; `_dag_chain` lines 487–502; `write_manifest` lines 589–601; `manifest_ids` lines 603–607)

**Interfaces:**
- Consumes: nothing new.
- Produces: globals `MODE` (`app`|`chain`), `T_TAGS` array, `TICKETS_SET`; runtime manifest format v2: first line `#mode=<MODE>`, rows `idx<TAB>identifier<TAB>files<TAB>blockers<TAB>tags`; helpers `manifest_mode()` and comment-safe `manifest_ids()`. Task 5 adds `_dag_app`; Task 7 reads `manifest_mode` + tags.

- [ ] **Step 1: Extend the globals block (~line 50)**

```bash
T_TITLE=() ; T_FILE=() ; T_DESC=() ; T_DEPS=() ; T_TAGS=() ; IDS=()
N=0
MODE=""
```

and in the flags block add:

```bash
TICKETS=10
TICKETS_SET=0
```

- [ ] **Step 2: Flag parsing sets `TICKETS_SET`** — in `main()`'s case arms:

```bash
      --tickets) shift; TICKETS="${1:?--tickets needs a value}"; TICKETS_SET=1 ;;
      --tickets=*) TICKETS="${1#*=}"; TICKETS_SET=1 ;;
```

- [ ] **Step 3: Rewrite `build_dag` (mode selection)**

```bash
# build_dag fills T_TITLE / T_FILE / T_DESC / T_DEPS / T_TAGS (1-indexed),
# N, and MODE.
#   (no flag)     -> the 10-ticket greet app DAG from scripts/smoke/dag/
#   --fast        -> 3-step semantic code chain
#   --tickets N   -> N-step semantic code chain
build_dag() {
  T_TITLE=(_) ; T_FILE=(_) ; T_DESC=(_) ; T_DEPS=(_) ; T_TAGS=(_) ; IDS=(_)

  if [[ "$FAST" -eq 1 ]]; then
    MODE="chain" ; N=3 ; _dag_chain ; return
  fi
  if [[ "$TICKETS_SET" -eq 1 ]]; then
    MODE="chain" ; N="$TICKETS" ; _dag_chain ; return
  fi
  MODE="app" ; _dag_app
}
```

- [ ] **Step 4: Rewrite `_dag_chain` as the semantic code chain**

```bash
# _dag_chain builds a chain of real Python modules: step i's run() calls
# step i-1's, so the dependency is semantic -- a child claimed before its
# blocker merged cannot pass its own tests (the imported module is absent).
_dag_chain() {
  local i file prev tag num expected="x" one
  one="Run \`uv run pytest\` from the repo root and make it pass before committing. Do not add dependencies and do not touch \`pyproject.toml\` or CI files."
  for i in $(seq 1 "$N"); do
    num="$(printf '%02d' "$i")"
    tag="step-$num"
    expected="${tag}:${expected}"
    file="src/greet/steps/step_${num}.py"
    T_TITLE[i]="[smoke] chain step $i"
    T_FILE[i]="$file,tests/test_step_${num}.py"
    T_TAGS[i]=""
    if [[ "$i" -eq 1 ]]; then
      T_DESC[i]="Create the package directory \`src/greet/steps/\` with an empty \`__init__.py\`, and create \`$file\` containing exactly one public function \`def run(value: str) -> str\` that returns the string \`step-01:\` followed by \`value\` (so \`run(\"x\")\` returns \`\"step-01:x\"\`). Also create \`tests/test_step_01.py\` with a pytest test asserting exactly that. Modify ONLY those files. $one"
      T_DEPS[i]=""
    else
      prev="$(printf '%02d' $((i - 1)))"
      T_DESC[i]="Create \`$file\` containing exactly one public function \`def run(value: str) -> str\`. It must call \`run\` from \`greet.steps.step_${prev}\` (the previous step) and prepend its own tag, so \`run(\"x\")\` returns exactly \`\"${expected}\"\`. Also create \`tests/test_step_${num}.py\` with a pytest test asserting \`run(\"x\") == \"${expected}\"\`. Modify ONLY those two files. $one"
      T_DEPS[i]="$((i - 1))"
    fi
  done
}
```

- [ ] **Step 5: Manifest v2 — rewrite `write_manifest`, `manifest_ids`; add `manifest_mode`**

```bash
# write_manifest records the seeded DAG for the run/verify phases. Line 1 is
# a "#mode=app|chain" header; data rows are
#   index<TAB>identifier<TAB>files<TAB>blocker-identifiers<TAB>tags
# Readers split on TAB only (IFS=$'\t'), so the space-separated blocker and
# tag lists survive in any column.
write_manifest() {
  mkdir -p "$SMOKE_HOME"
  printf '#mode=%s\n' "$MODE" > "$MANIFEST"
  local i d blockers
  for i in $(seq 1 "$N"); do
    blockers=""
    for d in ${T_DEPS[i]:-}; do
      if [[ -n "$blockers" ]]; then blockers="$blockers ${IDS[d]}"; else blockers="${IDS[d]}"; fi
    done
    printf '%s\t%s\t%s\t%s\t%s\n' "$i" "${IDS[i]}" "${T_FILE[i]}" "$blockers" "${T_TAGS[i]:-}" >> "$MANIFEST"
  done
  info "wrote manifest: $MANIFEST"
}

# manifest_ids prints the seeded identifiers, one per line (skips comments).
manifest_ids() {
  [[ -f "$MANIFEST" ]] || die "no manifest at $MANIFEST -- run 'seed' first"
  grep -v '^#' "$MANIFEST" | cut -f2
}

# manifest_mode prints the manifest's recorded mode (default: app).
manifest_mode() {
  local m=""
  [[ -f "$MANIFEST" ]] && m="$(sed -n 's/^#mode=//p' "$MANIFEST" | head -1)"
  printf '%s' "${m:-app}"
}
```

- [ ] **Step 6: Update every existing manifest reader for 5 columns + comment lines.** In `verify()` there are two `while IFS=$'\t' read -r … done < "$MANIFEST"` loops. Change both:

```bash
# old (both loops)
while IFS=$'\t' read -r _ id _ blockers; do
  ...
done < "$MANIFEST"

# new (both loops): consume the 5th field; skip the header
while IFS=$'\t' read -r _ id _ blockers _tags; do
  ...
done < <(grep -v '^#' "$MANIFEST")
```

(Without the extra `_tags` var, `read` would glue the tags column onto `blockers`.)

- [ ] **Step 7: Verify**

Run: `bash -n scripts/smoke/smoke.sh && grep -n 'done < "\$MANIFEST"' scripts/smoke/smoke.sh`
Expected: syntax OK; no remaining raw `< "$MANIFEST"` reads (both now via `grep -v '^#'`).

- [ ] **Step 8: Commit**

```bash
git add scripts/smoke/smoke.sh
git commit -m "feat(smoke): mode plumbing, semantic code chain, manifest v2"
```

---

### Task 5: App-mode seeding from `dag/`; header + usage text

**Files:**
- Modify: `scripts/smoke/smoke.sh` (add `_dag_app` + `DAG_DIR`; replace `_dag_greet` lines 504–548; update the header comment block lines 14–35)

**Interfaces:**
- Consumes: Task 2's `dag/manifest.tsv` + `TNN.md`; Task 4's `MODE`/`T_TAGS`.
- Produces: `_dag_app()` filling the same parallel arrays `_dag_chain` fills; `seed()` itself is untouched (it already writes `T_DESC` to a temp file and passes `--description-file`).

- [ ] **Step 1: Add `DAG_DIR` next to `BASELINE_DIR` in `load_env`**

```bash
DAG_DIR="$SCRIPT_DIR/dag"
```

- [ ] **Step 2: Delete `_dag_greet` entirely; add `_dag_app`**

```bash
# _dag_app loads the curated 10-ticket greet app DAG from scripts/smoke/dag/:
# manifest.tsv (idx / title / files / deps / tags, tab-separated) plus one
# TNN.md spec file per ticket (the verbatim Linear issue body).
_dag_app() {
  local manifest="$DAG_DIR/manifest.tsv"
  [[ -f "$manifest" ]] || die "missing app DAG manifest: $manifest"
  N=0
  local idx title files deps tags spec
  while IFS=$'\t' read -r idx title files deps tags; do
    [[ -n "$idx" && "$idx" != \#* ]] || continue
    spec="$DAG_DIR/$(printf 'T%02d.md' "$idx")"
    [[ -f "$spec" ]] || die "missing ticket spec: $spec"
    T_TITLE[idx]="$title"
    T_FILE[idx]="$files"
    T_DESC[idx]="$(cat "$spec")"
    T_DEPS[idx]="$deps"
    T_TAGS[idx]="$tags"
    N="$idx"
  done < "$manifest"
  [[ "$N" -gt 0 ]] || die "no tickets found in $manifest"
}
```

- [ ] **Step 3: Update the header comment block** (it feeds `usage()`; keep the `#`-comment format). Replace the `seed` line and the `--tickets`/`--fast` flag lines:

```text
#   seed      create the issue DAG on Linear. Default: the 10-ticket greet
#             app DAG (real Python modules + tests, incl. a deliberate
#             README merge-conflict pair). --fast / --tickets N seed a
#             semantic code chain instead.
...
#   --tickets N   seed an N-step code chain (step i imports step i-1, so
#                 dependency order is enforced by the tests themselves).
#   --fast        3-step code chain (~10 min). Overrides --tickets.
```

- [ ] **Step 4: Verify**

Run: `bash -n scripts/smoke/smoke.sh && ./scripts/smoke/smoke.sh --help | head -25 && grep -c '_dag_greet' scripts/smoke/smoke.sh`
Expected: syntax OK; usage shows the new text; `0` `_dag_greet` references.

- [ ] **Step 5: Commit**

```bash
git add scripts/smoke/smoke.sh
git commit -m "feat(smoke): seed app dag from data files"
```

---

### Task 6: `generate_config` cleanup + passthroughs; new defaults

**Files:**
- Modify: `scripts/smoke/smoke.sh` (`load_env` defaults lines 107–122; `generate_config` lines 181–233)

**Interfaces:**
- Consumes: nothing new.
- Produces: generated yaml without `scribe`; optional `models:` block from `MODEL_CODER`/`MODEL_CODER_DOCS`/`MODEL_REVIEWER`; optional verbatim append from `EXTRA_CONFIG_YAML` (path). All unset → config semantically identical to today.

- [ ] **Step 1: Update `load_env` defaults**

```bash
# old
: "${MAX_RUNTIME_S:=900}"
...
: "${TIMEOUT_S:=3600}"
...
: "${CAP_SCRIBE:=2}"

# new: code turns run longer than markdown turns
: "${MAX_RUNTIME_S:=1800}"
...
: "${TIMEOUT_S:=5400}"
# CAP_SCRIBE deleted (the scribe lane no longer exists)

# new optional passthroughs (empty = omitted from the generated config)
: "${MODEL_CODER:=}"
: "${MODEL_CODER_DOCS:=}"
: "${MODEL_REVIEWER:=}"
: "${EXTRA_CONFIG_YAML:=}"
```

- [ ] **Step 2: In `generate_config`'s heredoc, delete the `scribe: $CAP_SCRIBE` line from `per_lane`.**

- [ ] **Step 3: Append the optional blocks after the heredoc** (inside `generate_config`, before the final `info`; note the `if` form — see Global Constraints re `set -e`):

```bash
  if [[ -n "$MODEL_CODER$MODEL_CODER_DOCS$MODEL_REVIEWER" ]]; then
    {
      printf '\n# per-lane model overrides (from smoke.env MODEL_* vars)\n'
      printf 'models:\n'
      if [[ -n "$MODEL_CODER" ]];      then printf '  coder: "%s"\n' "$MODEL_CODER"; fi
      if [[ -n "$MODEL_CODER_DOCS" ]]; then printf '  coder_docs: "%s"\n' "$MODEL_CODER_DOCS"; fi
      if [[ -n "$MODEL_REVIEWER" ]];   then printf '  reviewer: "%s"\n' "$MODEL_REVIEWER"; fi
    } >> "$SMOKE_YAML"
  fi

  if [[ -n "$EXTRA_CONFIG_YAML" ]]; then
    [[ -f "$EXTRA_CONFIG_YAML" ]] || die "EXTRA_CONFIG_YAML is set but not a file: $EXTRA_CONFIG_YAML"
    {
      printf '\n# --- appended verbatim from EXTRA_CONFIG_YAML (%s) ---\n' "$EXTRA_CONFIG_YAML"
      cat "$EXTRA_CONFIG_YAML"
    } >> "$SMOKE_YAML"
  fi
```

- [ ] **Step 4: Verify against the real config loader**

Run:

```bash
bash -n scripts/smoke/smoke.sh \
  && ./scripts/smoke/smoke.sh generate \
  && grep -c scribe ~/Code/clipse-smoke/clipse.smoke.yaml \
  && MODEL_CODER="anthropic:claude-sonnet-4-6" ./scripts/smoke/smoke.sh generate \
  && grep -A2 '^models:' ~/Code/clipse-smoke/clipse.smoke.yaml
```

Expected: `0` scribe lines; second generate shows the `models:` block with the coder override. (Path is `$SMOKE_HOME/clipse.smoke.yaml`; adjust if your `smoke.env` overrides `SMOKE_HOME`.)

- [ ] **Step 5: Commit**

```bash
git add scripts/smoke/smoke.sh
git commit -m "feat(smoke): model passthroughs, drop scribe, longer defaults"
```

---

### Task 7: `verify` v2 — transcripts, integration clone, conflict evidence

**Files:**
- Modify: `scripts/smoke/smoke.sh` (`verify()` lines 691–784; insert the new sections after the dependency-ordering block and before the `check-placement.py` block)

**Interfaces:**
- Consumes: `manifest_mode`, `manifest_ids`, 5-col manifest (Task 4); `BOARD_DIR`, `REMOTE_URL`, `db_scalar`.
- Produces: three new verify sections; sections 1–2 increment the existing `fails` counter, section 3 is report-only.

- [ ] **Step 1: Insert the transcript assertions (fatal)**

```bash
  # (c2) transcripts: every ticket must have a per-issue transcript with at
  # least one coder and one coder_docs turn_start (the docs sub-turn is part
  # of the coder graph's clean path).
  info "verify: per-ticket transcripts"
  local tf
  while IFS= read -r id; do
    [[ -n "$id" ]] || continue
    tf="$BOARD_DIR/logs/$id.transcript.jsonl"
    if [[ ! -f "$tf" ]]; then
      err "  $id: missing transcript $tf"
      fails=$((fails + 1))
      continue
    fi
    if ! jq -r 'select(.event=="turn_start") | .lane' "$tf" 2>/dev/null | grep -qx "coder"; then
      err "  $id: transcript has no coder turn_start"
      fails=$((fails + 1))
    fi
    if ! jq -r 'select(.event=="turn_start") | .lane' "$tf" 2>/dev/null | grep -qx "coder_docs"; then
      err "  $id: transcript has no coder_docs turn_start"
      fails=$((fails + 1))
    fi
  done < <(manifest_ids)
```

- [ ] **Step 2: Insert the integration-clone assertions (fatal)**

```bash
  # (d) integration: a fresh clone of merged main must actually work.
  info "verify: integration clone (pytest + CLI)"
  local itmp iclone out
  itmp="$(mktemp -d)"
  iclone="$itmp/clone"
  if git clone -q --depth 1 "$REMOTE_URL" "$iclone" 2>/dev/null; then
    if grep -rn '^<<<<<<< ' "$iclone" --exclude-dir=.git >/dev/null 2>&1; then
      err "  merged main contains conflict markers"
      fails=$((fails + 1))
    fi
    if (cd "$iclone" && uv run --quiet pytest -q >/dev/null 2>&1); then
      info "  pytest green on merged main"
    else
      err "  pytest failed on merged main:"
      (cd "$iclone" && uv run pytest -q 2>&1 | tail -n 20 >&2) || true
      fails=$((fails + 1))
    fi
    if [[ "$(manifest_mode)" == "app" ]]; then
      out="$(cd "$iclone" && uv run --quiet greet --name smoke --loud 2>/dev/null || true)"
      if [[ "$out" != "HELLO, SMOKE!" ]]; then
        err "  greet --name smoke --loud printed '$out', want 'HELLO, SMOKE!'"
        fails=$((fails + 1))
      fi
      out="$(cd "$iclone" && uv run --quiet greet --name smoke --locale es 2>/dev/null || true)"
      if [[ "$out" != "¡Hola, smoke!" ]]; then
        err "  greet --name smoke --locale es printed '$out', want '¡Hola, smoke!'"
        fails=$((fails + 1))
      fi
      if ! grep -q '^## Usage' "$iclone/README.md" 2>/dev/null; then
        err "  README missing '## Usage' section"
        fails=$((fails + 1))
      fi
      if ! grep -q '^## Examples' "$iclone/README.md" 2>/dev/null; then
        err "  README missing '## Examples' section"
        fails=$((fails + 1))
      fi
    fi
  else
    err "  could not clone $REMOTE_URL for the integration check"
    fails=$((fails + 1))
  fi
  rm -rf "$itmp"
```

- [ ] **Step 3: Insert the conflict-pair evidence (report-only)**

```bash
  # (e) conflict-pair evidence (report-only): did the README pair actually
  # exercise the stale-base -> rework -> sync_base path? Timing-dependent,
  # so it never affects the verdict -- the merged outcome is what (d) asserts.
  if [[ "$(manifest_mode)" == "app" ]]; then
    info "verify: conflict-pair evidence (report-only)"
    local cruns tags
    while IFS=$'\t' read -r _ id _ _ tags; do
      [[ "$tags" == *conflict-pair* ]] || continue
      cruns="$(db_scalar "SELECT count(*) FROM runs r JOIN issues i ON i.id=r.issue_id WHERE i.identifier='$id' AND r.lane='coder';")"
      if [[ "${cruns:-0}" -gt 1 ]]; then
        info "  $id: ${cruns} coder runs -- stale-base rework path fired"
      else
        info "  $id: ${cruns:-0} coder run(s) -- no conflict this run"
      fi
    done < <(grep -v '^#' "$MANIFEST")
  fi
```

- [ ] **Step 4: Declare the new locals** at the top of `verify()` alongside the existing declarations: `local tf itmp iclone out cruns tags` (drop any that would duplicate existing `local` names — `tags` may already exist from Task 4's reader rename; check).

- [ ] **Step 5: Verify**

Run: `bash -n scripts/smoke/smoke.sh` and, if installed, `shellcheck scripts/smoke/smoke.sh` (warnings about sourced smoke.env are pre-existing and fine).
Expected: clean syntax; no new shellcheck errors.

- [ ] **Step 6: Commit**

```bash
git add scripts/smoke/smoke.sh
git commit -m "feat(smoke): verify transcripts, integration clone, conflict evidence"
```

---

### Task 8: Docs — README, smoke.env.example, example yaml

**Files:**
- Modify: `scripts/smoke/README.md` (rewrite the stale sections)
- Modify: `scripts/smoke/smoke.env.example`
- Modify: `configs/clipse.smoke.example.yaml`

**Interfaces:** none (docs only).

- [ ] **Step 1: Update `scripts/smoke/README.md`.** Keep the overall structure; rewrite these parts:

- "What it exercises": replace the pipeline line (`documentation` column no longer exists) and the greet-DAG description with: the 10-ticket **greet app DAG** (real Python modules + pytest tests assembled into a working CLI), the **README conflict pair** (T8/T9 exercising `sync_base` stale-base merge + coder conflict-marker resolution), **semantic code chains** for `--fast`/`--tickets N` (step i imports step i−1), per-ticket **transcript** assertions, and the final **integration clone** (pytest + pinned CLI outputs).
- "What pass means": add assertions 4 (transcripts: coder + coder_docs turn_start per ticket) and 5 (integration clone: pytest green, no conflict markers, app mode also exact CLI outputs + both README sections); note the conflict-evidence line is report-only.
- "Cost & time notes": real code + tests per ticket costs more than one markdown file; `--fast` is the cheap check; new defaults `MAX_RUNTIME_S=1800`, `TIMEOUT_S=5400`.
- Add a "Migration from the markdown smoke (pre-2026-07-07)" section: the baseline changed and `BASELINE_TAG` now defaults to `smoke-baseline-py` — re-run `./scripts/smoke/smoke.sh setup` once after pulling (it sees the tag missing and force-pushes the new baseline; the old `smoke-baseline` tag is left behind, harmless).
- "Files": add `baseline/` (the pushed target-project baseline) and `dag/` (manifest + ticket specs).

- [ ] **Step 2: Update `scripts/smoke/smoke.env.example`:** delete `CAP_SCRIBE`; change the documented defaults for `MAX_RUNTIME_S` (1800) and `TIMEOUT_S` (5400); add a commented block for the new knobs:

```bash
# --- optional per-lane model overrides (omit to use clipse defaults) ---
# MODEL_CODER="anthropic:claude-sonnet-4-6"
# MODEL_CODER_DOCS="anthropic:claude-sonnet-4-6"
# MODEL_REVIEWER="anthropic:claude-opus-4-6"

# Path to a yaml snippet appended verbatim to the generated config -- use
# for nested blocks like model_params: / shell_allow_list:.
# EXTRA_CONFIG_YAML="$HOME/Code/clipse-smoke/extra.yaml"
```

- [ ] **Step 3: Update `configs/clipse.smoke.example.yaml`:** remove the `scribe:` cap line; update `max_runtime_s` to 1800; add a commented `models:` / `model_params:` example mirroring the smoke.env knobs.

- [ ] **Step 4: Verify**

Run: `grep -rn 'scribe\|smoke-baseline"' scripts/smoke/README.md scripts/smoke/smoke.env.example configs/clipse.smoke.example.yaml || echo docs-clean`
Expected: `docs-clean`.

- [ ] **Step 5: Commit**

```bash
git add scripts/smoke/README.md scripts/smoke/smoke.env.example configs/clipse.smoke.example.yaml
git commit -m "docs(smoke): rewrite for real-project dag, new knobs and defaults"
```

---

### Task 9: Final validation

**Files:** none (validation only).

- [ ] **Step 1: Static checks**

```bash
bash -n scripts/smoke/smoke.sh
command -v shellcheck >/dev/null && shellcheck scripts/smoke/smoke.sh || true
make -C . lint
make -C . test
```

Expected: all pass (`make test` proves no kernel regression — this work should not have touched Go/Python, this is the guard).

- [ ] **Step 2: Re-verify the baseline in a temp copy** (same command as Task 1 Step 2). Expected: `1 passed`.

- [ ] **Step 3: One-time setup re-run against the real target repo**

```bash
./scripts/smoke/smoke.sh setup
```

Expected: creates/keeps the repo, pushes the new `smoke-baseline-py` baseline (tag was absent), confirms protection.

- [ ] **Step 4: Live fast smoke (costs tokens — get user go-ahead first)**

```bash
./scripts/smoke/smoke.sh --fast --keep
```

Expected: `SMOKE PASS`; transcript + integration sections green; chain step tests prove the semantic ordering.

- [ ] **Step 5: Live full smoke (costs more tokens — user go-ahead)**

```bash
./scripts/smoke/smoke.sh --keep
```

Expected: `SMOKE PASS`; conflict-evidence line reports the stale-base rework fired on one of the README pair (expected in practice, not asserted).
