# chocolateyInstall.ps1 — runs after Chocolatey has placed this package's files.
#
# Restarts the runnyd service only if chocolateyBeforeModify.ps1 stopped it for
# this upgrade. A fresh install leaves no flag and no registered service, so
# nothing starts: the package deliberately does not register a service, that is
# `runnyctl install-daemon`'s job (ADR-0023's privilege boundary — installing a
# system daemon is an explicit, elevated operator action, not a side effect of
# unpacking a package).
#
# The service's registered binary path (lib\runny\tools\runnyd.exe) is stable
# across upgrades, so nothing needs re-registering — the new bytes are simply at
# the path the SCM already points at.

$ErrorActionPreference = 'Continue'

$svcName = 'runnyd'
$flag = Join-Path $env:ChocolateyPackageFolder '.runnyd-was-running'

if (-not (Test-Path $flag)) {
    Write-Host "runny: $svcName was not running before this operation; leaving it alone."
    return
}
Remove-Item $flag -Force -ErrorAction SilentlyContinue

$svc = Get-Service -Name $svcName -ErrorAction SilentlyContinue
if (-not $svc) {
    Write-Warning "runny: $svcName was running before the upgrade but is no longer registered; run 'runnyctl install-daemon' from an elevated prompt."
    return
}

Write-Host "runny: restarting $svcName..."
Start-Service -Name $svcName -ErrorAction Continue
Start-Sleep -Seconds 3

$svc = Get-Service -Name $svcName -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -eq 'Running') {
    Write-Host "runny: $svcName is running."
} else {
    # Loud, not silent: the operator's fleet is down and only they can see why.
    Write-Warning "runny: $svcName did not come back up. Check C:\ProgramData\runny\logs\service.err.log and 'runnyctl doctor'; until a valid config is in place it crash-loops by design."
}
