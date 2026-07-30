#!/usr/bin/env bash
# Remove the non-root system LaunchDaemon. Delegates to
# `runnyctl uninstall-daemon`, which verifies the job is unloaded, then removes
# the plist AND the home /Library/Application Support/runny — config, the App
# key, and artifacts go with it. BACK UP config.yaml first if you want to keep
# it. Only the _runny service account is kept (so a reinstall reuses its uid).
# Prefers the staged runnyctl from install-system.sh, else PATH.
set -euo pipefail

RUNNYCTL="${RUNNYCTL:-/usr/local/libexec/runny/runnyctl}"
[ -x "$RUNNYCTL" ] || RUNNYCTL="$(command -v runnyctl || true)"

[ "$(id -u)" -eq 0 ] || { echo "run via sudo — removing a system LaunchDaemon is privileged" >&2; exit 1; }
[ -n "$RUNNYCTL" ] || { echo "runnyctl not found; set RUNNYCTL=/path/to/runnyctl" >&2; exit 1; }

exec "$RUNNYCTL" uninstall-daemon
