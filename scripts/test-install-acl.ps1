# Tests the .env ACL block of scripts/install.ps1 on a real Windows host
# (CI: windows-latest). Runs only the lines between "# BEGIN env-acl" and
# "# END env-acl" against a scratch .env that starts out readable by
# Everyone, then reads the ACL back. Nothing is downloaded or registered.
#
#   pwsh -File scripts/test-install-acl.ps1
$ErrorActionPreference = "Stop"
$src   = Get-Content -Raw (Join-Path $PSScriptRoot "install.ps1")
$begin = $src.IndexOf("# BEGIN env-acl")
$end   = $src.IndexOf("# END env-acl")
if ($begin -lt 0 -or $end -le $begin) { throw "env-acl markers not found in install.ps1" }
$block = $src.Substring($begin, $end - $begin)

$dir = Join-Path ([System.IO.Path]::GetTempPath()) ("acl-test-" + [guid]::NewGuid())
New-Item -ItemType Directory $dir | Out-Null
$failed = 0
try {
  $envPath = Join-Path $dir ".env"
  Set-Content -LiteralPath $envPath -Value "CR_API_TOKEN=x"
  # Start loose: an explicit Everyone entry, on top of whatever the temp
  # folder lets the file inherit.
  $everyone = New-Object System.Security.Principal.SecurityIdentifier("S-1-1-0")
  $loose = Get-Acl -LiteralPath $envPath
  $loose.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule($everyone, "Read", "Allow")))
  Set-Acl -LiteralPath $envPath -AclObject $loose

  & ([scriptblock]::Create($block))

  $me     = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
  $system = New-Object System.Security.Principal.SecurityIdentifier("S-1-5-18")
  $acl    = Get-Acl -LiteralPath $envPath
  $rules  = @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))
  $ids    = @($rules | ForEach-Object { $_.IdentityReference.Value } | Sort-Object -Unique)

  if ($acl.AreAccessRulesProtected) { Write-Host "ok   - inheritance is off" }
  else { Write-Host "FAIL - inheritance is still on"; $failed++ }

  if (@($rules | Where-Object { $_.IsInherited }).Count -eq 0) { Write-Host "ok   - no inherited entries" }
  else { Write-Host "FAIL - inherited entries remain"; $failed++ }

  $want = @($me.Value, $system.Value) | Sort-Object -Unique
  if ((Compare-Object $ids $want) -eq $null) { Write-Host "ok   - exactly you and SYSTEM: $($ids -join ', ')" }
  else { Write-Host "FAIL - entries are $($ids -join ', '), want $($want -join ', ')"; $failed++ }

  # The owner can still read it (the collector runs as this account).
  if ((Get-Content -LiteralPath $envPath) -eq "CR_API_TOKEN=x") { Write-Host "ok   - still readable by you" }
  else { Write-Host "FAIL - unreadable after tightening"; $failed++ }
} finally {
  Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue
}
if ($failed -ne 0) { throw "$failed ACL check(s) failed" }
Write-Host "all ACL checks passed"
