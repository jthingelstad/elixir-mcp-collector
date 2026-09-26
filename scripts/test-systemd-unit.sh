#!/bin/sh
# End-to-end test of scripts/elixir-collector.service under real systemd
# (issue #6): the hardened sandbox must start the collector, let it
# tighten a loose .env, and let it SELF-UPDATE in place - the one write
# the sandbox has to allow. It installs the unit the way a
# one-directory-per-instance operator does (elixir-collector@.service
# with %i in the three paths), under /home so ProtectHome is exercised
# too, then checks that exit 2 stops the unit instead of looping.
#
# Needs systemd as PID 1, root or sudo, go and python3. CI runs it on
# ubuntu-latest. The "hub" is python's static file server on 127.0.0.1:
# /config names a newer build of this same checkout; nothing leaves the
# machine and no real token exists.
#
#   sh scripts/test-systemd-unit.sh

set -u

SELF_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
REPO="$(dirname "$SELF_DIR")"
[ -d /run/systemd/system ] || { echo "skip: systemd is not PID 1 here"; exit 0; }
SUDO=""; [ "$(id -u)" -eq 0 ] || SUDO="sudo"

SVC_USER="elixir-smoke"
HOME_DIR="/home/$SVC_USER"
INST="smoke"
DIR="$HOME_DIR/$INST"
UNIT="elixir-collector-smoke@.service"
UNIT_INST="elixir-collector-smoke@$INST.service"
PORT=18787
KEY="go-linux-$(cd "$REPO" && go env GOARCH)"
CR_TOKEN="eyJsmoke.crsecretcrsecret.sig"
API_TOKEN="emcg_smokesecretsmokesecret"

WEB="$(mktemp -d "${TMPDIR:-/tmp}/systemd-test.XXXXXX")" || exit 1
chmod 755 "$WEB"
PY_PID=""
cleanup() {
  $SUDO systemctl stop "$UNIT_INST" >/dev/null 2>&1
  $SUDO rm -f "/etc/systemd/system/$UNIT"
  $SUDO systemctl daemon-reload >/dev/null 2>&1
  $SUDO systemctl reset-failed >/dev/null 2>&1
  [ -n "$PY_PID" ] && kill "$PY_PID" 2>/dev/null
  id "$SVC_USER" >/dev/null 2>&1 && $SUDO userdel -r "$SVC_USER" >/dev/null 2>&1
  rm -rf "$WEB"
}
trap cleanup EXIT INT TERM

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "ok   - $1"; }
no() { FAIL=$((FAIL + 1)); echo "FAIL - $1"; [ -n "${2:-}" ] && echo "       $2"; }
journal() { $SUDO journalctl -q -u "$UNIT_INST" -o cat --no-pager 2>/dev/null; }
wait_for() { # wait_for <seconds> <journal substring>
  i=0
  while [ "$i" -lt "$1" ]; do
    journal | grep -qF -- "$2" && return 0
    sleep 1; i=$((i + 1))
  done
  return 1
}
sha() { sha256sum "$1" | awk '{print $1}'; }

# --- the two builds and the fake hub ---
(cd "$REPO" && CGO_ENABLED=0 go build -ldflags "-X main.version=v0.0.1-smoke" -o "$WEB/old" ./cmd/collector) || exit 1
(cd "$REPO" && CGO_ENABLED=0 go build -ldflags "-X main.version=v0.0.2-smoke" -o "$WEB/new" ./cmd/collector) || exit 1
NEW_SHA="$(sha "$WEB/new")"
mkdir -p "$WEB/api/collector"
cat > "$WEB/api/collector/config" <<J
{"pacing_ms":1500,"gateway":{"name":"smoke","channel":"bulk","status":"active"},
 "update":{"$KEY":{"version":"v0.0.2-smoke","sha256":"$NEW_SHA","url":"http://127.0.0.1:$PORT/new"}}}
J
python3 -m http.server --bind 127.0.0.1 --directory "$WEB" "$PORT" >"$WEB/http.log" 2>&1 &
PY_PID=$!
i=0; until curl -fs "http://127.0.0.1:$PORT/api/collector/config" >/dev/null; do
  i=$((i + 1)); [ "$i" -gt 20 ] && { echo "fake hub did not start"; cat "$WEB/http.log"; exit 1; }
  sleep 0.5
done

# --- the unit, installed as a template with %i ---
id "$SVC_USER" >/dev/null 2>&1 || $SUDO useradd --create-home --home-dir "$HOME_DIR" --shell /usr/sbin/nologin "$SVC_USER"
$SUDO install -d -o "$SVC_USER" -g "$SVC_USER" -m 700 "$DIR"
$SUDO install -o "$SVC_USER" -g "$SVC_USER" -m 755 "$WEB/old" "$DIR/collector"
printf 'CR_API_TOKEN=%s\nELIXIR_API_TOKEN=%s\nELIXIR_API_BASE=http://127.0.0.1:%s/api/collector\n' \
  "$CR_TOKEN" "$API_TOKEN" "$PORT" | $SUDO tee "$DIR/.env" >/dev/null
$SUDO chown "$SVC_USER:$SVC_USER" "$DIR/.env"
$SUDO chmod 644 "$DIR/.env"   # loose on purpose: the collector must tighten it

sed -e "s|^User=CHANGE_ME|User=$SVC_USER|" \
    -e "s|/CHANGE_ME/elixir-mcp-collector|$HOME_DIR/%i|g" \
    "$SELF_DIR/elixir-collector.service" | $SUDO tee "/etc/systemd/system/$UNIT" >/dev/null
if grep -q CHANGE_ME "/etc/systemd/system/$UNIT"; then
  no "every placeholder in the unit is substituted" "$(grep -n CHANGE_ME "/etc/systemd/system/$UNIT")"
fi
$SUDO systemctl daemon-reload
if out="$($SUDO systemd-analyze verify "/etc/systemd/system/$UNIT_INST" 2>&1)" && ! printf '%s' "$out" | grep -qi "unknown\|error"; then
  ok "systemd-analyze verify accepts the unit"
else
  no "systemd-analyze verify accepts the unit" "$out"
fi

$SUDO systemctl start "$UNIT_INST"

# --- 1. starts inside the sandbox, on the old build ---
if wait_for 20 "version=v0.0.1-smoke"; then ok "the collector starts under the hardened unit"
else no "the collector starts under the hardened unit" "$(journal | tail -20)"; fi

# --- 2. self-update writes the new binary through ReadWritePaths ---
if wait_for 60 "version=v0.0.2-smoke"; then ok "self-update replaces the binary and systemd restarts into it"
else no "self-update replaces the binary and systemd restarts into it" "$(journal | tail -20)"; fi
if [ "$($SUDO sha256sum "$DIR/collector" | awk '{print $1}')" = "$NEW_SHA" ]; then ok "the installed binary is the named build"
else no "the installed binary is the named build"; fi
if journal | grep -qF "self-update failed"; then no "no self-update failure in the log" "$(journal | grep -F 'self-update failed')"
else ok "no self-update failure in the log"; fi
left="$($SUDO find "$DIR" -name '.collector-update*' | head -1)"
if [ -z "$left" ]; then ok "no update temp file is left behind"; else no "no update temp file is left behind" "$left"; fi

# --- 3. the loose .env was tightened, and said so ---
mode="$($SUDO stat -c %a "$DIR/.env")"
if [ "$mode" = "600" ]; then ok "the loose .env is tightened to 600"; else no "the loose .env is tightened to 600" "mode $mode"; fi
if journal | grep -qF "tightened to 600"; then ok "the tightening is logged"; else no "the tightening is logged"; fi

# --- 4. the loopback-http base is allowed, with a note ---
if journal | grep -qF "loopback address - development only"; then ok "plain http to loopback runs, with a note"
else no "plain http to loopback runs, with a note"; fi

# --- 5. no secret reaches the journal ---
if journal | grep -qF -e "$CR_TOKEN" -e "$API_TOKEN" -e "secretsmokesecret" -e "crsecretcrsecret"; then
  no "neither token appears in the journal"
else
  ok "neither token appears in the journal"
fi

# --- 6. exit 2 stops the unit rather than restart-looping ---
$SUDO systemctl stop "$UNIT_INST"
printf 'ELIXIR_API_BASE=http://127.0.0.1:%s/api/collector\n' "$PORT" | $SUDO tee "$DIR/.env" >/dev/null
$SUDO systemctl start "$UNIT_INST"
sleep 3
before="$($SUDO systemctl show -p NRestarts --value "$UNIT_INST")"
sleep 5   # more than twice RestartSec: a looping unit would restart here
status="$($SUDO systemctl show -p ExecMainStatus --value "$UNIT_INST")"
state="$($SUDO systemctl show -p ActiveState --value "$UNIT_INST")"
restarts="$($SUDO systemctl show -p NRestarts --value "$UNIT_INST")"
if [ "$status" = "2" ] && [ "$state" != "active" ] && [ "$state" != "activating" ] && [ "$restarts" = "$before" ]; then
  ok "a missing token (exit 2) stops the unit"
else
  no "a missing token (exit 2) stops the unit" "status=$status state=$state restarts $before -> $restarts"
fi

echo
echo "--- systemd-analyze security (informational) ---"
$SUDO systemd-analyze security --no-pager "$UNIT_INST" 2>/dev/null | tail -1
echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
