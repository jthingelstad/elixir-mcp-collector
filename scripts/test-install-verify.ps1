# Tests the download-and-verify block of scripts/install.ps1 (issue #5):
# the repository copy, which resolves Latest and checks SHA256SUMS, and
# the copy a release publishes, pinned by scripts/pin-installers.sh to
# its own tag and checksums, which must fetch nothing but the binary.
# Runs only the lines between "# BEGIN verify" and "# END verify", with
# Invoke-WebRequest and Invoke-RestMethod replaced by stand-ins, in a
# scratch folder. Nothing is downloaded or registered. Needs sh (Git for
# Windows' on a Windows host) to run pin-installers.sh.
#
#   pwsh -File scripts/test-install-verify.ps1
$ErrorActionPreference = "Stop"

function Get-VerifyBlock($path) {
  $src   = Get-Content -Raw $path
  $begin = $src.IndexOf("# BEGIN verify")
  $end   = $src.IndexOf("# END verify")
  if ($begin -lt 0 -or $end -le $begin) { throw "verify markers not found in $path" }
  return $src.Substring($begin, $end - $begin)
}

$failed = 0
function Check($ok, $what) {
  if ($ok) { Write-Host "ok   - $what" } else { Write-Host "FAIL - $what"; $script:failed++ }
}

$repo    = "jthingelstad/elixir-mcp-collector"
$asset   = "collector_windows_amd64.exe"
$payload = [System.Text.Encoding]::ASCII.GetBytes("pretend this is a collector")
$sha     = -join ([System.Security.Cryptography.SHA256]::Create().ComputeHash($payload) | ForEach-Object { $_.ToString("x2") })
$zeros   = "0" * 64

# Stand-ins: functions win over cmdlets of the same name.
$script:uris = @()
$script:sumsBody = $null
function Invoke-WebRequest {
  param([string]$Uri, [string]$OutFile)
  $script:uris += $Uri
  # install.ps1 writes Windows paths; PowerShell elsewhere reads them
  # with / (this only matters when the test runs off Windows).
  if ($IsLinux -or $IsMacOS) { $OutFile = $OutFile.Replace('\', '/') }
  if ($Uri -like "*/SHA256SUMS") {
    if ($null -eq $script:sumsBody) { throw "404 Not Found" }
    [System.IO.File]::WriteAllText($OutFile, $script:sumsBody)
  } else {
    [System.IO.File]::WriteAllBytes($OutFile, $payload)
  }
}
function Invoke-RestMethod {
  param([string]$Uri)
  $script:uris += $Uri
  [pscustomobject]@{ tag_name = "v9.9.9" }
}

# Run-Block runs a verify block in a fresh folder; it returns the error
# (or $null) and whether collector.exe was installed.
function Run-Block($block, $sums) {
  $script:uris = @()
  $script:sumsBody = $sums
  $dir = Join-Path ([System.IO.Path]::GetTempPath()) ("verify-test-" + [guid]::NewGuid())
  New-Item -ItemType Directory $dir | Out-Null
  $exe = "$dir\collector.exe"
  $err = $null
  try {
    & ([scriptblock]::Create($block)) | Out-Null
  } catch {
    $err = $_.Exception.Message
  }
  $installed = Test-Path -LiteralPath $exe
  $left = @(Get-ChildItem -Force -LiteralPath $dir | Where-Object { $_.Name -like "*.collector.*" -or $_.Name -like "*.sums.*" })
  Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue
  return [pscustomobject]@{ Err = $err; Installed = $installed; Left = $left.Count }
}

# --- the repository copy: Latest, then that release's SHA256SUMS ---
$repoBlock = Get-VerifyBlock (Join-Path $PSScriptRoot "install.ps1")
$r = Run-Block $repoBlock "$sha  $asset`n$zeros  collector_windows_arm64.exe`n"
Check ($null -eq $r.Err -and $r.Installed) "the repository copy installs a matching binary ($($r.Err))"
Check ($script:uris -contains "https://github.com/$repo/releases/download/v9.9.9/SHA256SUMS") "the repository copy checks Latest's SHA256SUMS"
# (Off Windows, Remove-Item does not resolve install.ps1's \ paths.)
$onWindows = [System.Environment]::OSVersion.Platform -eq "Win32NT"
if ($onWindows) { Check ($r.Left -eq 0) "no temp files are left behind" }
$r = Run-Block $repoBlock "$zeros  $asset`n"
Check ($r.Err -like "*SHA256 mismatch*" -and -not $r.Installed) "the repository copy refuses a mismatch ($($r.Err))"
$r = Run-Block $repoBlock $null
Check ($r.Err -like "*refusing to install unverified*" -and -not $r.Installed) "the repository copy refuses without SHA256SUMS ($($r.Err))"

# --- the published copy, pinned the way release.yml pins it ---
$sh = $null
foreach ($candidate in @("$env:ProgramFiles\Git\bin\sh.exe", "$env:ProgramFiles\Git\usr\bin\sh.exe")) {
  if ($env:ProgramFiles -and (Test-Path -LiteralPath $candidate)) { $sh = $candidate; break }
}
if (-not $sh) { $found = Get-Command sh -ErrorAction SilentlyContinue; if ($found) { $sh = $found.Source } }
if (-not $sh) { throw "no sh to run pin-installers.sh with" }

function Pin($sumsLine) {
  $work = Join-Path ([System.IO.Path]::GetTempPath()) ("pin-test-" + [guid]::NewGuid())
  New-Item -ItemType Directory $work | Out-Null
  [System.IO.File]::WriteAllText((Join-Path $work "binsums"), "$sumsLine`n")
  & $sh (Join-Path $PSScriptRoot "pin-installers.sh") v1.2.3 (Join-Path $work "binsums") (Join-Path $work "out") | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "pin-installers.sh exited $LASTEXITCODE" }
  $pinned = Join-Path $work "out/install.ps1"
  # The whole pinned script must still parse, not just the block.
  $errors = $null
  [System.Management.Automation.Language.Parser]::ParseFile($pinned, [ref]$null, [ref]$errors) | Out-Null
  $block = Get-VerifyBlock $pinned
  Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
  return [pscustomobject]@{ Block = $block; ParseErrors = @($errors).Count }
}

$p = Pin "$sha  $asset"
Check ($p.ParseErrors -eq 0) "the pinned install.ps1 parses"
$r = Run-Block $p.Block $null
Check ($null -eq $r.Err -and $r.Installed) "the pinned copy installs its own release's binary ($($r.Err))"
Check ($script:uris.Count -eq 1 -and $script:uris[0] -eq "https://github.com/$repo/releases/download/v1.2.3/$asset") "the pinned copy fetches only the binary, from its own tag ($($script:uris -join ', '))"
$p = Pin "$zeros  $asset"
$r = Run-Block $p.Block "$sha  $asset`n"
Check ($r.Err -like "*SHA256 mismatch*" -and -not $r.Installed) "the pinned copy trusts its baked checksum, not a downloaded one ($($r.Err))"

if ($failed -ne 0) { throw "$failed verify check(s) failed" }
Write-Host "all verify checks passed"
