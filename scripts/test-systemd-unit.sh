#!/bin/sh
# End-to-end test of scripts/elixir-collector.service under real systemd
# (issue #6): the hardened sandbox must start the collector, let it
# tighten a loose .env, and let it SELF-UPDATE in place - the one write
# the sandbox has to allow - and ROLL BACK a release that cannot start:
# a candidate that passes the updater's self-check but crashes at
# startup must end with the previous binary running and its version
# refused. Every release it serves is signed the way release.yml signs
# one (a throwaway key, compiled into these builds for the test), and a
# release whose signature does not verify must be refused without
# touching the binary (issue #5). It installs the unit the way a
# one-directory-per-instance operator does (elixir-collector@.service
# with %i in the three paths), under /home so ProtectHome is exercised
# too, then checks that exit 2 stops the unit instead of looping.
#
# Needs systemd as PID 1, root or sudo, go, python3 and ssh-keygen. CI
# runs it on ubuntu-latest and ubuntu-24.04-arm. The "hub" is python's
# static file server on 127.0.0.1: /config names a newer build of this
# same checkout, served as a release at /releases/download/<version>/;
# nothing leaves the machine and no real token or key exists.
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
GOARCH="$(cd "$REPO" && go env GOARCH)"
KEY="go-linux-$GOARCH"
ASSET="collector_linux_$GOARCH"
# Above the updater's install floor (internal/v2/trust.go).
V_OLD=v9.9.1-smoke
V_NEW=v9.9.2-smoke
V_BAD=v9.9.3-smoke
V_FORGED=v9.9.4-smoke
CR_TOKEN="eyJsmoke.crsecretcrsecret.sig"
API_TOKEN="emcg_smokesecretsmokesecret"

WEB="$(mktemp -d "${TMPDIR:-/tmp}/systemd-test.XXXXXX")" || exit 1
chmod 755 "$WEB"
KEYS="$(mktemp -d "${TMPDIR:-/tmp}/systemd-test-keys.XXXXXX")" || exit 1  # never served
PY_PID=""
cleanup() {
  $SUDO systemctl stop "$UNIT_INST" >/dev/null 2>&1
  $SUDO rm -f "/etc/systemd/system/$UNIT"
  $SUDO systemctl daemon-reload >/dev/null 2>&1
  $SUDO systemctl reset-failed >/dev/null 2>&1
  [ -n "$PY_PID" ] && kill "$PY_PID" 2>/dev/null
  id "$SVC_USER" >/dev/null 2>&1 && $SUDO userdel -r "$SVC_USER" >/dev/null 2>&1
  rm -rf "$WEB" "$KEYS"
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

# --- a throwaway release key, the builds, and the fake hub ---
ssh-keygen -q -t ed25519 -N '' -C smoke-release-key -f "$KEYS/release-key" || exit 1
ssh-keygen -q -t ed25519 -N '' -C forger -f "$KEYS/forged-key" || exit 1
TRUST="-X 'github.com/jthingelstad/elixir-mcp-collector/internal/v2.releasePublicKeys=$(cat "$KEYS/release-key.pub")'"
(cd "$REPO" && CGO_ENABLED=0 go build -ldflags "-X main.version=$V_OLD $TRUST" -o "$WEB/old" ./cmd/collector) || exit 1
(cd "$REPO" && CGO_ENABLED=0 go build -ldflags "-X main.version=$V_NEW $TRUST" -o "$WEB/new" ./cmd/collector) || exit 1
(cd "$REPO" && CGO_ENABLED=0 go build -ldflags "-X main.version=$V_FORGED $TRUST" -o "$WEB/forged" ./cmd/collector) || exit 1
# The bad release: passes `version`, then panics before any answer from
# the hub (internal/v2/testdata/fakecollector, with the real v2.Guard).
(cd "$REPO" && CGO_ENABLED=0 go build -ldflags "-X main.version=$V_BAD -X main.mode=crash" -o "$WEB/bad" ./internal/v2/testdata/fakecollector) || exit 1
NEW_SHA="$(sha "$WEB/new")"
BAD_SHA="$(sha "$WEB/bad")"

# publish <version> <binary> [forged]: a release directory the way
# release.yml writes one - the asset, VERSION, SHA256SUMS and its
# signature. "forged" signs with a key the builds do not trust.
publish() {
  rel="$WEB/releases/download/$1"
  mkdir -p "$rel"
  cp "$2" "$rel/$ASSET"
  printf '%s\n' "$1" > "$rel/VERSION"
  (cd "$rel" && sha256sum "$ASSET" VERSION > SHA256SUMS)
  signer="$KEYS/release-key"
  [ "${3:-}" = forged ] && signer="$KEYS/forged-key"
  ssh-keygen -Y sign -q -f "$signer" -n elixir-mcp-collector-release "$rel/SHA256SUMS" || exit 1
}
publish "$V_NEW" "$WEB/new"
publish "$V_BAD" "$WEB/bad"
publish "$V_FORGED" "$WEB/forged" forged
FORGED_SHA="$(sha "$WEB/forged")"
# name <version> <sha>: what the hub's /config names for this platform.
name() {
  cat > "$WEB/api/collector/config" <<J
{"pacing_ms":1500,"gateway":{"name":"smoke","channel":"bulk","status":"active"},
 "update":{"$KEY":{"version":"$1","sha256":"$2","url":"http://127.0.0.1:$PORT/releases/download/$1/$ASSET"}}}
J
}
mkdir -p "$WEB/api/collector"
name "$V_NEW" "$NEW_SHA"
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
# Directives only: the header comment names the placeholder on purpose.
if grep -v '^[[:space:]]*#' "/etc/systemd/system/$UNIT" | grep -q CHANGE_ME; then
  no "every placeholder in the unit is substituted" "$(grep -n CHANGE_ME "/etc/systemd/system/$UNIT" | grep -v ':[[:space:]]*#')"
else
  ok "every placeholder in the unit is substituted"
fi
$SUDO systemctl daemon-reload
if out="$($SUDO systemd-analyze verify "/etc/systemd/system/$UNIT_INST" 2>&1)" && ! printf '%s' "$out" | grep -qi "unknown\|error"; then
  ok "systemd-analyze verify accepts the unit"
else
  no "systemd-analyze verify accepts the unit" "$out"
fi

$SUDO systemctl start "$UNIT_INST"

# --- 1. starts inside the sandbox, on the old build ---
if wait_for 20 "version=$V_OLD"; then ok "the collector starts under the hardened unit"
else no "the collector starts under the hardened unit" "$(journal | tail -20)"; fi

# --- 2. self-update writes the new binary through ReadWritePaths ---
if wait_for 60 "version=$V_NEW"; then ok "self-update replaces the binary and systemd restarts into it"
else no "self-update replaces the binary and systemd restarts into it" "$(journal | tail -20)"; fi
if [ "$($SUDO sha256sum "$DIR/collector" | awk '{print $1}')" = "$NEW_SHA" ]; then ok "the installed binary is the named build"
else no "the installed binary is the named build"; fi
if journal | grep -qF "self-update failed"; then no "no self-update failure in the log" "$(journal | grep -F 'self-update failed')"
else ok "no self-update failure in the log"; fi
if journal | grep -qF "$V_NEW is signed by release key SHA256:"; then ok "the release's signature was verified before the install"
else no "the release's signature was verified before the install" "$(journal | tail -20)"; fi
left="$($SUDO find "$DIR" -name '.collector-update*' | head -1)"
if [ -z "$left" ]; then ok "no update temp file is left behind"; else no "no update temp file is left behind" "$left"; fi
if wait_for 30 "update to $V_NEW proven"; then ok "the new build's first answer from the hub proves it"
else no "the new build's first answer from the hub proves it" "$(journal | tail -20)"; fi
if $SUDO test -e "$DIR/collector.prev" || $SUDO test -e "$DIR/collector.trial"; then
  no "proof removes the previous binary and the trial" "$($SUDO ls -la "$DIR")"
else
  ok "proof removes the previous binary and the trial"
fi

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

# --- 6. a release signed by any other key is refused, untouched ---
name "$V_FORGED" "$FORGED_SHA"
$SUDO systemctl restart "$UNIT_INST"   # the hourly config check, now
if wait_for 30 "self-update REFUSED $V_FORGED"; then ok "a release signed by an untrusted key is refused"
else no "a release signed by an untrusted key is refused" "$(journal | tail -20)"; fi
if [ "$($SUDO sha256sum "$DIR/collector" | awk '{print $1}')" = "$NEW_SHA" ]; then ok "the refused release never touched the binary"
else no "the refused release never touched the binary"; fi
if grep -qF "GET /releases/download/$V_FORGED/$ASSET " "$WEB/http.log"; then no "the refused release's binary was never downloaded"
else ok "the refused release's binary was never downloaded"; fi
if $SUDO test -e "$DIR/collector.refused" || $SUDO test -e "$DIR/collector.trial"; then
  no "a signature failure leaves no refusal or trial" "$($SUDO ls -la "$DIR")"
else
  ok "a signature failure leaves no refusal or trial"
fi

# --- 7. a release that crashes at startup is rolled back ---
# The rollback needs every start systemd allows (5 in 10 s by default:
# update exit, three crashes, the restored build). Let the window of the
# restart in step 6 pass first, or it counts against them.
sleep 11
$SUDO systemctl reset-failed "$UNIT_INST" >/dev/null 2>&1
name "$V_BAD" "$BAD_SHA"
$SUDO systemctl restart "$UNIT_INST"   # the hourly config check, now
if wait_for 30 "gateway up (fake) version=$V_BAD"; then ok "the bad release passes the self-check and is installed"
else no "the bad release passes the self-check and is installed" "$(journal | tail -20)"; fi
if wait_for 120 "REFUSING update to $V_BAD"; then ok "after the rollback, the restored build refuses the bad version"
else no "after the rollback, the restored build refuses the bad version" "$(journal | tail -30)"; fi
if journal | grep -qF "ROLLED BACK: $V_BAD crashed 3 times"; then ok "the rollback is logged loudly"
else no "the rollback is logged loudly" "$(journal | tail -30)"; fi
# A Go panic exits 2, which RestartPreventExitStatus=2 would take as a
# configuration error and stop on; a trial panic must die by SIGABRT.
if journal | grep -q "status=2/INVALIDARGUMENT"; then no "the crashing release was restarted, never stopped as exit 2" "$(journal | grep status=)"
elif journal | grep -q "status=6/ABRT"; then ok "the crashing release was restarted, never stopped as exit 2"
else no "the crashing release was restarted, never stopped as exit 2" "$(journal | grep -i "status=" | tail -5)"; fi
if [ "$($SUDO sha256sum "$DIR/collector" | awk '{print $1}')" = "$NEW_SHA" ]; then ok "the previous build is back in place"
else no "the previous build is back in place"; fi
sleep 3
state="$($SUDO systemctl show -p ActiveState --value "$UNIT_INST")"
last_up="$(journal | grep -F "gateway up" | tail -1)"
after_rollback="$(journal | sed -n '/ROLLED BACK/,$p' | grep -cF "version=$V_NEW")"
if [ "$state" = "active" ] && [ "$after_rollback" -ge 1 ] && printf '%s' "$last_up" | grep -qF "version=$V_NEW"; then
  ok "the previous build is running"
else
  no "the previous build is running" "state=$state last start: $last_up"
fi
if $SUDO grep -qF "\"version\":\"$V_BAD\"" "$DIR/collector.refused" 2>/dev/null; then ok "the refusal names exactly the bad version"
else no "the refusal names exactly the bad version" "$($SUDO cat "$DIR/collector.refused" 2>&1)"; fi
if $SUDO test -e "$DIR/collector.prev" || $SUDO test -e "$DIR/collector.trial"; then
  no "no trial or previous binary is left after the rollback" "$($SUDO ls -la "$DIR")"
else
  ok "no trial or previous binary is left after the rollback"
fi
downloads="$(grep -cF "GET /releases/download/$V_BAD/$ASSET " "$WEB/http.log")"
if [ "$downloads" = "1" ]; then ok "the refused version is not downloaded again"
else no "the refused version is not downloaded again" "$downloads downloads"; fi

# --- 8. exit 2 stops the unit rather than restart-looping ---
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
