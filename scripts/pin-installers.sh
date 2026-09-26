#!/bin/sh
# Writes the copies of install.sh and install.ps1 that a release
# publishes (release.yml, issue #5): each is pinned to that release's
# tag and carries that release's binary checksums, so an installer an
# operator has verified against the signed SHA256SUMS needs to trust
# nothing it downloads afterwards. The copies in the repository keep
# the markers empty and resolve the Latest release instead.
#
#   sh scripts/pin-installers.sh <tag> <binary-sums-file> <out-dir>
#
# <binary-sums-file> is sha256sum output for the collector_* binaries.
set -eu

[ $# -eq 3 ] || { echo "usage: $0 <tag> <binary-sums-file> <out-dir>" >&2; exit 2; }
tag="$1"; sums_file="$2"; out="$3"
SELF_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"

printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' \
  || { echo "not a release tag: $tag" >&2; exit 1; }
# Only well-formed lines are baked in: the value lands inside quotes in
# two scripts, so nothing but hex, spaces and asset names may reach it.
bad="$(grep -Ev '^[0-9a-f]{64}  collector_[a-z0-9_]+(\.exe)?$' "$sums_file" || true)"
[ -z "$bad" ] || { echo "unexpected line in $sums_file: $bad" >&2; exit 1; }
[ -s "$sums_file" ] || { echo "$sums_file is empty" >&2; exit 1; }

# pin <src> <dst> <tag marker> <pinned tag line> <sums marker> <open> <close>
# replaces the tag marker line, and the sums marker line with <open>, the
# sums lines, <close>. Each marker must appear exactly once, or the
# output would be a half-pinned installer.
pin() {
  src="$1"; dst="$2"
  for marker in "$3" "$5"; do
    n="$(grep -cxF -- "$marker" "$src" || true)"
    [ "$n" = 1 ] || { echo "$src: marker '$marker' found $n times, want 1" >&2; exit 1; }
  done
  # The sums file's path goes through the environment: awk -v would
  # read the backslashes in a Windows path as escapes.
  SUMS_FILE="$sums_file" awk -v tagm="$3" -v tagv="$4" -v summ="$5" -v openq="$6" -v closeq="$7" '
    $0 == tagm { print tagv; next }
    $0 == summ {
      printf "%s", openq
      first = 1
      while ((r = (getline line < ENVIRON["SUMS_FILE"])) > 0) { printf "%s%s", (first ? "" : "\n"), line; first = 0 }
      if (r < 0 || first) { print "cannot read the checksums from " ENVIRON["SUMS_FILE"] > "/dev/stderr"; exit 1 }
      print closeq
      next
    }
    { print }
  ' "$src" > "$dst"
  # Never publish a half-pinned installer: every checksum must be in it.
  while IFS= read -r line; do
    grep -qF -- "$line" "$dst" || { echo "$dst: missing '$line' after pinning" >&2; exit 1; }
  done < "$sums_file"
}

mkdir -p "$out"
pin "$SELF_DIR/install.sh" "$out/install.sh" \
  'PINNED_TAG=""' "PINNED_TAG=\"$tag\"" \
  'PINNED_SUMS=""' 'PINNED_SUMS="' '"'
pin "$SELF_DIR/install.ps1" "$out/install.ps1" \
  "\$PinnedTag = ''" "\$PinnedTag = '$tag'" \
  "\$PinnedSums = ''" "\$PinnedSums = '" "'"
echo "pinned install.sh and install.ps1 to $tag"
