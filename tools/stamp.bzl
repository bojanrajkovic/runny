"""Version stamping for runny binaries (ADR-0010).

stamped_go_binary owns the x_defs incantation that binds main.version to
the workspace-status STABLE_VERSION, so every binary gets the stamp by
construction instead of each BUILD remembering it (runnyd shipped "dev"
the one time it was hand-copied).
"""

load("@rules_go//go:def.bzl", "go_binary")

def stamped_go_binary(name, **kwargs):
    """go_binary whose main.version is stamped under --config=release."""
    x_defs = dict(kwargs.pop("x_defs", {}))
    x_defs["main.version"] = "{STABLE_VERSION}"
    go_binary(
        name = name,
        x_defs = x_defs,
        **kwargs
    )
