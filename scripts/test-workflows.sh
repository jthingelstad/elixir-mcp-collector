#!/bin/sh
# Keeps the workflows as issue #5 left them: every action pinned to a
# full commit SHA, no default token permissions, every job asking for
# its own, and no checkout leaving the token in .git/config.
#
#   sh scripts/test-workflows.sh

set -u
SELF_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
WF="$(dirname "$SELF_DIR")/.github/workflows"

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "ok   - $1"; }
no() { FAIL=$((FAIL + 1)); echo "FAIL - $1"; [ -n "${2:-}" ] && echo "       $2"; }

for f in "$WF"/*.yml; do
  name="$(basename "$f")"
  unpinned="$(grep -nE '^[[:space:]]*-?[[:space:]]*uses:' "$f" | grep -vE 'uses:[[:space:]]*[^@[:space:]]+@[0-9a-f]{40}([[:space:]]|$)')"
  if [ -z "$unpinned" ]; then ok "$name: every action is pinned to a commit SHA"
  else no "$name: every action is pinned to a commit SHA" "$unpinned"; fi

  if grep -qx 'permissions: {}' "$f"; then ok "$name: no default token permissions"
  else no "$name: no default token permissions" "want a top-level 'permissions: {}'"; fi

  # Each job (two-space key under jobs:) must carry its own permissions.
  missing="$(awk '
    /^jobs:/ { in_jobs = 1; next }
    in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
      if (job != "" && !seen) print job
      job = $1; seen = 0; next
    }
    in_jobs && /^    permissions:/ { seen = 1 }
    END { if (job != "" && !seen) print job }
  ' "$f")"
  if [ -z "$missing" ]; then ok "$name: every job sets its own permissions"
  else no "$name: every job sets its own permissions" "$missing"; fi

  checkouts="$(grep -c 'uses: actions/checkout@' "$f")"
  kept="$(grep -c 'persist-credentials: false' "$f")"
  if [ "$checkouts" = "$kept" ]; then ok "$name: no checkout persists the token"
  else no "$name: no checkout persists the token" "$checkouts checkouts, $kept with persist-credentials: false"; fi
done

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
