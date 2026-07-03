#!/usr/bin/env bash
# demo.sh — prep a clean-slate Clipse pipeline run and show it live in the TUI.
#
# You handle the windows and the screen recording; this script only does the
# "rest of the prep": it wipes the board to a clean slate, rebuilds the binary,
# seeds the ticket DAG on Linear, then (on your cue) launches the dispatcher in
# the background and takes over this terminal with the live TUI. Quit the TUI
# (`q`) when you're done — the background dispatcher is stopped automatically.
#
# It ORCHESTRATES the smoke harness (scripts/smoke/smoke.sh) through its
# documented interface only; the harness sources the LINEAR/ANTHROPIC keys
# itself (from scripts/smoke/smoke.env), so this script handles no secrets.
#
# Suggested flow:
#   1. Open the Linear board (list view grouped by status) and this terminal
#      side by side; get them positioned how you want on screen.
#   2. Run `scripts/demo/demo.sh` — it preps the board, then waits.
#   3. Start your screen recording, then press ENTER: the pipeline + TUI start.
#   4. Watch it drive to all-green; press `q` to quit the TUI, stop recording.
#
# Flags: --full (10-ticket DAG instead of the default --fast 3-ticket chain),
#        --keep (leave generated artifacts + board in place after the run).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SMOKE="$ROOT/scripts/smoke/smoke.sh"
CLIPSE_BIN="$ROOT/bin/clipse"

info() { printf '\033[0;36m[demo]\033[0m %s\n' "$*"; }
die()  { printf '\033[0;31m[demo] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

FAST_FLAG="--fast"
KEEP_FLAG=""
for arg in "$@"; do
  case "$arg" in
    --full) FAST_FLAG="" ;;
    --keep) KEEP_FLAG="--keep" ;;
    -h|--help) sed -n '2,27p' "$0"; exit 0 ;;
    *) die "unknown flag: $arg" ;;
  esac
done

[[ -f "$ROOT/scripts/smoke/smoke.env" ]] \
  || die "scripts/smoke/smoke.env missing — copy smoke.env.example and run 'smoke.sh setup' first"
# shellcheck disable=SC1091
source "$ROOT/scripts/smoke/smoke.env"
: "${SMOKE_HOME:=$HOME/Code/clipse-smoke}"
: "${BOARD_DIR:=$SMOKE_HOME/board}"
RUN_LOG="$SMOKE_HOME/demo-run.log"

SMOKE_PID=""
cleanup() {
  # Killing the smoke `run` process triggers smoke.sh's own dispatcher-stop trap.
  [[ -n "$SMOKE_PID" ]] && kill "$SMOKE_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# --- prep (clean slate): reset + build + generate + seed, no run yet ---------
info "prepping clean slate: reset + build + seed (${FAST_FLAG:-full DAG})"
# shellcheck disable=SC2086
"$SMOKE" $FAST_FLAG $KEEP_FLAG --no-run

# --- cue: you arrange windows + start recording, then press ENTER ------------
echo
info "board is seeded and clean. arrange your windows and start recording."
read -r -p $'\033[0;36m[demo]\033[0m press ENTER to launch the pipeline + TUI... ' _

# --- launch the dispatcher (background) + show the TUI (foreground) ----------
info "launching pipeline (dispatcher log: $RUN_LOG)"
"$SMOKE" run >"$RUN_LOG" 2>&1 &
SMOKE_PID=$!
# The TUI hard-errors if the board db is absent; wait for the dispatcher to make it.
until [[ -f "$BOARD_DIR/clipse.db" ]]; do
  kill -0 "$SMOKE_PID" 2>/dev/null || die "pipeline exited before creating the board (see $RUN_LOG)"
  sleep 0.5
done

clear
"$CLIPSE_BIN" tui --board "$BOARD_DIR" || true
info "TUI closed. dispatcher log at $RUN_LOG"
