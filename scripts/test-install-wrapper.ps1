# Tests the run-collector.cmd block of scripts/install.ps1 on a real
# Windows host (CI: windows-latest). Runs only the lines between
# "# BEGIN wrapper" and "# END wrapper" in a scratch folder, with a
# stand-in collector.exe (a copy of hostname.exe), then runs the wrapper.
# Nothing is downloaded or registered.
#
#   pwsh -File scripts/test-install-wrapper.ps1
$ErrorActionPreference = "Stop"
$src   = Get-Content -Raw (Join-Path $PSScriptRoot "install.ps1")
$begin = $src.IndexOf("# BEGIN wrapper")
$end   = $src.IndexOf("# END wrapper")
if ($begin -lt 0 -or $end -le $begin) { throw "wrapper markers not found in install.ps1" }
$block = $src.Substring($begin, $end - $begin)

$dir = Join-Path ([System.IO.Path]::GetTempPath()) ("wrapper-test-" + [guid]::NewGuid())
New-Item -ItemType Directory $dir | Out-Null
$failed = 0
function Check($ok, $what) {
  if ($ok) { Write-Host "ok   - $what" } else { Write-Host "FAIL - $what"; $script:failed++ }
}
try {
  $exe   = "$dir\collector.exe"
  $stand = "$env:SystemRoot\System32\hostname.exe"
  & ([scriptblock]::Create($block))
  Check (Test-Path "$dir\run-collector.cmd") "the wrapper is written"

  # A power cut between the two renames: no collector.exe, only .prev.
  Copy-Item $stand "$exe.prev"
  $out = & cmd.exe /c "`"$dir\run-collector.cmd`""
  Check ($LASTEXITCODE -eq 0 -and "$out".Trim() -eq $env:COMPUTERNAME) "the wrapper starts the collector (exit $LASTEXITCODE, output '$out')"
  Check (Test-Path $exe) "a missing collector.exe is restored from collector.exe.prev"
  Check (-not (Test-Path "$exe.prev")) "collector.exe.prev was moved, not copied"

  # An update on trial: both exist. The previous build must stay put.
  Copy-Item $stand "$exe.prev"
  & cmd.exe /c "`"$dir\run-collector.cmd`"" | Out-Null
  Check (Test-Path "$exe.prev") "with collector.exe present, collector.exe.prev is left alone"
} finally {
  Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue
}
if ($failed -ne 0) { throw "$failed wrapper check(s) failed" }
Write-Host "all wrapper checks passed"
