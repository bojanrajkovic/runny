#!/usr/bin/env bash
# Asserts the signed Runny.app actually carries runnyd + runnyctl correctly:
# present under the bare names a launchd BundleProgram and the vended-CLI symlink
# assume, each independently codesign-valid (the inside-out seal, not just the
# outer bundle), the daemon entitled for virtualization and the CLI not, and each
# genuinely executable. A corrupt or wrong-arch Mach-O passes presence and
# codesign but fails the exec, so the exec is not redundant. Darwin-only (codesign
# / ditto); the signing it checks lives in tools/sign/sign.bzl.
set -euo pipefail

ZIP="${1:?usage: bundle_contents_test.sh <Runny_signed.zip>}"
# $(rootpath) is runfiles-root-relative; sh_test usually runs from there, but
# fall back through the documented runfiles roots so a layout change fails loud
# with a path, not a confusing ditto error.
if [ ! -f "$ZIP" ]; then
  for cand in \
    "${TEST_SRCDIR:-}/$ZIP" "${TEST_SRCDIR:-}/_main/$ZIP" \
    "${RUNFILES_DIR:-}/$ZIP" "${RUNFILES_DIR:-}/_main/$ZIP"; do
    if [ -f "$cand" ]; then ZIP="$cand"; break; fi
  done
fi
[ -f "$ZIP" ] || { echo "FAIL: cannot locate the signed app zip (arg='$1', cwd=$(pwd))" >&2; exit 1; }

WORK="$(mktemp -d "${TEST_TMPDIR:-/tmp}/bundle.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
ditto -x -k "$ZIP" "$WORK"
APP="$(echo "$WORK"/*.app)"
MACOS="$APP/Contents/MacOS"

fail() { echo "FAIL: $*" >&2; exit 1; }

# 1. Both binaries present under the EXACT bare names — not runnyd_signed.bin,
#    not a path nested by the genrule's package.
[ -f "$MACOS/runnyd" ]   || fail "Contents/MacOS/runnyd is missing"
[ -f "$MACOS/runnyctl" ] || fail "Contents/MacOS/runnyctl is missing"

# 2. Each nested Mach-O validates on its own (the inside-out seal).
codesign --verify --strict "$MACOS/runnyd"   || fail "runnyd nested signature invalid"
codesign --verify --strict "$MACOS/runnyctl" || fail "runnyctl nested signature invalid"

# 3. The daemon carries com.apple.security.virtualization — without it, it boots
#    but is silently denied VM creation (the headline silent failure). The CLI
#    carries none — signing it with the VM grant would be a real over-grant.
codesign -d --entitlements :- "$MACOS/runnyd" 2>/dev/null | grep -q com.apple.security.virtualization \
  || fail "runnyd is missing com.apple.security.virtualization (silent VM-denial)"
if codesign -d --entitlements :- "$MACOS/runnyctl" 2>/dev/null | grep -q com.apple.security.virtualization; then
  fail "runnyctl is over-granted com.apple.security.virtualization"
fi

# 4. Each binary actually execs. Both version probes are side-effect-free (no
#    home, no config, no daemon), so this proves the Mach-O runs on this arch.
"$MACOS/runnyctl" version  >/dev/null 2>&1 || fail "runnyctl does not exec"
"$MACOS/runnyd"   -version >/dev/null 2>&1 || fail "runnyd does not exec"

echo "ok: runnyd + runnyctl present, signed inside-out, correctly entitled, executable"
