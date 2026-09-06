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

# Resolve the release ONCE so the binary and its checksums cannot come
# from two different builds if Latest moves between the requests.
# ($ErrorActionPreference is already Stop at the top of this script, so
# a failed request throws rather than warning and carrying on.)
$tag = (Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest").tag_name
if (-not $tag) { throw "Could not resolve the latest release of $repo." }
$base = "https://github.com/$repo/releases/download/$tag"

Write-Host "Downloading $asset ($tag)..."
# Download beside the target, never onto it: a failed or unverified
# install must leave a working collector exactly where it was.
$tmp  = "$dir\.collector.$PID.exe"
$sums = "$dir\.sums.$PID"
try {
  Invoke-WebRequest -Uri "$base/$asset" -OutFile $tmp
  try {
    Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sums
  } catch {
    throw "Could not download SHA256SUMS for $tag - refusing to install unverified."
  }

  $sumLines = @(Select-String -Path $sums -Pattern "\s\*?$([regex]::Escape($asset))$")
  if ($sumLines.Count -ne 1) {
    throw "SHA256SUMS for $tag has $($sumLines.Count) entries for $asset (expected exactly 1) - refusing to install."
  }
  $want = $sumLines[0].Line.Split(" ")[0].ToLower()
  if ($want -notmatch '^[0-9a-f]{64}$') {
    throw "Malformed checksum for $asset in $tag - refusing to install."
  }
  $got = (Get-FileHash -Algorithm SHA256 $tmp).Hash.ToLower()
  if ($want -ne $got) {
    throw "SHA256 mismatch for $asset in $tag - refusing to install.`n  expected $want`n  got      $got"
  }
  Write-Host "SHA256 verified against $tag."

  # Verified: only now replace whatever is already installed.
  Move-Item -Force $tmp $exe
} finally {
  Remove-Item $tmp, $sums -ErrorAction SilentlyContinue
}

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
