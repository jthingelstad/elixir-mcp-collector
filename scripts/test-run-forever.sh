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

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
