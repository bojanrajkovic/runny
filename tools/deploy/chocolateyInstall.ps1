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

# Ask the NEW binary whether it accepts the config that is already in place,
# while the operator is still watching this upgrade. On darwin the equivalent
# question is asked before the handover (upgrade-daemon's exit gate consults the
# respawn target's -test-config); Windows cannot ask before the swap, because the
# new binary does not exist until Chocolatey has written it -- so this is the
# only moment the question can be put, and it is the same question.
#
# -test-config is side-effect-free and runs LOCAL checks only: no home, no lock,
# no network. So it cannot fail spuriously on a GitHub or registry blip, and a
# verdict here is about the config, not about the weather.
#
# Advisory, not a gate: the service still starts. Withholding it would leave the
# fleet down pending a second manual Start-Service, and would let a false
# negative do more damage than the crash-loop it was trying to prevent -- which
# runnyd documents and announces for itself. The value wanted here is the
# operator learning NOW rather than from a service that will not stay up.
$configPath = Join-Path $env:ProgramData 'runny\config.yaml'
$newBinary = Join-Path $env:ChocolateyPackageFolder 'tools\runnyd.exe'
if ((Test-Path $configPath) -and (Test-Path $newBinary)) {
    $raw = & $newBinary -test-config $configPath 2>&1 | Out-String
    try {
        $verdict = $raw | ConvertFrom-Json
        switch ($verdict.status) {
            'ok' { Write-Host "runny: the upgraded binary accepts $configPath." }
            'warn' {
                Write-Host "runny: the upgraded binary accepts $configPath with warnings:"
                $verdict.warnings | ForEach-Object { Write-Host "  - $($_.detail)" }
            }
            default {
                Write-Warning "runny: the upgraded binary REJECTS $configPath -- it will crash-loop until this is fixed:"
                $verdict.errors | ForEach-Object { Write-Warning "  - $_" }
                Write-Warning "runny: fix $configPath, then 'runnyctl doctor'. Starting the service anyway so it recovers on its own once the config is valid."
            }
        }
    } catch {
        # A binary that cannot produce a verdict is itself worth surfacing, but
        # it is not a reason to withhold the service.
        Write-Warning "runny: could not read a config verdict from the upgraded binary: $($raw.Trim())"
    }
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
