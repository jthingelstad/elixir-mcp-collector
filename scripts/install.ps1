# One-command install for the Elixir MCP collector on Windows (PowerShell).
# Downloads the latest release .exe for this machine, verifies its
# SHA-256, and registers a Scheduled Task (built into Windows - no extra
# tooling) that starts it at logon and restarts it if it stops.
#
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1
#
# Requires a .env in the current directory (see .env.example) with
# CR_API_TOKEN and ELIXIR_API_TOKEN. No Node, no AWS, no git needed.
$ErrorActionPreference = "Stop"
$repo = "jthingelstad/elixir-mcp-collector"
$dir  = (Get-Location).Path
if (-not (Test-Path "$dir\.env")) {
  Write-Error "No .env here. Copy .env.example to .env and fill in CR_API_TOKEN + ELIXIR_API_TOKEN first."
}

$arch = $env:PROCESSOR_ARCHITECTURE
switch ($arch) {
  "ARM64" { $asset = "collector_windows_arm64.exe" }
  default { $asset = "collector_windows_amd64.exe" }  # AMD64
}
$exe = "$dir\collector.exe"

Write-Host "Downloading $asset (latest release)..."
Invoke-WebRequest -Uri "https://github.com/$repo/releases/latest/download/$asset" -OutFile $exe

# Verify against the release SHA256SUMS (best effort).
try {
  Invoke-WebRequest -Uri "https://github.com/$repo/releases/latest/download/SHA256SUMS" -OutFile "$dir\.sums"
  $want = (Select-String -Path "$dir\.sums" -Pattern " $([regex]::Escape($asset))$").Line.Split(" ")[0]
  $got  = (Get-FileHash -Algorithm SHA256 $exe).Hash.ToLower()
  Remove-Item "$dir\.sums" -ErrorAction SilentlyContinue
  if ($want -and ($want -ne $got)) { Write-Error "SHA256 mismatch - refusing to install." }
  if ($want) { Write-Host "SHA256 verified." }
} catch { Write-Host "(Could not fetch SHA256SUMS; skipping verification.)" }

$taskName = "ElixirMCPCollector"
$env:ELIXIR_MCP_ENV_FILE = "$dir\.env"
$action  = New-ScheduledTaskAction -Execute $exe
$trigger = New-ScheduledTaskTrigger -AtLogOn
# Restart on failure, keep running indefinitely.
$settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable
# Pass the env-file location to the task's environment via a wrapper.
$wrapper = "$dir\run-collector.cmd"
"@echo off`r`nset ELIXIR_MCP_ENV_FILE=$dir\.env`r`n`"$exe`"" | Out-File -Encoding ascii $wrapper
$action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c `"$wrapper`""

Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings -Description "Elixir MCP collector" | Out-Null
Start-ScheduledTask -TaskName $taskName
Write-Host "Installed + started as Scheduled Task '$taskName'. Logs go to stdout;"
Write-Host "to capture them, edit run-collector.cmd to append '>> collector.log 2>&1'."
