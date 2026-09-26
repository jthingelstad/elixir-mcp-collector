#!/bin/sh
# Tests for scripts/next-version.sh: the version each release build
# publishes from the tags already in the repository.
#
#   sh scripts/test-next-version.sh

set -u
SELF_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "ok   - $1"; }
no() { FAIL=$((FAIL + 1)); echo "FAIL - $1"; [ -n "${2:-}" ] && echo "       $2"; }

# expect <description> <want> <tags...>   (want "!" = must refuse)
expect() {
  what="$1"; want="$2"; shift 2
  got="$(for t in "$@"; do printf 'deadbeef\trefs/tags/%s\n' "$t"; done | sh "$SELF_DIR/next-version.sh" 3 0 2>/dev/null)"
  code=$?
  if [ "$want" = "!" ]; then
    if [ "$code" -ne 0 ] && [ -z "$got" ]; then ok "$what"; else no "$what" "printed '$got', exit $code"; fi
  elif [ "$code" -eq 0 ] && [ "$got" = "$want" ]; then ok "$what"
  else no "$what" "got '$got' (exit $code), want $want"; fi
}

expect "the merge that moves the major builds exactly v3.0.0" v3.0.0 v2.0.1 v2.0.9 v2.0.34 v2.0.35
expect "the next green push builds v3.0.1" v3.0.1 v2.0.35 v3.0.0
expect "and the one after, v3.0.2" v3.0.2 v2.0.35 v3.0.0 v3.0.1
expect "patches compare as numbers, not strings" v3.0.11 v3.0.9 v3.0.10 v3.0.2
expect "a gap is never refilled: the next is above the highest used" v3.0.6 v3.0.0 v3.0.5
expect "tags that are not releases are ignored" v3.0.1 v3.0.0 v3.0.7-smoke 3.0.9 v3.0.8.1 latest
expect "no tags at all starts the series" v3.0.0
expect "a higher release tag already there refuses" ! v2.0.35 v3.1.0
expect "a higher major refuses" ! v4.0.0 v3.0.3
expect "every v2.0.x is below v3.0.0, however large its patch" v3.0.0 v2.0.999999

# bare tag names work too, and git ls-remote's real shape
got="$(printf 'v3.0.0\nv3.0.1\n' | sh "$SELF_DIR/next-version.sh" 3 0)"
[ "$got" = "v3.0.2" ] && ok "bare tag names are read" || no "bare tag names are read" "$got"
if sh "$SELF_DIR/next-version.sh" 3 x </dev/null >/dev/null 2>&1; then no "a non-numeric series is refused"; else ok "a non-numeric series is refused"; fi

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
