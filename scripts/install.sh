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
  Darwin-arm64)  asset=collector_darwin_arm64 ;;
  Darwin-x86_64) asset=collector_darwin_arm64 ;; # Rosetta runs arm64
  Linux-aarch64|Linux-arm64) asset=collector_linux_arm64 ;;
  Linux-x86_64)  asset=collector_linux_amd64 ;;
  Linux-armv7l)  asset=collector_linux_armv7 ;;
  *) echo "No prebuilt binary for $os-$arch. Build from source: go build -o collector ./cmd/collector"; exit 1 ;;
esac

echo "Downloading $asset (latest release)..."
url="https://github.com/$REPO/releases/latest/download/$asset"
curl -fSL "$url" -o "$DIR/collector"
chmod +x "$DIR/collector"

# Verify against the release SHA256SUMS when available (best effort).
if curl -fsSL "https://github.com/$REPO/releases/latest/download/SHA256SUMS" -o "$DIR/.sums" 2>/dev/null; then
  want="$(grep " $asset\$" "$DIR/.sums" | awk '{print $1}')"
  got="$( (shasum -a 256 "$DIR/collector" 2>/dev/null || sha256sum "$DIR/collector") | awk '{print $1}')"
  rm -f "$DIR/.sums"
  [ -z "$want" ] || [ "$want" = "$got" ] || { echo "SHA256 mismatch — refusing to install."; exit 1; }
  echo "SHA256 verified."
fi

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
