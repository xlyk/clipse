#!/usr/bin/env bash
#
# clipse clean-slate smoke test.
#
# Drives the whole clipse pipeline against real external systems -- a Linear
# team, a throwaway GitHub repo, the clipse binary, and its SQLite board --
# and asserts that a dependency DAG of Linear issues turns into merged PRs in
# the right order. This is pure external orchestration; it never touches the
# Go/Python kernel internals.
#
# Everything machine-specific comes from scripts/smoke/smoke.env (copy it from
# smoke.env.example). This script writes no secrets and hard-codes no paths.
#
# Subcommands (default is the full pipeline):
#   setup     one-time: create the throwaway target repo, push a baseline
#             commit + CI workflow, apply branch protection. Idempotent.
#   reset     clean slate: delete smoke Linear issues, close PRs + delete
#             branches, force target main back to baseline, wipe the board.
#   build     compile a fresh ./bin/clipse.
#   seed      create the issue DAG on Linear. Default: the 10-ticket greet
#             app DAG (real Python modules + tests, incl. a deliberate
#             README merge-conflict pair). --fast / --tickets N seed a
#             semantic code chain instead.
#   run       launch the dispatcher and poll the board until terminal.
#   verify    assert every seeded ticket is done, order held, PRs merged.
#   (none)    reset -> build -> seed -> run -> verify -> teardown.
#
# Flags:
#   --tickets N   seed an N-step code chain (step i imports step i-1, so
#                 dependency order is enforced by the tests themselves).
#   --fast        3-step code chain (~10 min). Overrides --tickets.
#   --no-run      stop after seed (do not launch the dispatcher).
#   --keep        keep generated artifacts + board after the run for
#                 `clipse tui` / `status` inspection.
#
# The dispatcher is always stopped on exit; a smoke run never leaves a daemon
# running. It does NOT delete the seeded Linear issues / merged PRs on exit --
# they are left for inspection and cleared by the next `reset`.

set -euo pipefail

# ---------------------------------------------------------------------------
# Paths & globals
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
ENV_FILE="$SCRIPT_DIR/smoke.env"

# Populated by build_dag(): parallel 1-indexed arrays (index 0 is a "_"
# placeholder so ticket numbers read naturally). DEPS holds space-separated
# blocker indices; IDS is filled with the created CLI-N identifiers.
T_TITLE=() ; T_FILE=() ; T_DESC=() ; T_DEPS=() ; T_TAGS=() ; IDS=()
N=0
MODE=""

# Set when the dispatcher is launched so the EXIT trap can stop it.
DISPATCH_PID=""

# Flags (defaults).
TICKETS=10
TICKETS_SET=0
FAST=0
NO_RUN=0
KEEP=0

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

info()  { printf '\033[0;36m[smoke]\033[0m %s\n' "$*"; }
warn()  { printf '\033[0;33m[smoke] WARN:\033[0m %s\n' "$*" >&2; }
err()   { printf '\033[0;31m[smoke] ERROR:\033[0m %s\n' "$*" >&2; }
die()   { err "$*"; exit 1; }

banner_pass() { printf '\n\033[1;32m========== SMOKE PASS ==========\033[0m\n'; }
banner_fail() { printf '\n\033[1;31m========== SMOKE FAIL ==========\033[0m\n'; }

# ---------------------------------------------------------------------------
# Environment
# ---------------------------------------------------------------------------

# load_env sources smoke.env (required) then applies defaults for every
# tunable so the rest of the script can rely on them being set (this also
# keeps shellcheck happy about "unassigned" variables).
load_env() {
  [[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE -- copy scripts/smoke/smoke.env.example to it and fill in your values"
  set +u
  # shellcheck source=/dev/null
  . "$ENV_FILE"
  set -u

  : "${TARGET_REPO:=xlyk/clipse-smoke-target}"
  : "${REPO_VISIBILITY:=private}"
  : "${REMOTE_URL:=git@github.com:${TARGET_REPO}.git}"
  : "${BASE_BRANCH:=main}"
  : "${BASELINE_TAG:=smoke-baseline-py}"

  : "${TEAM_KEY:=CLI}"
  : "${TEAM_ID:=8b5b3301-8da3-4933-9b07-9efc027bc09d}"

  : "${SMOKE_HOME:=$HOME/Code/clipse-smoke}"
  : "${BOARD_DIR:=$SMOKE_HOME/board}"
  : "${CHECKPOINTS_DIR:=$SMOKE_HOME/checkpoints}"
  : "${PRIMARY_CLONE:=$SMOKE_HOME/primary}"
  : "${SMOKE_YAML:=$SMOKE_HOME/clipse.smoke.yaml}"
  : "${CLIPSE_REPO:=$REPO_ROOT}"
  BASELINE_DIR="$SCRIPT_DIR/baseline"
  DAG_DIR="$SCRIPT_DIR/dag"

  : "${LINEAR_KEY_SOURCE:=$HOME/.secrets}"
  : "${ANTHROPIC_KEY_SOURCE:=env}"

  : "${POLL_INTERVAL_S:=20}"
  # code turns run longer than markdown turns
  : "${MAX_RUNTIME_S:=1800}"
  : "${MAX_TOKENS_PER_RUN:=2000000}"
  : "${TURN_CAP:=3}"
  : "${MAX_ATTEMPTS:=3}"
  : "${REWORK_CAP:=3}"
  : "${RECOVER_CAP:=5}"

  : "${TIMEOUT_S:=5400}"
  : "${WATCH_INTERVAL_S:=15}"

  : "${CAP_GLOBAL:=6}"
  : "${CAP_CODER:=3}"
  : "${CAP_REVIEWER:=2}"
  : "${CAP_GIT_OPERATOR:=2}"

  # optional per-lane model overrides / verbatim config append (empty =
  # omitted from the generated config)
  : "${MODEL_CODER:=}"
  : "${MODEL_CODER_DOCS:=}"
  : "${MODEL_REVIEWER:=}"
  : "${EXTRA_CONFIG_YAML:=}"

  DB="$BOARD_DIR/clipse.db"
  DISPATCH_LOG="$SMOKE_HOME/dispatch.log"
  MANIFEST="$SMOKE_HOME/smoke-manifest.tsv"
  CLIPSE_BIN="$CLIPSE_REPO/bin/clipse"
}

# source_key exports the named key from its configured source. SOURCE is
# either the literal "env" (already exported) or a path to a file to source.
# Never echoes the value.
source_key() {
  local name="$1" src="$2"
  if [[ "$src" == "env" ]]; then
    return 0
  fi
  [[ -f "$src" ]] || die "$name source file not found: $src (set ${name}_SOURCE=env if it is already exported)"
  set +u
  # shellcheck source=/dev/null
  . "$src"
  set -u
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------

preflight() {
  info "preflight: checking tools and credentials"

  local missing=0 tool
  for tool in linear gh sqlite3 uv git make jq; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      err "required tool not found on PATH: $tool"
      missing=1
    fi
  done
  [[ "$missing" -eq 0 ]] || die "install the missing tools above and retry"

  source_key LINEAR_API_KEY "$LINEAR_KEY_SOURCE"
  source_key ANTHROPIC_API_KEY "$ANTHROPIC_KEY_SOURCE"

  [[ -n "${LINEAR_API_KEY:-}" ]] || die "LINEAR_API_KEY is not set (LINEAR_KEY_SOURCE=$LINEAR_KEY_SOURCE)"
  [[ -n "${ANTHROPIC_API_KEY:-}" ]] || die "ANTHROPIC_API_KEY is not set (ANTHROPIC_KEY_SOURCE=$ANTHROPIC_KEY_SOURCE)"
  export LINEAR_API_KEY ANTHROPIC_API_KEY

  if ! gh auth status >/dev/null 2>&1; then
    die "gh is not authenticated -- run 'gh auth login'"
  fi

  info "preflight ok (target repo=$TARGET_REPO team=$TEAM_KEY board=$BOARD_DIR)"
}

# ---------------------------------------------------------------------------
# Generated dispatcher config
# ---------------------------------------------------------------------------

# generate_config writes SMOKE_YAML from the smoke.env values. Regenerated on
# every run so the config can never drift from smoke.env. Gitignored.
generate_config() {
  mkdir -p "$(dirname "$SMOKE_YAML")"
  cat > "$SMOKE_YAML" <<YAML
# GENERATED by scripts/smoke/smoke.sh from smoke.env. Do not edit -- it is
# overwritten on every run. Machine-specific; gitignored.
repo:
  remote: "$REMOTE_URL"
  path: "$PRIMARY_CLONE"
  base_branch: "$BASE_BRANCH"

team_key: "$TEAM_KEY"
team_id: "$TEAM_ID"

poll_interval_s: $POLL_INTERVAL_S

caps:
  global: $CAP_GLOBAL
  per_lane:
    coder: $CAP_CODER
    reviewer: $CAP_REVIEWER
    git_operator: $CAP_GIT_OPERATOR

turn_cap: $TURN_CAP
max_runtime_s: $MAX_RUNTIME_S
max_tokens_per_run: $MAX_TOKENS_PER_RUN
lane_label_prefix: "agent:"
max_attempts: $MAX_ATTEMPTS
rework_cap: $REWORK_CAP
recover_cap: $RECOVER_CAP

worker:
  command:
    - uv
    - --project
    - "$CLIPSE_REPO/agent"
    - run
    - clipse-worker

checkpoints_dir: "$CHECKPOINTS_DIR"
board_dir: "$BOARD_DIR"

# LINEAR_API_KEY is deliberately absent: it is the kernel-only credential and
# must never reach a worker.
env_allowlist:
  - ANTHROPIC_API_KEY
  - PATH
  - HOME
  - GH_TOKEN
  - GITHUB_TOKEN
YAML

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

  info "wrote dispatcher config: $SMOKE_YAML"
}

# ---------------------------------------------------------------------------
# Setup (one-time, idempotent)
# ---------------------------------------------------------------------------

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

# apply_branch_protection PUTs classic branch protection matching clipse's
# gitops gate: strict required status checks (contexts go/python/codegen-drift),
# 0 required approvals, admins not enforced, force-push allowed (so reset can
# force main back to the baseline). Idempotent (PUT sets desired state).
apply_branch_protection() {
  info "applying branch protection on $TARGET_REPO@$BASE_BRANCH"
  gh api --method PUT "repos/$TARGET_REPO/branches/$BASE_BRANCH/protection" --input - >/dev/null <<'JSON'
{
  "required_status_checks": { "strict": true, "contexts": ["go", "python", "codegen-drift"] },
  "enforce_admins": false,
  "required_pull_request_reviews": null,
  "restrictions": null,
  "allow_force_pushes": true,
  "allow_deletions": false
}
JSON
  # Confirm it actually took: on some plans branch protection is unavailable
  # for private repos, which would silently break the merge gate.
  if ! gh api "repos/$TARGET_REPO/branches/$BASE_BRANCH/protection" >/dev/null 2>&1; then
    die "branch protection did not apply to $TARGET_REPO@$BASE_BRANCH -- your plan may not allow protection on a $REPO_VISIBILITY repo. Set REPO_VISIBILITY=public in smoke.env and re-run setup."
  fi
  info "branch protection confirmed"
}

setup() {
  info "setup: target repo $TARGET_REPO"

  if gh repo view "$TARGET_REPO" >/dev/null 2>&1; then
    info "repo already exists"
  else
    info "creating $REPO_VISIBILITY repo $TARGET_REPO"
    gh repo create "$TARGET_REPO" "--$REPO_VISIBILITY" \
      --description "throwaway target for the clipse smoke test" >/dev/null
  fi

  if gh api "repos/$TARGET_REPO/git/ref/tags/$BASELINE_TAG" >/dev/null 2>&1; then
    info "baseline tag $BASELINE_TAG already present -- skipping baseline push"
  else
    info "pushing baseline project + $BASELINE_TAG tag"
    push_baseline
  fi

  apply_branch_protection
  info "setup complete"
}

# ---------------------------------------------------------------------------
# Reset (clean slate)
# ---------------------------------------------------------------------------

# smoke_issue_ids prints the CLI-N identifiers of every smoke issue on the
# team, one per line. The marker is the "[smoke]" title prefix (see seed()).
# jq walks the response recursively so it tolerates whatever envelope the
# linear CLI wraps the GraphQL payload in.
smoke_issue_ids() {
  local q
  q="$(printf 'query { team(id: "%s") { issues(first: 250) { nodes { identifier title } } } }' "$TEAM_ID")"
  linear api "$q" 2>/dev/null \
    | jq -r '[.. | objects | select(has("identifier") and has("title"))] | .[] | select(.title | startswith("[smoke]")) | .identifier' 2>/dev/null \
    | sort -u
}

reset_linear() {
  info "reset: deleting smoke Linear issues (marker: '[smoke]' title prefix)"
  local id count=0
  # Read into a list first; deleting mutates the set we would otherwise page.
  local ids
  ids="$(smoke_issue_ids || true)"
  if [[ -z "$ids" ]]; then
    info "  no smoke issues found"
    return 0
  fi
  for id in $ids; do
    if linear issue delete "$id" -y >/dev/null 2>&1; then
      count=$((count + 1))
    else
      warn "  failed to delete $id (continuing)"
    fi
  done
  info "  deleted $count smoke issue(s)"
}

reset_github() {
  info "reset: closing open PRs and deleting branches on $TARGET_REPO"

  local n
  gh pr list --repo "$TARGET_REPO" --state open --json number --jq '.[].number' 2>/dev/null \
    | while read -r n; do
        [[ -n "$n" ]] || continue
        gh pr close "$n" --repo "$TARGET_REPO" --delete-branch >/dev/null 2>&1 || true
      done

  # Force main back to the baseline commit (resolved from the baseline tag).
  local sha
  sha="$(gh api "repos/$TARGET_REPO/commits/$BASELINE_TAG" --jq .sha 2>/dev/null || true)"
  if [[ -n "$sha" ]]; then
    info "  forcing $BASE_BRANCH -> $BASELINE_TAG ($sha)"
    gh api --method PATCH "repos/$TARGET_REPO/git/refs/heads/$BASE_BRANCH" \
      -f sha="$sha" -F force=true >/dev/null 2>&1 \
      || warn "  could not force $BASE_BRANCH to baseline (continuing)"
  else
    warn "  baseline tag $BASELINE_TAG not found -- run 'smoke.sh setup' first"
  fi

  # Delete every non-baseline branch (catches merged + abandoned PR heads).
  local b
  gh api "repos/$TARGET_REPO/branches" --paginate --jq '.[].name' 2>/dev/null \
    | while read -r b; do
        [[ -n "$b" && "$b" != "$BASE_BRANCH" ]] || continue
        gh api --method DELETE "repos/$TARGET_REPO/git/refs/heads/$b" >/dev/null 2>&1 || true
      done
  info "  github reset done"
}

reset_local() {
  info "reset: wiping local board and re-cloning the primary"

  # Stop any dispatcher still holding the board lock (e.g. after a crash).
  if [[ -f "$BOARD_DIR/clipse.lock" ]]; then
    local pid
    pid="$(head -n1 "$BOARD_DIR/clipse.lock" 2>/dev/null | tr -dc '0-9' || true)"
    if [[ -n "$pid" ]] && kill -0 "$pid" >/dev/null 2>&1; then
      warn "  a dispatcher (pid $pid) still holds the board lock -- stopping it"
      kill -TERM "$pid" >/dev/null 2>&1 || true
      sleep 2
    fi
  fi

  rm -rf "$BOARD_DIR" "$CHECKPOINTS_DIR"
  mkdir -p "$BOARD_DIR" "$CHECKPOINTS_DIR" "$SMOKE_HOME"

  if ! gh repo view "$TARGET_REPO" >/dev/null 2>&1; then
    die "target repo $TARGET_REPO not found -- run './scripts/smoke/smoke.sh setup' first"
  fi

  rm -rf "$PRIMARY_CLONE"
  mkdir -p "$(dirname "$PRIMARY_CLONE")"
  info "  cloning $TARGET_REPO -> $PRIMARY_CLONE"
  git clone -q "$REMOTE_URL" "$PRIMARY_CLONE"
  info "  local reset done"
}

reset() {
  reset_linear
  reset_github
  reset_local
  info "reset complete -- clean slate"
}

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

build() {
  info "build: make build (fresh binary)"
  make -C "$CLIPSE_REPO" build >/dev/null
  [[ -x "$CLIPSE_BIN" ]] || die "expected binary not found after build: $CLIPSE_BIN"
  info "  built $CLIPSE_BIN"
}

# ---------------------------------------------------------------------------
# Seed
# ---------------------------------------------------------------------------

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

seed() {
  build_dag
  info "seed: creating $N ticket(s) on team $TEAM_KEY"

  local i url id desc_file
  for i in $(seq 1 "$N"); do
    desc_file="$(mktemp)"
    printf '%s\n' "${T_DESC[i]}" > "$desc_file"
    url="$(linear issue create --team "$TEAM_KEY" \
            --title "${T_TITLE[i]}" \
            --description-file "$desc_file" \
            --label "agent:coder" \
            --state "Todo" \
            --no-interactive 2>&1 | grep -oE 'https://linear.app/[^ ]+' | tail -1 || true)"
    rm -f "$desc_file"
    id="$(printf '%s' "$url" | grep -oE 'CLI-[0-9]+' | head -1 || true)"
    [[ -n "$id" ]] || die "failed to create ticket T$i (${T_TITLE[i]}) -- linear output: $url"
    IDS[i]="$id"
    info "  T$i -> $id  ${T_TITLE[i]} (${T_FILE[i]})"
  done

  info "seed: wiring blocked-by relations"
  local d
  for i in $(seq 1 "$N"); do
    for d in ${T_DEPS[i]:-}; do
      info "  ${IDS[i]} blocked-by ${IDS[d]}"
      linear issue relation add "${IDS[i]}" blocked-by "${IDS[d]}" >/dev/null 2>&1 \
        || warn "  failed to wire ${IDS[i]} blocked-by ${IDS[d]}"
    done
  done

  write_manifest
  info "seed complete: $(printf '%s ' "${IDS[@]:1}")"
}

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

# sql_in_list prints a quoted, comma-separated SQL IN(...) body of the seeded
# identifiers, e.g. 'CLI-8','CLI-9'.
sql_in_list() {
  local id out=""
  while IFS= read -r id; do
    [[ -n "$id" ]] || continue
    if [[ -n "$out" ]]; then out="$out,'$id'"; else out="'$id'"; fi
  done < <(manifest_ids)
  printf '%s' "$out"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

db_scalar() { sqlite3 "$DB" "$1" 2>/dev/null || true; }

run() {
  [[ -x "$CLIPSE_BIN" ]] || die "no clipse binary at $CLIPSE_BIN -- run 'build' first"
  [[ -f "$SMOKE_YAML" ]] || die "no dispatcher config at $SMOKE_YAML -- run without a subcommand, or 'generate' first"

  local in_list
  in_list="$(sql_in_list)"
  [[ -n "$in_list" ]] || die "manifest has no seeded tickets"

  local total
  total="$(manifest_ids | grep -c .)"

  info "run: launching dispatcher (log: $DISPATCH_LOG)"
  : > "$DISPATCH_LOG"
  "$CLIPSE_BIN" dispatch --config "$SMOKE_YAML" >> "$DISPATCH_LOG" 2>&1 &
  DISPATCH_PID="$!"
  info "  dispatcher pid $DISPATCH_PID"

  local start now elapsed term counts
  start="$(date +%s)"
  while true; do
    now="$(date +%s)"
    elapsed=$((now - start))

    if ! kill -0 "$DISPATCH_PID" >/dev/null 2>&1; then
      err "dispatcher exited early (see $DISPATCH_LOG) -- verify will report the incomplete board"
      tail -n 20 "$DISPATCH_LOG" >&2 || true
      DISPATCH_PID=""
      break
    fi

    counts="$(db_scalar "SELECT board_status||'='||count(*) FROM issues WHERE identifier IN ($in_list) GROUP BY board_status ORDER BY board_status;" | tr '\n' ' ')"
    term="$(db_scalar "SELECT count(*) FROM issues WHERE identifier IN ($in_list) AND board_status IN ('done','blocked','cancelled');")"
    term="${term:-0}"
    printf '\033[0;36m[watch]\033[0m +%4ds  %-60s | terminal %s/%s\n' "$elapsed" "${counts:-<no board yet>}" "$term" "$total"

    if [[ "$term" -ge "$total" ]]; then
      info "run: all $total ticket(s) terminal after ${elapsed}s"
      break
    fi
    if [[ "$elapsed" -ge "$TIMEOUT_S" ]]; then
      warn "run: timeout after ${elapsed}s (limit ${TIMEOUT_S}s) -- $term/$total terminal"
      break
    fi
    sleep "$WATCH_INTERVAL_S"
  done

  # Stop the daemon so verify reads a quiescent board.
  stop_dispatcher
}

# ---------------------------------------------------------------------------
# Verify
# ---------------------------------------------------------------------------

# is_merged reports (exit 0) whether a merged PR exists for an issue, matching
# on the exact head branch first, then the "CLI-N:" PR title prefix.
#   $1 merged-PR JSON (array of {title, headRefName})
#   $2 issue identifier (CLI-N)  $3 recorded branch_name (may be empty)
is_merged() {
  local json="$1" id="$2" br="$3"
  printf '%s' "$json" | jq -e --arg id "$id" --arg br "$br" \
    'any(.[]?; (($br != "") and (.headRefName == $br)) or (.title | test("^" + $id + "[: ]")))' \
    >/dev/null 2>&1
}

verify() {
  [[ -f "$DB" ]] || die "no board db at $DB -- nothing to verify"
  local in_list
  in_list="$(sql_in_list)"
  [[ -n "$in_list" ]] || die "manifest has no seeded tickets"

  local fails=0

  # Fetch all merged PRs once (title + head branch) for assertion (c). Each
  # issue is matched by its exact recorded branch_name first, then by the
  # "CLI-N:" PR title prefix as a fallback.
  local merged_json
  merged_json="$(gh pr list --repo "$TARGET_REPO" --state merged --limit 200 --json title,headRefName 2>/dev/null || echo '[]')"

  info "verify: per-ticket assertions"
  printf '%-10s  %-12s  %-7s  %-9s  %10s\n' "TICKET" "STATUS" "MERGED" "WALL(s)" "TOKENS"
  printf '%-10s  %-12s  %-7s  %-9s  %10s\n' "------" "------" "------" "-------" "------"

  local id blockers
  local status done_ts start_ts tokens wall merged_yn branch
  local total_tokens=0
  local tf itmp iclone out cruns tags markers
  # (a) + (c) + row rendering.
  while IFS=$'\t' read -r _ id _ blockers _tags; do
    [[ -n "$id" ]] || continue
    status="$(db_scalar "SELECT board_status FROM issues WHERE identifier='$id';")"
    done_ts="$(db_scalar "SELECT COALESCE(MAX(e.ts),0) FROM events e JOIN issues i ON i.id=e.issue_id WHERE i.identifier='$id';")"
    start_ts="$(db_scalar "SELECT COALESCE(MIN(r.started_at),0) FROM runs r JOIN issues i ON i.id=r.issue_id WHERE i.identifier='$id';")"
    tokens="$(db_scalar "SELECT COALESCE(SUM(r.tokens_in + r.tokens_out),0) FROM runs r JOIN issues i ON i.id=r.issue_id WHERE i.identifier='$id';")"
    branch="$(db_scalar "SELECT branch_name FROM issues WHERE identifier='$id';")"
    status="${status:-<none>}"; done_ts="${done_ts:-0}"; start_ts="${start_ts:-0}"; tokens="${tokens:-0}"

    if [[ "$start_ts" -gt 0 && "$done_ts" -ge "$start_ts" ]]; then
      wall=$((done_ts - start_ts))
    else
      wall=0
    fi
    total_tokens=$((total_tokens + tokens))

    if is_merged "$merged_json" "$id" "$branch"; then
      merged_yn="yes"
    else
      merged_yn="no"
    fi

    printf '%-10s  %-12s  %-7s  %-9s  %10s\n' "$id" "$status" "$merged_yn" "$wall" "$tokens"

    if [[ "$status" != "done" ]]; then
      err "  $id is '$status', want 'done'"
      fails=$((fails + 1))
    fi
    if [[ "$merged_yn" != "yes" ]]; then
      err "  $id has no merged PR"
      fails=$((fails + 1))
    fi
  done < <(grep -v '^#' "$MANIFEST")

  info "total tokens (in+out): $total_tokens"

  # (b) dependency order: for every edge child<-blocker, the blocker must have
  # reached done no later than the child (a child cannot start before its
  # blockers are done, so it necessarily finishes after them).
  info "verify: dependency ordering"
  local child_done blocker_done blocker
  while IFS=$'\t' read -r _ id _ blockers _tags; do
    [[ -n "$id" && -n "$blockers" ]] || continue
    child_done="$(db_scalar "SELECT COALESCE(MAX(e.ts),0) FROM events e JOIN issues i ON i.id=e.issue_id WHERE i.identifier='$id';")"
    child_done="${child_done:-0}"
    # blockers is space-separated; the loop body's IFS is the default, so
    # this word-splits cleanly (the tab-scoped IFS only applied to read).
    for blocker in $blockers; do
      blocker_done="$(db_scalar "SELECT COALESCE(MAX(e.ts),0) FROM events e JOIN issues i ON i.id=e.issue_id WHERE i.identifier='$blocker';")"
      blocker_done="${blocker_done:-0}"
      if [[ "$blocker_done" -eq 0 || "$child_done" -eq 0 || "$blocker_done" -gt "$child_done" ]]; then
        err "  order violation: $id (done@$child_done) finished before blocker $blocker (done@$blocker_done)"
        fails=$((fails + 1))
      fi
    done
  done < <(grep -v '^#' "$MANIFEST")

  # (c2) transcripts: every ticket must have a per-issue transcript with at
  # least one coder and one coder_docs turn_start (the docs sub-turn is part
  # of the coder graph's clean path).
  info "verify: per-ticket transcripts"
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

  # (d) integration: a fresh clone of merged main must actually work.
  info "verify: integration clone (pytest + CLI)"
  itmp="$(mktemp -d)" || die "mktemp -d failed for the integration clone"
  iclone="$itmp/clone"
  if git clone -q --depth 1 "$REMOTE_URL" "$iclone" 2>"$itmp/clone.err"; then
    markers="$(grep -rl '^<<<<<<< ' "$iclone" --exclude-dir=.git 2>/dev/null || true)"
    if [[ -n "$markers" ]]; then
      err "  merged main contains conflict markers in: ${markers//$'\n'/ }"
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
    sed 's/^/    /' "$itmp/clone.err" >&2 || true
    fails=$((fails + 1))
  fi
  rm -rf "$itmp"

  # (e) conflict-pair evidence (report-only): did the README pair actually
  # exercise the stale-base -> rework -> sync_base path? Timing-dependent,
  # so it never affects the verdict -- the merged outcome is what (d) asserts.
  if [[ "$(manifest_mode)" == "app" ]]; then
    info "verify: conflict-pair evidence (report-only)"
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

  # R5 (optional, report-only): reviewer inline-comment placement validity.
  # Never affects the smoke verdict; requires gh auth against $TARGET_REPO.
  if command -v python3 >/dev/null 2>&1; then
    info "verify: inline-comment placement (report-only)"
    python3 "$(dirname "${BASH_SOURCE[0]}")/check-placement.py" --repo "$TARGET_REPO" \
      || warn "placement check failed (non-fatal)"
  fi

  if [[ "$fails" -eq 0 ]]; then
    info "verify: all assertions passed"
    return 0
  fi
  err "verify: $fails assertion(s) failed"
  return 1
}

# ---------------------------------------------------------------------------
# Teardown
# ---------------------------------------------------------------------------

stop_dispatcher() {
  [[ -n "$DISPATCH_PID" ]] || return 0
  if kill -0 "$DISPATCH_PID" >/dev/null 2>&1; then
    info "stopping dispatcher (pid $DISPATCH_PID)"
    kill -TERM "$DISPATCH_PID" >/dev/null 2>&1 || true
    local i
    for i in $(seq 1 10); do
      kill -0 "$DISPATCH_PID" >/dev/null 2>&1 || break
      sleep 1
    done
    if kill -0 "$DISPATCH_PID" >/dev/null 2>&1; then
      warn "dispatcher did not stop gracefully -- sending KILL"
      kill -KILL "$DISPATCH_PID" >/dev/null 2>&1 || true
    fi
  fi
  DISPATCH_PID=""
}

# on_exit is the EXIT trap: it guarantees no dispatcher is left running.
on_exit() {
  stop_dispatcher
}

# report runs verify, prints the PASS/FAIL banner, and exits with the matching
# code (0 all-pass, 1 otherwise). `if verify` keeps set -e from aborting on a
# failing assertion so the FAIL banner is always reached.
report() {
  if verify; then
    banner_pass
    [[ "$KEEP" -eq 1 ]] && info "kept for inspection: $CLIPSE_BIN tui --board $BOARD_DIR"
    exit 0
  fi
  banner_fail
  info "logs: $DISPATCH_LOG | board: $CLIPSE_BIN status --board $BOARD_DIR"
  exit 1
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

usage() {
  # Print the header comment block: from line 3 until the first line that is
  # not a comment (robust to header edits / line-number drift).
  awk 'NR>=3 { if ($0 !~ /^#/) exit; sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"
}

main() {
  local cmd=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --tickets) shift; TICKETS="${1:?--tickets needs a value}"; TICKETS_SET=1 ;;
      --tickets=*) TICKETS="${1#*=}"; TICKETS_SET=1 ;;
      --fast) FAST=1 ;;
      --no-run) NO_RUN=1 ;;
      --keep) KEEP=1 ;;
      -h|--help) usage; exit 0 ;;
      setup|reset|build|seed|run|verify|generate|all) cmd="$1" ;;
      *) die "unknown argument: $1 (see --help)" ;;
    esac
    shift
  done
  [[ -n "$cmd" ]] || cmd="all"

  case "$TICKETS" in
    ''|*[!0-9]*) die "--tickets must be a positive integer, got '$TICKETS'" ;;
  esac
  [[ "$TICKETS" -ge 1 ]] || die "--tickets must be >= 1"

  load_env
  trap on_exit EXIT
  # On Ctrl-C / TERM, exit cleanly so the EXIT trap (on_exit) still runs and
  # stops the dispatcher -- a smoke run never leaves a daemon behind.
  trap 'exit 130' INT TERM

  case "$cmd" in
    setup)
      preflight; setup ;;
    reset)
      preflight; reset ;;
    build)
      preflight; build ;;
    seed)
      preflight; seed ;;
    generate)
      preflight; generate_config ;;
    run)
      preflight; generate_config; run; report ;;
    verify)
      preflight; report ;;
    all)
      preflight
      reset
      build
      generate_config
      seed
      if [[ "$NO_RUN" -eq 1 ]]; then
        info "--no-run: stopping after seed"
        info "inspect: linear issue query --team $TEAM_KEY"
        exit 0
      fi
      run
      report
      ;;
    *)
      die "unknown command: $cmd" ;;
  esac
}

main "$@"
