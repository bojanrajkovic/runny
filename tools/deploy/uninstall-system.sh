#!/usr/bin/env bash
# Remove the non-root system LaunchDaemon (#76). Delegates to
# `runnyctl uninstall-daemon`, which boots the job out and removes the plist but
# LEAVES the _runny service account and /Library/Application Support/runny
# (config, the App key, artifacts) intact — purging those is a deliberate manual
# step. Prefers the staged runnyctl from install-system.sh, else PATH.
set -euo pipefail

RUNNYCTL="${RUNNYCTL:-/usr/local/libexec/runny/runnyctl}"
[ -x "$RUNNYCTL" ] || RUNNYCTL="$(command -v runnyctl || true)"

[ "$(id -u)" -eq 0 ] || { echo "run via sudo — removing a system LaunchDaemon is privileged" >&2; exit 1; }
[ -n "$RUNNYCTL" ] || { echo "runnyctl not found; set RUNNYCTL=/path/to/runnyctl" >&2; exit 1; }

exec "$RUNNYCTL" uninstall-daemon
