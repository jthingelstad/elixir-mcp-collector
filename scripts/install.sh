#!/bin/sh
# One-command install for the Elixir MCP collector (Go binary).
# Downloads the latest release binary for this platform, drops it next
# to your .env, and installs a supervisor service (launchd on macOS,
# systemd on Linux) that keeps it running and restarts it on self-update.
#
#   sh scripts/install.sh        # from a checkout, OR
#   curl -fsSL https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/install.sh | sh
#
# Requires: a .env in the current directory (see .env.example) with
# CR_API_TOKEN and ELIXIR_API_TOKEN. No Node, no AWS, no git needed.
set -eu

REPO="jthingelstad/elixir-mcp-collector"
DIR="$(pwd)"
[ -f "$DIR/.env" ] || { echo "No .env here. Copy .env.example to .env and fill in CR_API_TOKEN + ELIXIR_API_TOKEN first."; exit 1; }

os="$(uname -s)"; arch="$(uname -m)"
case "$os-$arch" in
  Darwin-arm64)  asset=collector_darwin_arm64 ;;   # Apple Silicon
  Darwin-x86_64) asset=collector_darwin_amd64 ;;   # Intel Mac
  Linux-aarch64|Linux-arm64) asset=collector_linux_arm64 ;;
  Linux-x86_64)  asset=collector_linux_amd64 ;;
  Linux-armv7l)  asset=collector_linux_armv7 ;;
  *) echo "No prebuilt binary for $os-$arch."
     echo "On Windows, use scripts/install.ps1 in PowerShell instead."
     echo "Otherwise build from source: go build -o collector ./cmd/collector"
     exit 1 ;;
esac

# Resolve the release ONCE and pull both files from that exact tag.
# Fetching the binary and its checksums from /latest/ separately means a
# promotion between the two requests hands you a checksum file for a
# different build.
tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
  | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
[ -n "$tag" ] || { echo "Could not resolve the latest release of $REPO. Check your network and try again."; exit 1; }
base="https://github.com/$REPO/releases/download/$tag"

echo "Downloading $asset ($tag)..."
tmp="$DIR/.collector.$$"
sums="$DIR/.sums.$$"
cleanup() { rm -f "$tmp" "$sums"; }
trap cleanup EXIT INT TERM

# Everything below refuses rather than warns. An unverified collector is
# not a degraded install, it is an unknown binary, and it must never
# reach the point of replacing one that is already working.
curl -fSL "$base/$asset" -o "$tmp" || { echo "Download failed: $base/$asset"; exit 1; }
curl -fsSL "$base/SHA256SUMS" -o "$sums" || { echo "Could not download SHA256SUMS for $tag - refusing to install unverified."; exit 1; }

want="$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1 }' "$sums")"
lines="$(printf '%s\n' "$want" | grep -c '[0-9a-f]' || true)"
if [ "$lines" -ne 1 ]; then
  echo "SHA256SUMS for $tag has $lines entries for $asset (expected exactly 1) - refusing to install."
  exit 1
fi
case "$want" in
  [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
  *) echo "Malformed checksum for $asset in $tag - refusing to install."; exit 1 ;;
esac

got="$( (shasum -a 256 "$tmp" 2>/dev/null || sha256sum "$tmp") | awk '{print $1}')"
[ -n "$got" ] || { echo "Could not compute a checksum locally (no shasum or sha256sum) - refusing to install."; exit 1; }
if [ "$want" != "$got" ]; then
  echo "SHA256 mismatch for $asset in $tag - refusing to install."
  echo "  expected $want"
  echo "  got      $got"
  exit 1
fi
echo "SHA256 verified against $tag."

# Verified: only now replace whatever is already installed.
chmod +x "$tmp"
mv -f "$tmp" "$DIR/collector"

if [ "$os" = "Darwin" ]; then
  LABEL="com.poapkings.elixir-mcp-collector"
  PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
  cat > "$PLIST" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key><array><string>$DIR/collector</string></array>
  <key>EnvironmentVariables</key><dict><key>ELIXIR_MCP_ENV_FILE</key><string>$DIR/.env</string></dict>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/elixir-mcp-collector.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/elixir-mcp-collector.log</string>
</dict></plist>
PL
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST"
  echo "Installed + started. Logs: ~/Library/Logs/elixir-mcp-collector.log"
else
  echo "Binary at $DIR/collector. To supervise with systemd, edit and install scripts/elixir-collector.service,"
  echo "or run under any supervisor: ELIXIR_MCP_ENV_FILE=$DIR/.env $DIR/collector"
  echo "(scripts/run-forever.sh is a plain KeepAlive loop for hosts without a supervisor.)"
fi
