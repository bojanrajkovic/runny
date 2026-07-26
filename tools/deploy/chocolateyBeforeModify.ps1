# chocolateyBeforeModify.ps1 — runs BEFORE Chocolatey modifies this package's
# files, on upgrade AND uninstall, from the OLD package's copy of this script.
#
# Why this has to exist: `runnyctl install-daemon` registers the service against
# lib\runny\tools\runnyd.exe — the very directory Chocolatey moves to lib-bkp on
# upgrade. Windows locks the image file of a running process, so upgrading while
# the service is up fails partway and can leave the package directory half
# swapped. This is the only hook that runs early enough to prevent that.
#
# Chocolatey treats a hook failure as non-blocking: it logs and carries on into
# the file swap regardless. So this must not "try and give up" — if the graceful
# drain does not finish, it escalates to a hard stop, because proceeding with the
# service still running is the one outcome we cannot allow.
#
# No-ops cleanly when the service does not exist, which is the normal case for
# anyone who installed the package only for runnyctl.

$ErrorActionPreference = 'Continue'

$svcName = 'runnyd'
$drainTimeout = [TimeSpan]::FromMinutes(10)

$svc = Get-Service -Name $svcName -ErrorAction SilentlyContinue
if (-not $svc) {
    Write-Host "runny: no $svcName service registered; nothing to stop."
    return
}
if ($svc.Status -eq 'Stopped') {
    Write-Host "runny: $svcName is already stopped."
    return
}

# Record whether it was running so chocolateyInstall.ps1 knows to start it again.
# Under the package dir, which survives the lib -> lib-bkp move.
$flag = Join-Path $env:ChocolateyPackageFolder '.runnyd-was-running'
try { New-Item -ItemType File -Path $flag -Force | Out-Null } catch {
    Write-Warning "runny: could not record service state at $flag; you may need to start $svcName by hand after the upgrade."
}

Write-Host "runny: stopping $svcName so its binary can be replaced (the daemon drains its fleet first; this can take a few minutes)..."
$sw = [Diagnostics.Stopwatch]::StartNew()
Stop-Service -Name $svcName -ErrorAction Continue

while ($sw.Elapsed -lt $drainTimeout) {
    $s = Get-Service -Name $svcName -ErrorAction SilentlyContinue
    if (-not $s -or $s.Status -eq 'Stopped') { break }
    Start-Sleep -Seconds 2
}
$sw.Stop()

$s = Get-Service -Name $svcName -ErrorAction SilentlyContinue
if ($s -and $s.Status -ne 'Stopped') {
    # The drain did not finish in time. Chocolatey will proceed into the file
    # swap whatever we do here, and swapping under a live process is the failure
    # this hook exists to prevent -- so kill it rather than leave it running.
    Write-Warning "runny: $svcName did not stop within $($drainTimeout.TotalMinutes) minutes; terminating it so the upgrade does not swap files under a live process."
    Get-CimInstance Win32_Service -Filter "Name='$svcName'" |
        ForEach-Object { if ($_.ProcessId) { Stop-Process -Id $_.ProcessId -Force -ErrorAction Continue } }
    Start-Sleep -Seconds 3
}

$s = Get-Service -Name $svcName -ErrorAction SilentlyContinue
if ($s -and $s.Status -ne 'Stopped') {
    Write-Error "runny: could not stop $svcName. Stop it manually and re-run the upgrade -- continuing now would replace a running binary and leave the package half swapped."
} else {
    Write-Host "runny: $svcName stopped after $([int]$sw.Elapsed.TotalSeconds)s."
}
