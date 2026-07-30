#!/usr/bin/env bash
# Install runnyd as a NON-ROOT system LaunchDaemon — the headless path, for
# from-checkout / CI / config-management hosts.
#
# Homebrew users can instead run `sudo runnyctl install-daemon` directly: its
# plist points at the stable /opt/homebrew/bin/runnyd symlink, so it tracks
# `brew upgrade` automatically. This script is for the from-checkout case, where
# the built runnyd and runnyctl are NOT co-located (so install-daemon's
# sibling resolution can't find runnyd): it stages both into a stable libexec
# dir and delegates the privileged work to `runnyctl install-daemon`, so the
# dscl service account, the dual inheriting ACL, the plist, and the
# `launchctl bootstrap system` all come from ONE implementation (no bash copy of
# the security-critical ACL to drift). Re-run after rebuilding to restage.
set -euo pipefail

STAGE=/usr/local/libexec/runny
RUNNYD="${RUNNYD:-$(command -v runnyd || true)}"
RUNNYCTL="${RUNNYCTL:-$(command -v runnyctl || true)}"

[ "$(id -u)" -eq 0 ] || { echo "run via sudo — a system LaunchDaemon install is privileged" >&2; exit 1; }
{ [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; } || {
  echo "run via sudo from your normal login (SUDO_USER must be set — the home's ACL grants that account access)" >&2
  exit 1
}
[ -n "$RUNNYD" ]   || { echo "runnyd not found; set RUNNYD=/path/to/runnyd (e.g. a bazel-bin build)" >&2; exit 1; }
[ -n "$RUNNYCTL" ] || { echo "runnyctl not found; set RUNNYCTL=/path/to/runnyctl" >&2; exit 1; }

mkdir -p "$STAGE"
install -m 0755 "$RUNNYD" "$STAGE/runnyd"
install -m 0755 "$RUNNYCTL" "$STAGE/runnyctl"
echo "staged runnyd + runnyctl into $STAGE"

# install-daemon resolves runnyd as the sibling of this runnyctl (now both in
# $STAGE) and performs the account + ACL + plist + bootstrap. SUDO_USER carries
# through exec, so the inheriting ACL is granted to the right operator.
exec "$STAGE/runnyctl" install-daemon
