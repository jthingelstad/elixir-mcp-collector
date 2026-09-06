#!/bin/sh
# Shell-level tests for scripts/run-forever.sh — the layouts a real
# operator actually creates. POSIX sh only (this script and the one it
# tests both run under BusyBox ash on Synology DSM).
#
#   sh scripts/test-run-forever.sh
#   SH=dash sh scripts/test-run-forever.sh   # or busybox sh, to prove portability

set -u

SELF_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
TARGET="$SELF_DIR/run-forever.sh"
SH="${SH:-sh}"
[ -f "$TARGET" ] || { echo "cannot find run-forever.sh next to $0" >&2; exit 1; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/run-forever-test.XXXXXX")" || exit 1
trap 'rm -rf "$TMP"' EXIT INT TERM
# Normalize: $TMPDIR carries a trailing slash on macOS, and run-forever.sh
# reports paths that "cd && pwd" has already collapsed.
TMP="$(CDPATH= cd -- "$TMP" && pwd)"

PASS=0
FAIL=0

ok() { PASS=$((PASS + 1)); echo "ok   - $1"; }
no() { FAIL=$((FAIL + 1)); echo "FAIL - $1"; [ -n "${2:-}" ] && echo "       $2"; }

# Asserts $2 (haystack) contains $1 (needle).
contains() {
  case "$2" in
    *"$1"*) return 0 ;;
    *) return 1 ;;
  esac
}

fake_binary() {
  cat > "$1" <<'BIN'
#!/bin/sh
echo "fake collector ran"
exit 7
BIN
  chmod +x "$1"
}

# --- 1. script next to the binary (the standalone install.sh layout) ---
d="$TMP/standalone"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
fake_binary "$d/collector"
out="$( (cd "$TMP" && "$SH" "$d/run-forever.sh" --check) 2>&1 )"
if contains "would run Go collector ($d/collector)" "$out"; then
  ok "finds the binary next to the script"
else
  no "finds the binary next to the script" "$out"
fi
if contains "log $d/collector.log" "$out"; then
  ok "log defaults next to the binary, not above it"
else
  no "log defaults next to the binary, not above it" "$out"
fi

# --- 2. script in scripts/ with the binary at the repo root ---
d="$TMP/checkout"
mkdir -p "$d/scripts"
cp "$TARGET" "$d/scripts/run-forever.sh"
fake_binary "$d/collector"
out="$( (cd "$TMP" && "$SH" "$d/scripts/run-forever.sh" --check) 2>&1 )"
if contains "would run Go collector ($d/collector)" "$out"; then
  ok "finds the binary one level up from scripts/"
else
  no "finds the binary one level up from scripts/" "$out"
fi

# --- 3. binary only in the working directory ---
d="$TMP/cwdonly"
mkdir -p "$d/elsewhere" "$d/work"
cp "$TARGET" "$d/elsewhere/run-forever.sh"
fake_binary "$d/work/collector"
out="$( (cd "$d/work" && "$SH" "$d/elsewhere/run-forever.sh" --check) 2>&1 )"
if contains "would run Go collector ($d/work/collector)" "$out"; then
  ok "finds the binary in the working directory"
else
  no "finds the binary in the working directory" "$out"
fi

# --- 4. no binary, no Python worker: exit non-zero with a real message ---
d="$TMP/empty"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
out="$( (cd "$d" && "$SH" "$d/run-forever.sh" --check) 2>&1 )"
code=$?
if [ "$code" -ne 0 ]; then
  ok "no worker anywhere exits non-zero"
else
  no "no worker anywhere exits non-zero" "exit $code"
fi
if contains "no collector binary found near" "$out" && contains "no Python worker present" "$out"; then
  ok "no worker anywhere names the paths it checked"
else
  no "no worker anywhere names the paths it checked" "$out"
fi

# --- 5. Python twin when there is no binary ---
d="$TMP/pyonly"
mkdir -p "$d/python"
cp "$TARGET" "$d/run-forever.sh"
: > "$d/python/collector.py"
out="$( (cd "$TMP" && "$SH" "$d/run-forever.sh" --check) 2>&1 )"
if contains "would run Python worker" "$out"; then
  ok "falls back to the Python twin when it exists"
else
  no "falls back to the Python twin when it exists" "$out"
fi
out="$( (cd "$TMP" && PYTHON_BIN=definitely-not-a-real-python "$SH" "$d/run-forever.sh" --check) 2>&1 )"
code=$?
if [ "$code" -ne 0 ] && contains "not on PATH" "$out"; then
  ok "Python twin with no interpreter fails fast"
else
  no "Python twin with no interpreter fails fast" "exit $code: $out"
fi

# --- 6. unwritable log fails fast instead of looping ---
d="$TMP/unwritable"
mkdir -p "$d/ro"
cp "$TARGET" "$d/run-forever.sh"
fake_binary "$d/collector"
chmod 555 "$d/ro"
if [ -w "$d/ro" ]; then
  echo "skip - unwritable log fails fast (running as root)"
else
  out="$( (cd "$TMP" && "$SH" "$d/run-forever.sh" --check "$d/ro/collector.log") 2>&1 )"
  code=$?
  if [ "$code" -ne 0 ] && contains "cannot write log file" "$out"; then
    ok "unwritable log fails fast"
  else
    no "unwritable log fails fast" "exit $code: $out"
  fi
fi
chmod 755 "$d/ro"

out="$( (cd "$TMP" && "$SH" "$d/run-forever.sh" --check "$d/nope/collector.log") 2>&1 )"
code=$?
if [ "$code" -ne 0 ] && contains "log directory does not exist" "$out"; then
  ok "missing log directory fails fast"
else
  no "missing log directory fails fast" "exit $code: $out"
fi

# --- 7. the loop actually runs the discovered worker and restarts it ---
d="$TMP/loop"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
fake_binary "$d/collector"
( cd "$TMP" && "$SH" "$d/run-forever.sh" >/dev/null 2>&1 ) &
loop_pid=$!
sleep 3
kill "$loop_pid" 2>/dev/null
wait "$loop_pid" 2>/dev/null
log="$(cat "$d/collector.log" 2>/dev/null || true)"
if contains "fake collector ran" "$log"; then
  ok "loop runs the discovered worker"
else
  no "loop runs the discovered worker" "$log"
fi
if contains "worker exited (7); restarting in 2s" "$log"; then
  ok "loop reports the exit code and restarts"
else
  no "loop reports the exit code and restarts" "$log"
fi

# --- 8. start-up failures are mirrored into a log, not only stderr ---
# Under DSM Task Scheduler and systemd nobody sees stderr, so a boot
# task that dies at the pre-flight check must leave a trace on disk.
d="$TMP/mirror"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
( cd "$d" && "$SH" "$d/run-forever.sh" >/dev/null 2>&1 )
log="$(cat "$d/collector.log" 2>/dev/null || true)"
if contains "no collector binary found near" "$log"; then
  ok "no-worker failure is mirrored into the log beside the script"
else
  no "no-worker failure is mirrored into the log beside the script" "$log"
fi

d="$TMP/mirror2"
mkdir -p "$d/logs"
cp "$TARGET" "$d/run-forever.sh"
( cd "$TMP" && "$SH" "$d/run-forever.sh" "$d/logs/my.log" >/dev/null 2>&1 )
log="$(cat "$d/logs/my.log" 2>/dev/null || true)"
if contains "no collector binary found near" "$log"; then
  ok "no-worker failure is mirrored into an explicit logfile argument"
else
  no "no-worker failure is mirrored into an explicit logfile argument" "$log"
fi

# An unwritable log must still reach stderr and must not wedge.
d="$TMP/mirror3"
mkdir -p "$d/ro"
cp "$TARGET" "$d/run-forever.sh"
chmod 555 "$d/ro"
if [ -w "$d/ro" ]; then
  echo "skip - unwritable mirror target still reaches stderr (running as root)"
else
  out="$( (cd "$d/ro" && "$SH" "$d/run-forever.sh" "$d/ro/x.log") 2>&1 )"
  code=$?
  if [ "$code" -ne 0 ] && contains "run-forever:" "$out"; then
    ok "unwritable mirror target still reaches stderr and exits"
  else
    no "unwritable mirror target still reaches stderr and exits" "exit $code: $out"
  fi
fi
chmod 755 "$d/ro"

# --- 9. the log rotates instead of growing forever ---
d="$TMP/rotate"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
cat > "$d/collector" <<'BIN'
#!/bin/sh
# ~4 KB per run, so a 2 KB cap trips on the first restart.
i=0
while [ "$i" -lt 40 ]; do
  echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  i=$((i + 1))
done
exit 1
BIN
chmod +x "$d/collector"
( cd "$TMP" && MAX_LOG_BYTES=2048 "$SH" "$d/run-forever.sh" >/dev/null 2>&1 ) &
loop_pid=$!
sleep 3
kill "$loop_pid" 2>/dev/null
wait "$loop_pid" 2>/dev/null
if [ -f "$d/collector.log.1" ]; then
  ok "log rotates past MAX_LOG_BYTES, keeping one generation"
else
  no "log rotates past MAX_LOG_BYTES, keeping one generation" "no $d/collector.log.1"
fi
size="$(wc -c <"$d/collector.log" 2>/dev/null | tr -d ' ')"
if [ -n "$size" ] && [ "$size" -lt 8192 ]; then
  ok "log restarts small after rotation"
else
  no "log restarts small after rotation" "size=${size:-none}"
fi

d="$TMP/norotate"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
fake_binary "$d/collector"
( cd "$TMP" && MAX_LOG_BYTES=0 "$SH" "$d/run-forever.sh" >/dev/null 2>&1 ) &
loop_pid=$!
sleep 3
kill "$loop_pid" 2>/dev/null
wait "$loop_pid" 2>/dev/null
if [ ! -f "$d/collector.log.1" ]; then
  ok "MAX_LOG_BYTES=0 disables rotation"
else
  no "MAX_LOG_BYTES=0 disables rotation" "rotated anyway"
fi

# --- 10. exit 2 is a config error: stop, do not spin ---
d="$TMP/cfgfail"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
cat > "$d/collector" <<'BIN'
#!/bin/sh
echo "missing required config: ELIXIR_API_TOKEN" >&2
exit 2
BIN
chmod +x "$d/collector"
out="$( (cd "$TMP" && "$SH" "$d/run-forever.sh") 2>&1 )"
code=$?
if [ "$code" -eq 2 ]; then
  ok "worker exit 2 stops the loop with exit 2"
else
  no "worker exit 2 stops the loop with exit 2" "exit $code: $out"
fi
if contains "Not restarting" "$out"; then
  ok "config failure explains itself on stderr"
else
  no "config failure explains itself on stderr" "$out"
fi
log="$(cat "$d/collector.log" 2>/dev/null || true)"
if contains "missing required config" "$log" && contains "bad configuration" "$log"; then
  ok "config failure keeps the worker's own error next to ours in the log"
else
  no "config failure keeps the worker's own error next to ours in the log" "$log"
fi
runs="$(grep -c "worker exited" "$d/collector.log" 2>/dev/null || echo 0)"
if [ "$runs" -eq 1 ]; then
  ok "config failure runs the worker exactly once"
else
  no "config failure runs the worker exactly once" "ran $runs times"
fi

# Every other exit code still restarts.
d="$TMP/othercode"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
fake_binary "$d/collector"   # exits 7
( cd "$TMP" && "$SH" "$d/run-forever.sh" >/dev/null 2>&1 ) &
loop_pid=$!
sleep 3
kill "$loop_pid" 2>/dev/null
wait "$loop_pid" 2>/dev/null
runs="$(grep -c "worker exited (7)" "$d/collector.log" 2>/dev/null || echo 0)"
if [ "$runs" -ge 2 ]; then
  ok "a non-config exit still restarts"
else
  no "a non-config exit still restarts" "restarted $runs times"
fi

# --- 11. paths containing spaces (issue #4) ---
# ROOTS was one space-separated string iterated unquoted, so every path
# was split on whitespace and a real install under a folder with a space
# in it could not find the binary sitting beside the script.
d="$TMP/Elixir Collector"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
fake_binary "$d/collector"
out="$( (cd "$TMP" && "$SH" "$d/run-forever.sh" --check) 2>&1 )"
if contains "would run Go collector ($d/collector)" "$out"; then
  ok "finds a binary beside the script under a path with spaces"
else
  no "finds a binary beside the script under a path with spaces" "$out"
fi

d="$TMP/My Checkout/scripts"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
fake_binary "$TMP/My Checkout/collector"
out="$( (cd "$TMP" && "$SH" "$d/run-forever.sh" --check) 2>&1 )"
if contains "would run Go collector ($TMP/My Checkout/collector)" "$out"; then
  ok "finds a binary one level up under a path with spaces"
else
  no "finds a binary one level up under a path with spaces" "$out"
fi

d="$TMP/Python Home"
mkdir -p "$d/python"
cp "$TARGET" "$d/run-forever.sh"
: > "$d/python/collector.py"
out="$( (cd "$TMP" && "$SH" "$d/run-forever.sh" --check) 2>&1 )"
if contains "would run Python worker" "$out"; then
  ok "finds the Python twin under a path with spaces"
else
  no "finds the Python twin under a path with spaces" "$out"
fi

d="$TMP/Working Dir"
mkdir -p "$d"
cp "$TARGET" "$TMP/run-forever-cwd.sh"
fake_binary "$d/collector"
out="$( (cd "$d" && "$SH" "$TMP/run-forever-cwd.sh" --check) 2>&1 )"
if contains "would run Go collector ($d/collector)" "$out"; then
  ok "finds a binary in a working directory with spaces"
else
  no "finds a binary in a working directory with spaces" "$out"
fi

# An explicit logfile argument with spaces must survive too.
d="$TMP/Log Home"
mkdir -p "$d/log dir"
cp "$TARGET" "$d/run-forever.sh"
fake_binary "$d/collector"
out="$( (cd "$TMP" && "$SH" "$d/run-forever.sh" --check "$d/log dir/collector run.log") 2>&1 )"
if contains "log $d/log dir/collector run.log" "$out"; then
  ok "an explicit logfile containing spaces is used verbatim"
else
  no "an explicit logfile containing spaces is used verbatim" "$out"
fi

# And the loop actually runs from such a path, not just --check.
d="$TMP/Run Space"
mkdir -p "$d"
cp "$TARGET" "$d/run-forever.sh"
fake_binary "$d/collector"
( cd "$TMP" && "$SH" "$d/run-forever.sh" >/dev/null 2>&1 ) &
loop_pid=$!
sleep 3
kill "$loop_pid" 2>/dev/null
wait "$loop_pid" 2>/dev/null
if contains "fake collector ran" "$(cat "$d/collector.log" 2>/dev/null || true)"; then
  ok "the loop runs a worker found under a path with spaces"
else
  no "the loop runs a worker found under a path with spaces" "no output in $d/collector.log"
fi

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
