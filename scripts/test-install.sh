#!/bin/sh
# Tests for scripts/install.sh, with curl and uname mocked so nothing is
# downloaded and no service is registered. Platform is forced to Linux
# x86_64, whose branch only prints instructions.
#
#   sh scripts/test-install.sh
#   SH=dash sh scripts/test-install.sh

set -u

SELF_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
TARGET="$SELF_DIR/install.sh"
SH="${SH:-sh}"
[ -f "$TARGET" ] || { echo "cannot find install.sh next to $0" >&2; exit 1; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/install-test.XXXXXX")" || exit 1
TMP="$(CDPATH= cd -- "$TMP" && pwd)"
trap 'rm -rf "$TMP"' EXIT INT TERM

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "ok   - $1"; }
no() { FAIL=$((FAIL + 1)); echo "FAIL - $1"; [ -n "${2:-}" ] && echo "       $2"; }
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }

BIN_CONTENT="pretend this is a collector"
GOOD_SHA="$(printf '%s' "$BIN_CONTENT" | (shasum -a 256 2>/dev/null || sha256sum) | awk '{print $1}')"

# Builds a sandbox: a fake PATH with mocked curl/uname, a .env, and a
# SHA256SUMS body of the caller's choosing.
setup() {
  case="$TMP/$1"
  sums_body="$2"
  sums_ok="${3:-yes}"
  rm -rf "$case"
  mkdir -p "$case/bin" "$case/run"
  printf 'CR_API_TOKEN=x\nELIXIR_API_TOKEN=emcg_x\n' > "$case/run/.env"
  printf '%s' "$sums_body" > "$case/sums"
  printf '%s' "$BIN_CONTENT" > "$case/binary"

  cat > "$case/bin/uname" <<'U'
#!/bin/sh
[ "${1:-}" = "-m" ] && echo x86_64 || echo Linux
U
  cat > "$case/bin/curl" <<C
#!/bin/sh
# Args end with: -o <path> <url>  or  <url> -o <path>
out=""; url=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -o) out="\$2"; shift 2 ;;
    -*) shift ;;
    *) url="\$1"; shift ;;
  esac
done
case "\$url" in
  *api.github.com*releases/latest*) printf '{"tag_name": "v9.9.9"}'; exit 0 ;;
  *SHA256SUMS*)
    [ "$sums_ok" = yes ] || exit 22
    cp "$case/sums" "\$out"; exit 0 ;;
  *collector_*)
    cp "$case/binary" "\$out"; exit 0 ;;
esac
exit 22
C
  chmod +x "$case/bin/uname" "$case/bin/curl"
}

run() {
  ( cd "$TMP/$1/run" && PATH="$TMP/$1/bin:$PATH" "$SH" "$TARGET" 2>&1 )
}

# --- 1. the happy path installs and says what it verified ---
setup good "$GOOD_SHA  collector_linux_amd64
deadbeef  collector_darwin_arm64"
out="$(run good)"; code=$?
if [ "$code" -eq 0 ] && contains "SHA256 verified against v9.9.9" "$out"; then
  ok "a matching checksum installs and names the release it verified"
else
  no "a matching checksum installs and names the release it verified" "exit $code: $out"
fi
if [ -x "$TMP/good/run/collector" ]; then
  ok "the verified binary is installed executable"
else
  no "the verified binary is installed executable"
fi

# --- 2. no line for THIS platform must never report success (issue #3) ---
setup nolinux "deadbeef  collector_darwin_arm64"
out="$(run nolinux)"; code=$?
if [ "$code" -ne 0 ]; then
  ok "a missing platform checksum fails the install"
else
  no "a missing platform checksum fails the install" "exit 0: $out"
fi
if contains "SHA256 verified" "$out"; then
  no "a missing platform checksum must not claim verification" "$out"
else
  ok "a missing platform checksum must not claim verification"
fi
if [ ! -e "$TMP/nolinux/run/collector" ]; then
  ok "nothing is installed when verification fails"
else
  no "nothing is installed when verification fails"
fi

# --- 3. a mismatch refuses ---
setup mismatch "0000000000000000000000000000000000000000000000000000000000000000  collector_linux_amd64"
out="$(run mismatch)"; code=$?
if [ "$code" -ne 0 ] && contains "SHA256 mismatch" "$out"; then
  ok "a checksum mismatch refuses"
else
  no "a checksum mismatch refuses" "exit $code: $out"
fi

# --- 4. an unreachable SHA256SUMS refuses instead of installing blind ---
setup nosums "" no
out="$(run nosums)"; code=$?
if [ "$code" -ne 0 ] && contains "refusing to install unverified" "$out"; then
  ok "an unreachable checksum file refuses"
else
  no "an unreachable checksum file refuses" "exit $code: $out"
fi

# --- 5. a malformed checksum refuses ---
setup malformed "notahash  collector_linux_amd64"
out="$(run malformed)"; code=$?
if [ "$code" -ne 0 ] && contains "Malformed checksum" "$out"; then
  ok "a malformed checksum refuses"
else
  no "a malformed checksum refuses" "exit $code: $out"
fi

# --- 6. two entries for one asset are ambiguous, not "first wins" ---
setup ambiguous "$GOOD_SHA  collector_linux_amd64
0000000000000000000000000000000000000000000000000000000000000000  collector_linux_amd64"
out="$(run ambiguous)"; code=$?
if [ "$code" -ne 0 ] && contains "expected exactly 1" "$out"; then
  ok "an ambiguous checksum file refuses"
else
  no "an ambiguous checksum file refuses" "exit $code: $out"
fi

# --- 7. a failed install must not clobber a working collector ---
setup keepold "deadbeef  collector_darwin_arm64"
printf 'the collector already running here' > "$TMP/keepold/run/collector"
chmod +x "$TMP/keepold/run/collector"
run keepold >/dev/null 2>&1
if [ "$(cat "$TMP/keepold/run/collector")" = "the collector already running here" ]; then
  ok "an existing collector survives a failed install"
else
  no "an existing collector survives a failed install"
fi

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
