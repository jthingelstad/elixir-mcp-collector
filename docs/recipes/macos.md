# macOS

Free on a Mac that stays awake; your home or office IP (static, or
stable enough to update the key when it moves); Apple Silicon or Intel;
a launchd agent the installer registers.

```sh
mkdir -p ~/elixir-collector && cd ~/elixir-collector
printf 'CR_API_TOKEN=%s\nELIXIR_API_TOKEN=%s\n' "your-cr-key" "emcg_your-token" > .env
chmod 600 .env
```

Then the installer, checked against the release signature before it
runs (README, "3. Run it", explains each line):

```sh
base=https://github.com/jthingelstad/elixir-mcp-collector/releases/latest/download
curl -fsSL -O "$base/install.sh" -O "$base/SHA256SUMS" -O "$base/SHA256SUMS.sig" &&
echo 'elixir-mcp-collector-release ssh-ed25519 REPLACE_WITH_THE_RELEASE_PUBLIC_KEY' > allowed_signers &&
ssh-keygen -Y verify -f allowed_signers -I elixir-mcp-collector-release \
  -n elixir-mcp-collector-release -s SHA256SUMS.sig < SHA256SUMS &&
grep ' install.sh$' SHA256SUMS | shasum -a 256 -c - &&
sh install.sh
./collector doctor
```

The installer writes `~/Library/LaunchAgents/com.poapkings.elixir-mcp-collector.plist`
with `KeepAlive`, so the collector restarts after crashes and after its
own self-update. Logs: `~/Library/Logs/elixir-mcp-collector.log`.

Two macOS-specific notes:

- **Sleep.** A LaunchAgent runs only while you are logged in and the
  Mac is awake. System Settings → Energy → "Prevent automatic sleeping
  when the display is off" (or `sudo pmset -a sleep 0` on a desktop).
  A laptop lid closed is a collector stopped.
- **Rosetta.** If you downloaded the Intel binary on Apple Silicon it
  runs, slowly; `collector doctor`'s `runtime` line says so. Rerun the
  installer, which picks the native build.
