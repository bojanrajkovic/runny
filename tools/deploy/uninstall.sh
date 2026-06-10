#!/usr/bin/env bash
# Remove the runnyd LaunchAgent. The on-disk ~/.runny tree is left intact.
set -euo pipefail

LABEL="com.coderinserepeat.runnyd"
DEST="$HOME/Library/LaunchAgents/$LABEL.plist"

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
rm -f "$DEST"
echo "Removed $LABEL (left $HOME/.runny in place)"
