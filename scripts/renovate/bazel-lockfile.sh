#!/usr/bin/env bash
# Renovate postUpgradeTask for MODULE.bazel and bazel version updates
# (renovate.json5), run from the repo root. Regenerates MODULE.bazel.lock,
# which CI checks with --lockfile_mode=error. A new bazel version resolves
# different implicit deps, so it needs a fresh lockfile too.
set -euo pipefail

mise x -- bazel mod deps --lockfile_mode=update
