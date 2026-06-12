"""macOS signing, notarization, and dmg packaging as Bazel outputs.

Binaries (codesign_binary, notarize_binary):

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

App bundles (codesign_app, notarize_app, app_dmg, notarize_dmg) chain a
macos_application's .zip archive into a distributable dmg:

    :Runny → codesign_app → notarize_app → app_dmg → notarize_dmg

Every macro is env-driven with the same graceful tiers as the binary
macros: unset CODESIGN_IDENTITY means ad-hoc signing, unset NOTARY_KEY_B64
means notarization steps copy their input unchanged.

All targets are Darwin-only. See .bazelrc for --config=developer-id and
--config=notarize; see CONTRIBUTING.md "Codesigning tiers" for setup.

Output files are named <name>.bin / <name>.zip / <name>.dmg to avoid the
Bazel rule/file label collision that arises when a genrule output shares
its target name.
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
            if [ -z "$${{NOTARY_KEY_B64:-}}" ]; then
                cp $(location {binary}) $@
                exit 0
            fi
            NOTARY_TMPDIR=$$(mktemp -d /tmp/bazel-notary.XXXXXX)
            trap 'rm -rf "$$NOTARY_TMPDIR"' EXIT
            KEY="$$NOTARY_TMPDIR/key.p8"
            ZIP="$$NOTARY_TMPDIR/submit.zip"
            printf '%s' "$${{NOTARY_KEY_B64:-}}" | base64 --decode > "$$KEY"
            zip -j "$$ZIP" $(location {binary})
            xcrun notarytool submit "$$ZIP" \\
                --key "$$KEY" \\
                --key-id "$${{NOTARY_KEY_ID:-}}" \\
                --issuer "$${{NOTARY_ISSUER_ID:-}}" \\
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

def codesign_app(name, app_zip, **kwargs):
    """Re-sign a macos_application archive's .app bundle.

    rules_apple signs the bundle ad-hoc in-rule; this target re-signs it
    with the identity from CODESIGN_IDENTITY (action-env), defaulting to
    '-' (ad-hoc) when unset. Developer ID signing adds hardened runtime
    (--options runtime) and a trusted timestamp (--timestamp); ad-hoc adds
    neither.

    The bundle is signed with a single codesign of the .app — never --deep
    (deprecated, signs inside-out in unspecified order). The app's only
    nested Mach-O is its main executable, which signing the bundle covers.

    Args:
        name:    Target name. Output is <name>.zip containing the signed .app.
        app_zip: Label of a .zip containing the .app at its root (a
                 macos_application target's default output).
    """
    native.genrule(
        name = name,
        srcs = [app_zip],
        outs = [name + ".zip"],
        cmd = """
            IDENTITY="$${{CODESIGN_IDENTITY:--}}"
            if [ "$$IDENTITY" = "-" ]; then
                EXTRA_FLAGS=""
            else
                EXTRA_FLAGS="--options runtime --timestamp"
            fi
            WORK=$$(mktemp -d /tmp/bazel-sign-app.XXXXXX)
            trap 'rm -rf "$$WORK"' EXIT
            ditto -x -k $(location {app_zip}) "$$WORK"
            APP=$$(echo "$$WORK"/*.app)
            codesign -s "$$IDENTITY" $$EXTRA_FLAGS --force "$$APP"
            ditto -c -k --keepParent "$$APP" "$@"
        """.format(app_zip = app_zip),
        # no-sandbox: codesign needs keychain access (securityd) and network
        # access to Apple's timestamp/OCSP servers; Bazel's macOS sandbox
        # blocks both.
        tags = kwargs.pop("tags", []) + ["no-sandbox"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )

def notarize_app(name, app_zip, **kwargs):
    """Notarize and staple a signed .app archive.

    When NOTARY_KEY_B64 is set (see notarize_binary for the credential
    set), the zip is submitted via notarytool --wait, then the .app is
    stapled — unlike binaries, bundles can carry the Gatekeeper ticket
    offline — and re-zipped. When unset the input is copied unchanged: a
    safe no-op for ad-hoc-signed apps that cannot be notarized.

    Args:
        name:    Target name. Output is <name>.zip containing the stapled .app.
        app_zip: Label of a .zip containing the Developer ID-signed .app.
    """
    native.genrule(
        name = name,
        srcs = [app_zip],
        outs = [name + ".zip"],
        cmd = """
            if [ -z "$${{NOTARY_KEY_B64:-}}" ]; then
                cp $(location {app_zip}) $@
                exit 0
            fi
            NOTARY_TMPDIR=$$(mktemp -d /tmp/bazel-notary.XXXXXX)
            trap 'rm -rf "$$NOTARY_TMPDIR"' EXIT
            KEY="$$NOTARY_TMPDIR/key.p8"
            printf '%s' "$${{NOTARY_KEY_B64:-}}" | base64 --decode > "$$KEY"
            xcrun notarytool submit $(location {app_zip}) \\
                --key "$$KEY" \\
                --key-id "$${{NOTARY_KEY_ID:-}}" \\
                --issuer "$${{NOTARY_ISSUER_ID:-}}" \\
                --wait
            ditto -x -k $(location {app_zip}) "$$NOTARY_TMPDIR/bundle"
            APP=$$(echo "$$NOTARY_TMPDIR/bundle"/*.app)
            xcrun stapler staple "$$APP"
            ditto -c -k --keepParent "$$APP" "$@"
        """.format(app_zip = app_zip),
        # no-sandbox: notarytool needs outbound HTTPS to Apple's notarization
        # service. no-remote: credentials in action-env must not reach a remote
        # executor.
        tags = kwargs.pop("tags", []) + ["no-sandbox", "no-remote"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )

def app_dmg(name, app_zip, dmg_name, **kwargs):
    """Package a .app archive as a drag-to-/Applications dmg.

    Stages the .app next to an /Applications symlink and builds a
    compressed (UDZO) image. hdiutil on GitHub's macOS runners
    intermittently fails with "Resource busy"
    (actions/runner-images#7522), so creation retries up to 3 times with a
    5-second pause — bounded, never a hang.

    Args:
        name:     Target name. Output is <name>.dmg.
        app_zip:  Label of a .zip containing the .app at its root.
        dmg_name: Volume name of the mounted image (e.g. "Runny").
    """
    native.genrule(
        name = name,
        srcs = [app_zip],
        outs = [name + ".dmg"],
        cmd = """
            STAGE=$$(mktemp -d /tmp/bazel-dmg.XXXXXX)
            trap 'rm -rf "$$STAGE"' EXIT
            ditto -x -k $(location {app_zip}) "$$STAGE/root"
            ln -s /Applications "$$STAGE/root/Applications"
            for attempt in 1 2 3; do
                if hdiutil create -volname "{dmg_name}" \\
                    -srcfolder "$$STAGE/root" -ov -format UDZO \\
                    "$$STAGE/out.dmg"; then
                    break
                fi
                if [ "$$attempt" -eq 3 ]; then
                    echo "hdiutil create failed after 3 attempts" >&2
                    exit 1
                fi
                sleep 5
            done
            cp "$$STAGE/out.dmg" $@
        """.format(app_zip = app_zip, dmg_name = dmg_name),
        # no-sandbox: hdiutil talks to diskarbitrationd/diskimagesiod, which
        # Bazel's macOS sandbox blocks.
        tags = kwargs.pop("tags", []) + ["no-sandbox"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )

def notarize_dmg(name, dmg, **kwargs):
    """Notarize and staple a dmg.

    When NOTARY_KEY_B64 is set, the dmg is submitted via notarytool --wait
    and the output copy is stapled so Gatekeeper can verify it offline.
    When unset the input is copied unchanged.

    Args:
        name: Target name. Output is <name>.dmg.
        dmg:  Label of the dmg to notarize (its .app must already be
              notarized and stapled).
    """
    native.genrule(
        name = name,
        srcs = [dmg],
        outs = [name + ".dmg"],
        cmd = """
            cp $(location {dmg}) $@
            if [ -z "$${{NOTARY_KEY_B64:-}}" ]; then
                exit 0
            fi
            chmod +w $@
            NOTARY_TMPDIR=$$(mktemp -d /tmp/bazel-notary.XXXXXX)
            trap 'rm -rf "$$NOTARY_TMPDIR"' EXIT
            KEY="$$NOTARY_TMPDIR/key.p8"
            printf '%s' "$${{NOTARY_KEY_B64:-}}" | base64 --decode > "$$KEY"
            xcrun notarytool submit $(location {dmg}) \\
                --key "$$KEY" \\
                --key-id "$${{NOTARY_KEY_ID:-}}" \\
                --issuer "$${{NOTARY_ISSUER_ID:-}}" \\
                --wait
            xcrun stapler staple $@
        """.format(dmg = dmg),
        # no-sandbox: notarytool needs outbound HTTPS to Apple's notarization
        # service. no-remote: credentials in action-env must not reach a remote
        # executor.
        tags = kwargs.pop("tags", []) + ["no-sandbox", "no-remote"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )
