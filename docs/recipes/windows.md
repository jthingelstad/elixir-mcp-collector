# Windows

Free on a PC that stays on; your home or office IP; x64 or ARM64; a
Scheduled Task the installer registers (starts at logon, restarts on
failure).

In PowerShell, in a folder of your choosing:

```powershell
mkdir $HOME\elixir-collector; cd $HOME\elixir-collector
@"
CR_API_TOKEN=your-cr-key
ELIXIR_API_TOKEN=emcg_your-token
"@ | Set-Content -NoNewline .env
```

Then run the README's Windows block ("3. Run it" → Windows). It
downloads `install.ps1` with the release's signed `SHA256SUMS`, checks
both with `ssh-keygen` and `Get-FileHash`, and only then runs the
installer. Finish with:

```powershell
.\collector.exe doctor
```

Notes:

- The task runs at **logon**, so a PC that reboots to the login screen
  is a collector stopped until someone signs in. For a machine nobody
  sits at, change the task's trigger to "At startup" in Task Scheduler
  and set it to run whether the user is logged on or not.
- Self-update renames the running `.exe` aside as `collector.exe.prev`
  and deletes it once the new version has reached Elixir MCP; a
  version that cannot start is rolled back to it (README, "Staying
  current"). `collector.exe.prev`, `collector.exe.trial`,
  `collector.exe.refused` and `collector.exe.failed` are the updater's;
  leave them be. An install from before 2026-09-26 should re-run the
  installer once, for the `run-collector.cmd` line that covers a power
  cut mid-update.
- The installer restricts `.env` to your account and SYSTEM (inheritance
  off), and checks the result. If you change the task to run as a
  different account, give that account read access to `.env` too, or
  the collector will not find its tokens. Doctor does not check Windows
  ACLs; `icacls .env` shows what is set.
