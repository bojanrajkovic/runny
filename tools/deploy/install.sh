#!/usr/bin/env bash
# Install runnyd as a per-user LaunchAgent.
#
# Run this from a GUI login session (sit at the machine, or Screen Sharing —
# NOT a bare SSH shell), so the one-time macOS "Local Network" prompt can
# appear and the TCC grant can stick. See docs/deploy.md.
set -euo pipefail

LABEL="com.coderinserepeat.runnyd"
RUNNYD="${RUNNYD:-$(command -v runnyd || true)}"
RUNNY_HOME="${RUNNY_HOME:-$HOME/.runny}"
HERE="$(cd "$(dirname "$0")" && pwd)"
TMPL="$HERE/com.coderinserepeat.runnyd.plist.tmpl"
DEST="$HOME/Library/LaunchAgents/$LABEL.plist"
UID_NUM="$(id -u)"

[ -n "$RUNNYD" ] || { echo "runnyd not found on PATH; set RUNNYD=/path/to/runnyd or install it via the tap" >&2; exit 1; }
[ -f "$TMPL" ] || { echo "template missing: $TMPL" >&2; exit 1; }

mkdir -p "$HOME/Library/LaunchAgents" "$RUNNY_HOME/logs"
sed -e "s|@RUNNYD@|$RUNNYD|g" -e "s|@RUNNY_HOME@|$RUNNY_HOME|g" "$TMPL" > "$DEST"

# Reload cleanly if a previous generation is loaded.
launchctl bootout "gui/$UID_NUM/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$UID_NUM" "$DEST"
launchctl enable "gui/$UID_NUM/$LABEL"

echo "Installed $LABEL"
echo "  plist:      $DEST"
echo "  runnyd:     $RUNNYD"
echo "  RUNNY_HOME: $RUNNY_HOME"
echo
echo "First install on this machine: accept the macOS \"Local Network\" prompt"
echo "for runnyd when it appears. Then verify reachability with:"
echo "    runnyd -doctor          # the 'local-network' check confirms the grant"
