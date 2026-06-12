"""Targets in the repository root"""

load("@gazelle//:def.bzl", "gazelle")

# gazelle:prefix github.com/bojanrajkovic/runny
# gazelle:build_file_name BUILD

gazelle(name = "gazelle")

# True when --config=release is active (adds --define=release=1).
# Used by stamped_go_binary to gate linker symbol-stripping.
config_setting(
    name = "release_build",
    values = {"define": "release=1"},
    visibility = ["//visibility:public"],
)
