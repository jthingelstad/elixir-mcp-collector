# macOS

Free on a Mac that stays awake; your home or office IP (static, or
stable enough to update the key when it moves); Apple Silicon or Intel;
a launchd agent the installer registers.

```sh
mkdir -p ~/elixir-collector && cd ~/elixir-collector
printf 'CR_API_TOKEN=%s\nELIXIR_API_TOKEN=%s\n' "your-cr-key" "emcg_your-token" > .env
chmod 600 .env
curl -fsSL https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/install.sh | sh
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
