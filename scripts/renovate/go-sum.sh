#!/usr/bin/env bash
# Renovate postUpgradeTask for gomod updates (renovate.json5), run from the
# repo root. Runs the dependency workflow from CONTRIBUTING.md so go.sum,
# BUILD files and MODULE.bazel's use_repo match the bumped go.mod. The -e and
# the order both matter: see "Known wrinkle" in CONTRIBUTING.md.
set -euo pipefail

mise x -- go mod tidy -e
mise x -- bazel run //:gazelle
mise x -- bazel mod tidy
