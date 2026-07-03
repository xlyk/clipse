#!/usr/bin/env bash
# record-demo.sh — capture one clean-slate Clipse smoke run as a shareable,
# sped-up MP4: the Linear desktop app (left) beside the Clipse TUI (right) on
# one display, both updating in lockstep as the pipeline drives a small
# dependency DAG to all-green.
#
# It ORCHESTRATES the existing smoke harness (scripts/smoke/smoke.sh) through
# its documented interface only — it never re-implements or edits it. The
# harness sources the LINEAR/ANTHROPIC keys itself (from scripts/smoke/smoke.env),
# so this script handles no secrets.
#
# Prereqs (one-time, human): grant iTerm2 Screen Recording + Accessibility;
# `scripts/smoke/smoke.sh setup` once (creates the throwaway target repo); open
# Linear on the CLI team board in list-view-grouped-by-status. Then, from the
# iTerm window you want recorded: `scripts/demo/record-demo.sh`.
#
# Flags: --full (10-ticket DAG instead of the default --fast 3-ticket chain),
#        --keep (leave raw capture + board for inspection).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT_DIR="$ROOT/scripts/demo"
SMOKE="$ROOT/scripts/smoke/smoke.sh"
CLIPSE_BIN="$ROOT/bin/clipse"

info() { printf '\033[0;36m[demo]\033[0m %s\n' "$*"; }
warn() { printf '\033[0;33m[demo] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[0;31m[demo] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

FAST_FLAG="--fast"
KEEP=0
for arg in "$@"; do
  case "$arg" in
    --full) FAST_FLAG="" ;;
    --keep) KEEP=1 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) die "unknown flag: $arg" ;;
  esac
done

# --- config (from smoke.env, with defaults) ---------------------------------
[[ -f "$ROOT/scripts/smoke/smoke.env" ]] \
  || die "scripts/smoke/smoke.env missing — copy smoke.env.example and run 'smoke.sh setup' first"
# shellcheck disable=SC1091
source "$ROOT/scripts/smoke/smoke.env"
: "${SMOKE_HOME:=$HOME/Code/clipse-smoke}"
: "${BOARD_DIR:=$SMOKE_HOME/board}"
# The Linear view the demo opens. `open -a Linear <url>` routes it to the
# desktop app (not the browser). Set this view to list-layout grouped-by-status
# in Linear ONCE — Linear persists layout+grouping per view, so every later run
# opens straight into the right look.
: "${LINEAR_BOARD_URL:=https://linear.app/clipse-development/team/CLI/all}"
DEMO_DIR="$SMOKE_HOME/demo"
RAW="$DEMO_DIR/raw.mov"
OUT="$DEMO_DIR/clipse-demo.mp4"
RUN_LOG="$DEMO_DIR/run.log"
RC_FILE="$DEMO_DIR/rc"
mkdir -p "$DEMO_DIR"

FF_PID=""
SMOKE_PID=""
DOCK_HIDDEN=0

cleanup() {
  # Always stop the recorder and the dispatcher; always restore the Dock.
  [[ -n "$FF_PID" ]] && kill -INT "$FF_PID" 2>/dev/null || true
  [[ -n "$SMOKE_PID" ]] && kill "$SMOKE_PID" 2>/dev/null || true  # triggers smoke.sh's own dispatcher-stop trap
  if [[ "$DOCK_HIDDEN" == 1 ]]; then
    defaults write com.apple.dock autohide -bool false && killall Dock 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

# --- preflight --------------------------------------------------------------
command -v ffmpeg >/dev/null || die "ffmpeg not found"
command -v ffprobe >/dev/null || die "ffprobe not found"
[[ -x "$CLIPSE_BIN" ]] || die "no ./bin/clipse — the harness build will make it, but check the repo"
# Linear is launched + navigated in phase B (open -a Linear), so it need not be running here.

avail_kb="$(df -k "$SMOKE_HOME" 2>/dev/null | awk 'NR==2{print $4}')"
[[ -n "$avail_kb" && "$avail_kb" -lt 1048576 ]] && warn "less than ~1GB free under $SMOKE_HOME — a raw capture can be large"

# Re-enumerate the avfoundation screen index at runtime (indices shift).
IDX="$(ffmpeg -f avfoundation -list_devices true -i "" 2>&1 \
        | grep -oE '\[[0-9]+\] Capture screen 0' | grep -oE '[0-9]+' | head -1 || true)"
[[ -n "$IDX" ]] || die "could not find the 'Capture screen 0' avfoundation device"
info "screen capture device index: $IDX"

# TCC flush: fire any Screen-Recording permission prompt NOW, not mid-demo.
info "preflight: 2s permission-flush capture"
ffmpeg -hide_banner -f avfoundation -framerate 15 -i "${IDX}:none" -t 2 -f null - >/dev/null 2>&1 \
  || die "screen-recording capture failed — grant iTerm2 Screen Recording (System Settings → Privacy & Security), then re-run"

# --- phase A: pre-roll (NOT recorded): reset + build + generate + seed -------
info "pre-roll: reset + build + seed (${FAST_FLAG:-full DAG})"
# shellcheck disable=SC2086
"$SMOKE" $FAST_FLAG --no-run

# --- phase B: open Linear on the board + stage the windows ------------------
info "opening Linear on the CLI board: $LINEAR_BOARD_URL"
open -a Linear "$LINEAR_BOARD_URL" 2>/dev/null || warn "could not open Linear — open it manually on the CLI board"
sleep 2   # let Linear launch/navigate before positioning it
info "arranging windows (Linear left, terminal right)"
osascript "$SCRIPT_DIR/arrange-windows.applescript" || warn "window arrange failed (Accessibility permission?) — arrange manually"
if defaults write com.apple.dock autohide -bool true 2>/dev/null; then killall Dock 2>/dev/null || true; DOCK_HIDDEN=1; fi
sleep 1

# --- phase C: start recording -----------------------------------------------
info "recording -> $RAW"
ffmpeg -hide_banner -y -f avfoundation -capture_cursor 0 -framerate 15 -i "${IDX}:none" \
  -t 1560 -c:v h264_videotoolbox -b:v 6M -pix_fmt yuv420p "$RAW" >/dev/null 2>&1 &
FF_PID=$!
clear; printf '$ clipse tui --board %s\n' "$BOARD_DIR"

# --- phase D: launch the run (background; output to a log) -------------------
info "launching smoke run (log: $RUN_LOG)"
TIMEOUT_S="${TIMEOUT_S:-1500}" "$SMOKE" run >"$RUN_LOG" 2>&1 &
SMOKE_PID=$!
caffeinate -dis -w "$SMOKE_PID" >/dev/null 2>&1 &
# The TUI hard-errors if the board db is absent; wait for the dispatcher to make it.
until [[ -f "$BOARD_DIR/clipse.db" ]]; do
  kill -0 "$SMOKE_PID" 2>/dev/null || die "smoke run exited before creating the board (see $RUN_LOG)"
  sleep 0.5
done

# --- phase E: completion watcher (background) -------------------------------
(
  wait "$SMOKE_PID"; echo $? >"$RC_FILE"
  sleep 8                                                  # hold the finished frame
  osascript -e 'tell application "System Events" to keystroke "q"' 2>/dev/null || true
  kill -INT "$FF_PID" 2>/dev/null || true
) &

# --- phase F: the visible surface (foreground; THIS is what gets recorded) --
"$CLIPSE_BIN" tui --board "$BOARD_DIR" || true

# --- phase G: post ----------------------------------------------------------
wait "$FF_PID" 2>/dev/null || true; FF_PID=""
if [[ "$DOCK_HIDDEN" == 1 ]]; then defaults write com.apple.dock autohide -bool false && killall Dock 2>/dev/null || true; DOCK_HIDDEN=0; fi

rc="$(cat "$RC_FILE" 2>/dev/null || echo 1)"
if [[ "$rc" != 0 ]]; then
  warn "SMOKE FAILED (rc=$rc) — keeping raw + logs, skipping encode. See $RUN_LOG"
  exit 1
fi

info "encoding sped-up MP4"
DUR="$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$RAW")"
N="$(awk "BEGIN{n=int($DUR/60+0.5); print (n<1)?1:n}")"
info "raw ${DUR%.*}s -> ${N}x speed-up"
ffmpeg -hide_banner -y -i "$RAW" \
  -vf "crop=iw:ih-76:0:76,setpts=PTS/${N},fps=30,scale=1920:-2" \
  -an -c:v libx264 -crf 20 -preset slow -pix_fmt yuv420p -movflags +faststart "$OUT" >/dev/null 2>&1

[[ "$KEEP" == 1 ]] || rm -f "$RAW"
info "done -> $OUT ($(ffprobe -v error -show_entries format=duration -of csv=p=0 "$OUT" 2>/dev/null | cut -d. -f1)s)"
