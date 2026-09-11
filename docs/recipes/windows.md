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
irm https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/install.ps1 | iex
.\collector.exe doctor
```

Notes:

- The task runs at **logon**, so a PC that reboots to the login screen
  is a collector stopped until someone signs in. For a machine nobody
  sits at, change the task's trigger to "At startup" in Task Scheduler
  and set it to run whether the user is logged on or not.
- Self-update renames the running `.exe` aside and cleans it up at the
  next start; the `collector.exe.old` you may see is expected.
- Doctor skips the file-mode check on Windows (no POSIX modes); keep the
  folder to yourself with normal NTFS permissions.
