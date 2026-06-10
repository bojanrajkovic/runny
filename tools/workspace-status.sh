#!/usr/bin/env bash
# Bazel workspace status for stamped builds (--config=release).
# STABLE_ keys invalidate stamped targets when they change.
set -euo pipefail
echo "STABLE_VERSION $(tools/version.sh)"
echo "STABLE_GIT_SHA $(git rev-parse HEAD)"
