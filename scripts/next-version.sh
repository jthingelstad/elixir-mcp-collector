#!/bin/sh
# Prints the version a release build publishes (release.yml): the next
# patch in the MAJOR.MINOR series that no tag in this repository has
# used yet. So the first build of a new series is vMAJOR.MINOR.0, each
# green push after it the next patch, and nobody tags anything by hand.
# A red build uses no number (a tag exists only once a release is
# published), and a number is never reused. It refuses a version that
# would not be above every release tag the repository has, because the
# hub compares major.minor.patch numerically and a lower number would
# read as a downgrade.
#
#   git ls-remote --tags --refs <repo-url> | sh scripts/next-version.sh 3 0
#
# Reads `git ls-remote` lines (or bare tag names) on stdin.
set -eu
[ $# -eq 2 ] || { echo "usage: $0 <major> <minor> < tags" >&2; exit 2; }
case "$1$2" in *[!0-9]*|"") echo "major and minor must be numbers" >&2; exit 2 ;; esac

awk -v M="$1" -v m="$2" '
  function above(a, b, c, x, y, z) { return a > x || (a == x && (b > y || (b == y && c > z))) }
  {
    t = $NF
    sub("^refs/tags/", "", t)
    if (t !~ /^v[0-9]+\.[0-9]+\.[0-9]+$/) next
    split(substr(t, 2), p, ".")
    a = p[1] + 0; b = p[2] + 0; c = p[3] + 0
    if (hi == "" || above(a, b, c, hiA, hiB, hiC)) { hiA = a; hiB = b; hiC = c; hi = t }
    if (a == M + 0 && b == m + 0 && c + 1 > nx) nx = c + 1
  }
  END {
    v = "v" (M + 0) "." (m + 0) "." (nx + 0)
    if (hi != "" && !above(M + 0, m + 0, nx + 0, hiA, hiB, hiC)) {
      print "refusing to build " v ": it is not above " hi ", the highest release tag" > "/dev/stderr"
      exit 1
    }
    print v
  }'
