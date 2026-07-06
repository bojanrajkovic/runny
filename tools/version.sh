#!/usr/bin/env bash
# Derives runny's version (ADR-0010): the baseline is the last STABLE tag,
# found via `git describe --exclude` (real exclusion, unlike svu's glob-only
# --tag.pattern) rather than svu's own "current" — svu's current is just the
# latest tag of any kind, so it drifts once a manual beta tag exists between
# stable releases. Pinning svu's --tag.pattern to that one literal stable tag
# keeps its bump computation in sync with release-please's manifest-anchored
# view. This wrapper then only decides clean release (exactly on the tag) vs
# -beta.<shortsha> pre-release.
set -euo pipefail

stable=$(git describe --tags --exclude='*-beta*' --abbrev=0 2>/dev/null || echo v0.0.0)
next=$(svu next --tag.pattern "$stable")
current=$(svu current --tag.pattern "$stable" 2>/dev/null || echo v0.0.0)

if [ "$next" = "$current" ] &&
	[ "$(git rev-list -n 1 "$current" 2>/dev/null || echo none)" = "$(git rev-parse HEAD)" ]; then
	echo "${next#v}"
	exit 0
fi

# Past the tag with no implied bump (only chore:/docs:/ci: commits): svu
# returns the released version, and <released>-beta.<sha> would semver-sort
# BELOW the release it follows. Bump patch so the beta line sorts between
# this release and the next.
if [ "$next" = "$current" ]; then
	IFS=. read -r maj min pat <<<"${next#v}"
	next="v${maj}.${min}.$((pat + 1))"
fi
echo "${next#v}-beta.$(git rev-parse --short=8 HEAD)"
