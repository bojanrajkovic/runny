#!/usr/bin/env bash
# Derives runny's version (ADR-0010): svu computes the conventional-commit
# implied next version (--v0: breaking bumps minor while pre-1.0, matching
# release-please's bump-minor-pre-major); this wrapper only decides clean
# release (exactly on the tag) vs -beta.<shortsha> pre-release.
set -euo pipefail

next=$(svu next --v0)
current=$(svu current 2>/dev/null || echo v0.0.0)

if [ "$next" = "$current" ] &&
	[ "$(git rev-list -n 1 "$current" 2>/dev/null || echo none)" = "$(git rev-parse HEAD)" ]; then
	echo "${next#v}"
else
	echo "${next#v}-beta.$(git rev-parse --short=8 HEAD)"
fi
