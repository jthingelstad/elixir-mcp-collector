#!/bin/sh
# Generic supervisor for hosts without launchd (Linux, Synology DSM,
# BSD): reproduces launchd's KeepAlive in one loop. The worker EXITS
# after a successful self-update (and on crashes); this loop restarts
# it on the new code within seconds — the same contract launchd
# provides on macOS.
#
# Usage (foreground; put it under systemd, DSM Task Scheduler, or
# nohup/tmux yourself):
#   sh scripts/run-forever.sh [logfile]
#
# The worker reads .env from the repo root; NODE_BIN overrides which
# node runs it (useful for unofficial armv7 builds outside PATH).

set -u
# Works from a git checkout (Node worker) OR standalone next to the Go
# binary: if ./collector exists beside this script's parent, run it;
# else fall back to the Node worker. The Go path needs no git, no Node.
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOG="${1:-$REPO_ROOT/collector.log}"
NODE_BIN="${NODE_BIN:-node}"

if [ -x "$REPO_ROOT/collector" ]; then
  CMD="$REPO_ROOT/collector"
  echo "$(date -u +%FT%TZ) run-forever: starting Go collector ($CMD, log=$LOG)" >>"$LOG"
else
  CMD=""
  echo "$(date -u +%FT%TZ) run-forever: starting Node worker (node=$NODE_BIN, log=$LOG)" >>"$LOG"
fi

while :; do
  if [ -n "$CMD" ]; then
    "$CMD" >>"$LOG" 2>&1
  else
    "$NODE_BIN" "$REPO_ROOT/src/index.mjs" >>"$LOG" 2>&1
  fi
  code=$?
  echo "$(date -u +%FT%TZ) run-forever: worker exited ($code); restarting in 2s" >>"$LOG"
  sleep 2
done
