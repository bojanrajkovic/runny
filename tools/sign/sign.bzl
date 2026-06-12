"""codesign_binary, notarize_binary: macOS signing and notarization as Bazel outputs.

Usage:

    load("//tools/sign:sign.bzl", "codesign_binary", "notarize_binary")

    # Binary with special entitlements (e.g. runnyd needs com.apple.security.virtualization).
    codesign_binary(
        name = "runnyd_signed",
        binary = ":runnyd",
        entitlements = "//tools/sign:runnyd.entitlements",
        entitlement_key = "com.apple.security.virtualization",
    )

    # Plain CLI binary — no entitlements plist needed.
    codesign_binary(
        name = "runnyctl_signed",
        binary = ":runnyctl",
    )

    notarize_binary(
        name = "runnyd_notarized",
        binary = ":runnyd_signed",
    )

Both targets are Darwin-only. See .bazelrc for --config=developer-id and
--config=notarize; see CONTRIBUTING.md "Codesigning tiers" for setup.

Output files are named <name>.bin to avoid the Bazel rule/file label
collision that arises when a genrule output shares its target name.
"""

def codesign_binary(name, binary, entitlements = None, entitlement_key = None, **kwargs):
    """Produce a signed copy of a macOS binary.

    The signing identity is read from CODESIGN_IDENTITY (action-env).
    When unset it defaults to '-' (ad-hoc): boots VMs on any host, but
    cannot be distributed or notarized.

    Developer ID signing adds hardened runtime (--options runtime) and a
    trusted timestamp (--timestamp). Ad-hoc omits --timestamp; the
    timestamp server requires a real cert and rejects '-'.

    Args:
        name:            Target name. Output is <name>.bin.
        binary:          Label of the binary to sign.
        entitlements:    Optional label of an entitlements .plist to embed.
        entitlement_key: Optional key to assert is present in the signed
                         binary's entitlements (e.g.
                         "com.apple.security.virtualization"). Only
                         meaningful when entitlements is set.

    Set identity via --config=developer-id (see .bazelrc) or explicitly:
        bazel build --action_env="CODESIGN_IDENTITY=Developer ID Application: ..."
    """
    srcs = [binary]
    if entitlements:
        srcs.append(entitlements)
        entitlements_flag = "--entitlements $(location {e})".format(e = entitlements)
    else:
        entitlements_flag = ""

    if entitlement_key and entitlements:
        assert_cmd = """
            codesign -d --entitlements :- $@ 2>/dev/null | \\
                grep -q {key}
        """.format(key = entitlement_key)
    else:
        assert_cmd = ""

    native.genrule(
        name = name,
        srcs = srcs,
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
            codesign -s "$$IDENTITY" $$EXTRA_FLAGS {entitlements_flag} \\
                --force $@
            {assert_cmd}
        """.format(
            binary = binary,
            entitlements_flag = entitlements_flag,
            assert_cmd = assert_cmd,
        ),
        # no-sandbox: codesign needs keychain access (securityd) and network
        # access to Apple's timestamp/OCSP servers; Bazel's macOS sandbox
        # blocks both. Same mechanism rules_apple uses for its signing actions.
        tags = kwargs.pop("tags", []) + ["no-sandbox"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )

def notarize_binary(name, binary, **kwargs):
    """Notarize a Developer ID-signed binary via Apple's notarization service.

    The output is a byte-for-byte copy of the input binary — notarization
    registers an online Gatekeeper ticket keyed by the binary's CDHash rather
    than modifying the file. Building this target guarantees the ticket exists
    before the artifact leaves the build graph.

    Requires action-env variables (set by --config=notarize, or in CI from
    environment secrets decoded into job env vars):
      NOTARY_KEY_B64    — base64-encoded .p8 App Store Connect API key content
      NOTARY_KEY_ID     — key ID (e.g. ZLRYU227HK)
      NOTARY_ISSUER_ID  — issuer ID (UUID)

    When NOTARY_KEY_B64 is unset the action copies the binary unchanged — a
    safe no-op for ad-hoc-signed binaries that cannot be notarized.

    Set via --config=notarize (see .bazelrc). For local notarization, export
    the variables first:
        export NOTARY_KEY_B64=$(base64 < /path/to/key.p8 | tr -d '\\n')
        export NOTARY_KEY_ID=ZLRYU227HK
        export NOTARY_ISSUER_ID=69a6de7c-...
        bazel build --config=developer-id --config=notarize //cmd/runnyd:runnyd_notarized
    """
    native.genrule(
        name = name,
        srcs = [binary],
        outs = [name + ".bin"],
        cmd = """
            if [ -z "$$NOTARY_KEY_B64" ]; then
                cp $(location {binary}) $@
                exit 0
            fi
            NOTARY_TMPDIR=$$(mktemp -d /tmp/bazel-notary.XXXXXX)
            trap 'rm -rf "$$NOTARY_TMPDIR"' EXIT
            KEY="$$NOTARY_TMPDIR/key.p8"
            ZIP="$$NOTARY_TMPDIR/submit.zip"
            printf '%s' "$$NOTARY_KEY_B64" | base64 --decode > "$$KEY"
            zip -j "$$ZIP" $(location {binary})
            xcrun notarytool submit "$$ZIP" \\
                --key "$$KEY" \\
                --key-id "$$NOTARY_KEY_ID" \\
                --issuer "$$NOTARY_ISSUER_ID" \\
                --wait
            cp $(location {binary}) $@
        """.format(binary = binary),
        # no-sandbox: notarytool needs outbound HTTPS to Apple's notarization
        # service. no-remote: credentials in action-env must not reach a remote
        # executor.
        tags = kwargs.pop("tags", []) + ["no-sandbox", "no-remote"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )
