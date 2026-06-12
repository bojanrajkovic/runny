"""Version stamping for runny binaries (ADR-0010).

stamped_go_binary owns the x_defs incantation that binds main.version to
the workspace-status STABLE_VERSION, so every binary gets the stamp by
construction instead of each BUILD remembering it (runnyd shipped "dev"
the one time it was hand-copied).
"""

load("@rules_go//go:def.bzl", "go_binary")

def stamped_go_binary(name, **kwargs):
    """go_binary whose main.version is stamped under --config=release.

    Under --config=release (which sets --define=release=1), the linker
    strips the symbol table and DWARF (-s -w). Dev and CI builds retain
    symbols for profiling and crash symbolization.
    """
    x_defs = dict(kwargs.pop("x_defs", {}))
    x_defs["main.version"] = "{STABLE_VERSION}"
    gc_linkopts = list(kwargs.pop("gc_linkopts", []))
    gc_linkopts += select({
        "//:release_build": ["-s", "-w"],
        "//conditions:default": [],
    })
    go_binary(
        name = name,
        gc_linkopts = gc_linkopts,
        x_defs = x_defs,
        **kwargs
    )
