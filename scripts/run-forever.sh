#!/bin/sh
# Generic supervisor for hosts without launchd (Linux, Synology DSM,
# BSD): reproduces launchd's KeepAlive in one loop. The worker EXITS
# after a successful self-update (and on crashes, and when its progress
# watchdog fires); this loop restarts it on the new code within seconds
# — the same contract launchd provides on macOS.
#
# Usage (foreground; put it under systemd, DSM Task Scheduler, or
# nohup/tmux yourself):
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
# The log defaults to collector.log next to whichever worker is found;
# the optional [logfile] argument overrides that. The worker reads .env
# from its own directory (or $ELIXIR_MCP_ENV_FILE) — this loop does not
# care about the working directory. PYTHON_BIN overrides which python
# runs the Python twin.

set -u

CHECK=0
if [ "${1:-}" = "--check" ]; then
  CHECK=1
  shift
fi

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)" || exit 1
PARENT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)" || PARENT_DIR="$SCRIPT_DIR"
HERE_DIR="$(pwd)"

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
  if ! command -v "$PYTHON_BIN" >/dev/null 2>&1; then
    echo "run-forever: no collector binary found near $SEARCHED, and the Python worker at $PY_WORKER cannot run ($PYTHON_BIN not on PATH). Install the binary next to this script, or set PYTHON_BIN." >&2
    exit 1
  fi
fi

if [ -z "$CMD" ] && [ -z "$PY_WORKER" ]; then
  echo "run-forever: no collector binary found near $SEARCHED and no Python worker present. Put the collector binary next to this script (see scripts/install.sh) or run this from the directory holding it." >&2
  exit 1
fi

if [ -n "${1:-}" ]; then
  LOG="$1"
else
  LOG="$WORKER_DIR/collector.log"
fi

LOG_DIR="$(dirname -- "$LOG")"
if [ ! -d "$LOG_DIR" ]; then
  echo "run-forever: log directory does not exist: $LOG_DIR (pass a writable path: sh run-forever.sh /path/to/collector.log)" >&2
  exit 1
fi
if ! ( : >>"$LOG" ) 2>/dev/null; then
  echo "run-forever: cannot write log file $LOG (permission denied). Pass a writable path: sh run-forever.sh /path/to/collector.log" >&2
  exit 1
fi

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

stamp() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }

echo "$(stamp) run-forever: starting $DESC (log=$LOG)" >>"$LOG"

while :; do
  if [ -n "$CMD" ]; then
    "$CMD" >>"$LOG" 2>&1
  else
    "$PYTHON_BIN" "$PY_WORKER" >>"$LOG" 2>&1
  fi
  code=$?
  echo "$(stamp) run-forever: worker exited ($code); restarting in 2s" >>"$LOG"
  sleep 2
done
