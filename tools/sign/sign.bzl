"""codesign_binary: sign a macOS binary as a Bazel output.

Usage:

    load("//tools/sign:sign.bzl", "codesign_binary")

    codesign_binary(
        name = "runnyd_signed",
        binary = ":runnyd",
        entitlements = "//tools/sign:runnyd.entitlements",
    )

The signing identity is read from the CODESIGN_IDENTITY action-env variable.
When unset it defaults to '-' (ad-hoc), which boots VMs on any host but
cannot be distributed or notarized. To sign with Developer ID:

    bazel build --config=developer-id //cmd/runnyd:runnyd_signed

or at the command line:

    bazel build --action_env="CODESIGN_IDENTITY=Developer ID Application: ..."

The Developer ID config is defined in .bazelrc; the identity string is
printed by `security find-identity -v -p codesigning`.
"""

def codesign_binary(name, binary, entitlements, **kwargs):
    """Produce a signed copy of a macOS binary.

    Signing identity defaults to '-' (ad-hoc). Override with
    --action_env=CODESIGN_IDENTITY=... or --config=developer-id.

    Developer ID signing automatically enables hardened runtime
    (--options runtime) and a trusted timestamp (--timestamp), both
    required for notarization. Ad-hoc signing enables hardened runtime
    only (--timestamp requires a real cert and fails with '-').
    """
    native.genrule(
        name = name,
        srcs = [binary, entitlements],
        outs = [name + ".bin"],
        cmd = """
            IDENTITY="$${{CODESIGN_IDENTITY:--}}"
            if [ "$$IDENTITY" = "-" ]; then
                EXTRA_FLAGS="--options runtime"
            else
                EXTRA_FLAGS="--options runtime --timestamp"
            fi
            cp $(location {binary}) $@
            chmod +wx $@
            codesign -s "$$IDENTITY" $$EXTRA_FLAGS \\
                --entitlements $(location {entitlements}) \\
                --force $@
            codesign -d --entitlements :- $@ 2>/dev/null | \\
                grep -q com.apple.security.virtualization
        """.format(binary = binary, entitlements = entitlements),
        # no-sandbox: codesign needs keychain access (securityd) and network
        # access to Apple's timestamp/OCSP servers; Bazel's macOS sandbox
        # blocks both. Same mechanism rules_apple uses for its signing actions.
        tags = kwargs.pop("tags", []) + ["no-sandbox"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )
