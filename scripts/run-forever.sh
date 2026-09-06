#!/bin/sh
# Generic supervisor for hosts without launchd (Linux, Synology DSM,
# BSD): reproduces launchd's KeepAlive in one loop. The worker EXITS
# after a successful self-update (and on crashes, and when its progress
# watchdog fires); this loop restarts it on the new code within seconds
# — the same contract launchd provides on macOS.
#
# Usage (foreground; put it under systemd, DSM Task Scheduler, or
# nohup/tmux yourself). Always invoke it through "sh": a copy fetched
# with curl -O carries no executable bit, so ./run-forever.sh dies at
# boot with permission denied.
#   sh run-forever.sh [logfile]
#   sh run-forever.sh --check [logfile]   # resolve and report, run nothing
#
# Layout-agnostic on purpose. This script may live either next to the
# collector binary (what install.sh produces: ./collector, ./.env and
# this script in one directory) or in scripts/ inside a checkout with
# the binary at the repo root. It searches, in order:
#   1. its own directory
#   2. its parent directory
#   3. the current working directory
# and runs the first executable "collector" it finds. With no binary
# anywhere it falls back to the Python twin (python/collector.py), and
# with neither it exits non-zero with a message instead of looping.
#
# Start-up failures print to stderr AND are mirrored into the log when
# a log is writable, because under DSM Task Scheduler and systemd
# nobody ever sees stderr. The log defaults to collector.log next to
# whichever worker is found; the optional [logfile] argument overrides
# that.
#
# The worker reads .env from ITS OWN directory (or $ELIXIR_MCP_ENV_FILE)
# — the Go binary looks beside the binary, python/collector.py looks
# beside collector.py. This loop does not care about the working
# directory.
#
# Environment:
#   PYTHON_BIN     which python runs the Python twin (default python3)
#   MAX_LOG_BYTES  rotate the log past this size (default 10485760;
#                  0 disables). One .1 generation is kept.

set -u

CHECK=0
if [ "${1:-}" = "--check" ]; then
  CHECK=1
  shift
fi
LOG_OVERRIDE="${1:-}"

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)" || exit 1
PARENT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)" || PARENT_DIR="$SCRIPT_DIR"
HERE_DIR="$(pwd)"

MAX_LOG_BYTES="${MAX_LOG_BYTES:-10485760}"

# Append to $1 if that is possible; say nothing if it is not. Used both
# for normal logging and to mirror start-up failures somewhere an
# operator running headless can actually find them.
try_append() {
  _f="$1"
  shift
  [ -n "$_f" ] || return 1
  [ -d "$(dirname -- "$_f")" ] || return 1
  ( printf '%s\n' "$*" >>"$_f" ) 2>/dev/null || return 1
  return 0
}

# stderr always; the log too, wherever one can be written. Before the
# log is resolved the candidates are the override, then the two
# directories most likely to belong to the operator.
fatal() {
  printf 'run-forever: %s\n' "$*" >&2
  for _c in "$LOG_OVERRIDE" "$SCRIPT_DIR/collector.log" "$HERE_DIR/collector.log"; do
    if try_append "$_c" "$(stamp) run-forever: $*"; then
      break
    fi
  done
  exit 1
}

stamp() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }

# Ordered, de-duplicated search roots.
ROOTS=""
for d in "$SCRIPT_DIR" "$PARENT_DIR" "$HERE_DIR"; do
  seen=0
  for r in $ROOTS; do
    [ "$r" = "$d" ] && seen=1
  done
  [ "$seen" = 0 ] && ROOTS="$ROOTS $d"
done

CMD=""
PY_WORKER=""
WORKER_DIR=""
SEARCHED=""

for d in $ROOTS; do
  SEARCHED="${SEARCHED:+$SEARCHED, }$d/collector"
  if [ -z "$CMD" ] && [ -f "$d/collector" ] && [ -x "$d/collector" ]; then
    CMD="$d/collector"
    WORKER_DIR="$d"
  fi
done

if [ -z "$CMD" ]; then
  for d in $ROOTS; do
    for rel in python/collector.py collector.py; do
      if [ -z "$PY_WORKER" ] && [ -f "$d/$rel" ]; then
        PY_WORKER="$d/$rel"
        WORKER_DIR="$(CDPATH= cd -- "$(dirname -- "$d/$rel")" && pwd)"
      fi
    done
  done
fi

PYTHON_BIN="${PYTHON_BIN:-python3}"

if [ -z "$CMD" ] && [ -n "$PY_WORKER" ]; then
  command -v "$PYTHON_BIN" >/dev/null 2>&1 || fatal \
"no collector binary found near $SEARCHED, and the Python worker at $PY_WORKER cannot run ($PYTHON_BIN not on PATH). Install the binary next to this script, or set PYTHON_BIN."
fi

if [ -z "$CMD" ] && [ -z "$PY_WORKER" ]; then
  fatal \
"no collector binary found near $SEARCHED and no Python worker present. Put the collector binary next to this script (see scripts/install.sh) or run this from the directory holding it."
fi

if [ -n "$LOG_OVERRIDE" ]; then
  LOG="$LOG_OVERRIDE"
else
  LOG="$WORKER_DIR/collector.log"
fi

LOG_DIR="$(dirname -- "$LOG")"
[ -d "$LOG_DIR" ] || fatal \
"log directory does not exist: $LOG_DIR (pass a writable path: sh run-forever.sh /path/to/collector.log)"
( : >>"$LOG" ) 2>/dev/null || fatal \
"cannot write log file $LOG (permission denied). Pass a writable path: sh run-forever.sh /path/to/collector.log"

if [ -n "$CMD" ]; then
  DESC="Go collector ($CMD)"
else
  DESC="Python worker ($PYTHON_BIN $PY_WORKER)"
fi

if [ "$CHECK" = 1 ]; then
  echo "run-forever: would run $DESC"
  echo "run-forever: log $LOG"
  exit 0
fi

# Volunteer hardware runs this for years; ">> forever" eventually fills
# the partition. Keep one previous generation and start clean.
rotate_log() {
  [ "$MAX_LOG_BYTES" -gt 0 ] 2>/dev/null || return 0
  [ -f "$LOG" ] || return 0
  _size="$(wc -c <"$LOG" 2>/dev/null | tr -d ' ')"
  [ -n "$_size" ] || return 0
  [ "$_size" -gt "$MAX_LOG_BYTES" ] 2>/dev/null || return 0
  mv -f "$LOG" "$LOG.1" 2>/dev/null || return 0
  printf '%s\n' "$(stamp) run-forever: rotated log at $_size bytes (previous kept as $LOG.1)" >>"$LOG"
}

echo "$(stamp) run-forever: starting $DESC (log=$LOG)" >>"$LOG"

while :; do
  if [ -n "$CMD" ]; then
    "$CMD" >>"$LOG" 2>&1
  else
    "$PYTHON_BIN" "$PY_WORKER" >>"$LOG" 2>&1
  fi
  code=$?
  echo "$(stamp) run-forever: worker exited ($code); restarting in 2s" >>"$LOG"
  rotate_log
  sleep 2
done
