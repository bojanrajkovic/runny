#!/usr/bin/env bash
# Self-check for tools/version.sh's stable-baseline pinning: a manual beta
# tag between stable releases must not throw off later beta stamps. Builds
# throwaway git repos, no framework.
set -euo pipefail

VERSION_SH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/version.sh"

fail() {
	echo "FAIL: $1" >&2
	exit 1
}

new_repo() {
	local repo
	repo="$(mktemp -d)"
	git -C "$repo" init -q -b main
	git -C "$repo" config user.email "test@example.com"
	git -C "$repo" config user.name "test"
	echo "$repo"
}

commit() {
	git -C "$1" commit -q --allow-empty -m "$2"
}

assert_prefix() {
	case "$1" in
	"$2"*) ;;
	*) fail "expected prefix '$2', got '$1'" ;;
	esac
}

# v1.0.0 stable, then a manual beta tag, then more feat commits: version.sh
# must still stamp off v1.0.0 (next stable line: 1.1.0), not double-bump to
# 1.2.0 just because a beta tag for 1.1.0 already exists.
repo1="$(new_repo)"
commit "$repo1" "feat: first feature"
git -C "$repo1" tag v1.0.0
commit "$repo1" "feat: second feature"
git -C "$repo1" tag "v1.1.0-beta.$(git -C "$repo1" rev-parse --short=8 HEAD)"
commit "$repo1" "feat: third feature"
got1="$(cd "$repo1" && "$VERSION_SH")"
assert_prefix "$got1" "1.1.0-beta."
rm -rf "$repo1"

# Docs-only commit after a stable tag, no beta yet: must patch-nudge so the
# beta sorts above the release it follows, not equal/below it.
repo2="$(new_repo)"
commit "$repo2" "feat: only feature"
git -C "$repo2" tag v1.0.0
commit "$repo2" "docs: typo fix"
got2="$(cd "$repo2" && "$VERSION_SH")"
assert_prefix "$got2" "1.0.1-beta."
rm -rf "$repo2"

# Exactly on a stable tag: clean version, no beta suffix.
repo3="$(new_repo)"
commit "$repo3" "feat: only feature"
git -C "$repo3" tag v1.0.0
got3="$(cd "$repo3" && "$VERSION_SH")"
[ "$got3" = "1.0.0" ] || fail "expected clean '1.0.0', got '$got3'"
rm -rf "$repo3"

echo "ok"
