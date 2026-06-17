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

# The notarization credential recipe exists ONCE. Three macros submit three
# artifact shapes (zip of a binary, app archive, dmg), and a credential or
# flag change that lands in only some of them degrades silently to a
# pass-through at the unset-env tier — the drift would first surface as a
# Gatekeeper rejection on a published release.
_NOTARY_KEY_SETUP = """
            NOTARY_TMPDIR=$$(mktemp -d /tmp/bazel-notary.XXXXXX)
            trap 'rm -rf "$$NOTARY_TMPDIR"' EXIT
            KEY="$$NOTARY_TMPDIR/key.p8"
            printf '%s' "$${{NOTARY_KEY_B64:-}}" | base64 --decode > "$$KEY"
"""

# --timeout bounds --wait: an Apple-side stall fails the build loudly after
# 30 minutes instead of hanging it forever (no unbounded operations).
_NOTARY_SUBMIT = """
            xcrun notarytool submit {artifact} \\
                --key "$$KEY" \\
                --key-id "$${{NOTARY_KEY_ID:-}}" \\
                --issuer "$${{NOTARY_ISSUER_ID:-}}" \\
                --wait --timeout 30m
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
        cmd = ("""
            if [ -z "$${{NOTARY_KEY_B64:-}}" ]; then
                cp $(location {binary}) $@
                exit 0
            fi
""" + _NOTARY_KEY_SETUP + """
            ZIP="$$NOTARY_TMPDIR/submit.zip"
            zip -j "$$ZIP" $(location {binary})
""" + _NOTARY_SUBMIT + """
            cp $(location {binary}) $@
        """).format(binary = binary, artifact = '"$$ZIP"'),
        # no-sandbox: notarytool needs outbound HTTPS to Apple's notarization
        # service. no-remote: credentials in action-env must not reach a remote
        # executor.
        tags = kwargs.pop("tags", []) + ["no-sandbox", "no-remote"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )

def inject_launch_agent(name, plist, app_zip, **kwargs):
    """Inject a LaunchAgent plist into a macos_application bundle BEFORE signing.

    rules_apple routes resources to Contents/Resources/ and has no first-class
    LaunchAgents slot, so the plist is placed by hand at the SMAppService-resolved
    Contents/Library/LaunchAgents/<basename>. This runs BETWEEN macos_application
    and codesign_app: the plist must be inside the bundle when codesign_app seals
    it (so the launchd job's config is covered by the signature) and inside the
    bundle notarize_app submits (so the staple stays valid). A post-build
    injection would invalidate both — the data flow `:app → inject → sign →
    notarize` is load-bearing, not cosmetic.

    Args:
        name:    Target name. Output is <name>.zip containing the .app.
        plist:   Label of the LaunchAgent .plist to inject.
        app_zip: Label of a .zip containing the .app at its root (a
                 macos_application target's default output).
    """
    native.genrule(
        name = name,
        srcs = [app_zip, plist],
        outs = [name + ".zip"],
        cmd = """
            WORK=$$(mktemp -d /tmp/bazel-inject-agent.XXXXXX)
            trap 'rm -rf "$$WORK"' EXIT
            ditto -x -k $(location {app_zip}) "$$WORK"
            APP=$$(echo "$$WORK"/*.app)
            # Fail loud if extraction didn't yield exactly one .app: an unmatched
            # glob leaves the literal "*.app" (not a dir), and a multi-match makes
            # APP two space-separated paths — either way [ -d ] is false, so we
            # never mkdir/cp/zip a bogus tree that codesign_app would choke on later.
            [ -d "$$APP" ] || {{ echo "inject_launch_agent: expected exactly one .app in the archive, got: $$APP" >&2; exit 1; }}
            mkdir -p "$$APP/Contents/Library/LaunchAgents"
            cp $(location {plist}) "$$APP/Contents/Library/LaunchAgents/"
            ditto -c -k --keepParent "$$APP" "$@"
        """.format(app_zip = app_zip, plist = plist),
        # no-sandbox: ditto preserves the bundle's Apple metadata (xattrs,
        # resource forks) most reliably outside Bazel's macOS sandbox, matching
        # the codesign/dmg genrules it sits between.
        tags = kwargs.pop("tags", []) + ["no-sandbox"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )

def codesign_app(name, app_zip, binaries = {}, entitlements = {}, entitlement_keys = {}, **kwargs):
    """Re-sign a macos_application archive's .app bundle.

    rules_apple signs the bundle ad-hoc in-rule; this target re-signs it
    with the identity from CODESIGN_IDENTITY (action-env), defaulting to
    '-' (ad-hoc) when unset. Both tiers apply hardened runtime (--options
    runtime), matching codesign_binary, so dev builds exercise the same
    runtime restrictions the release ships with; Developer ID adds the
    trusted timestamp (the timestamp server rejects '-').

    Nested binaries (the daemon + CLI carried in Contents/MacOS) are placed
    and signed FIRST, then the single outer codesign of the .app seals over
    them — inside-out, by hand. This is NOT --deep SIGNING (deprecated; signs
    inside-out in an unspecified order with the wrong flags on nested Mach-Os):
    each nested binary is signed explicitly, with its own entitlements, before
    the bundle seal. Verification (below) DOES use --deep, which is a different
    operation — it only WALKS the seals to confirm each is valid, in no
    particular order, and is not deprecated. Do not "consistency-fix" the two
    to match.

    With no `binaries`, the macro behaves exactly as it did before nesting was
    added: one outer codesign, no placement, no extra assertions — so any
    bundle whose only Mach-O is its own executable is unaffected.

    app_zip must resolve to exactly one file: with --apple_generate_dsym
    or include_symbols_in_bundle, macos_application's default output grows
    dSYM files and $(location) fails loudly — strip those flags or point
    at the archive output explicitly.

    Args:
        name:    Target name. Output is <name>.zip containing the signed .app.
        app_zip: Label of a .zip containing the .app at its root (a
                 macos_application target's default output).
        binaries: dict of bare bundle name -> signed-binary label, placed at
                 Contents/MacOS/<bare-name> and signed inside-out. The bare
                 name is exact (no .bin suffix): a launchd BundleProgram and a
                 vended-CLI symlink both depend on it.
        entitlements: dict of bare name -> entitlements .plist label embedded
                 when signing that binary. A name absent here is signed plain.
        entitlement_keys: dict of bare name -> the one entitlement key that
                 binary MUST carry (e.g. com.apple.security.virtualization for
                 the daemon). Every such key is treated as sensitive across the
                 whole bundle: the build asserts the owning binary HAS it and
                 every OTHER nested binary does NOT — so a CLI can never inherit
                 the daemon's VM grant, and a daemon that lost it fails the build
                 red (the silent VM-denial this guards).
    """
    srcs = [app_zip] + list(binaries.values()) + list(entitlements.values())

    # A plist or required-key named for a binary not in `binaries` is a typo that
    # would silently drop that binary's must-have assertion — fail loud at analysis.
    # (Loop var must NOT be `name`: that would rebind the genrule's name parameter.)
    for keyed in list(entitlements.keys()) + list(entitlement_keys.keys()):
        if keyed not in binaries:
            fail("codesign_app: entitlements/entitlement_keys names %r, which is not in binaries %s" %
                 (keyed, sorted(binaries.keys())))

    # The sensitive keys across the bundle: each owning binary must carry its
    # own, and no other nested binary may. Deduped, order-stable.
    sensitive = []
    for key in entitlement_keys.values():
        if key not in sensitive:
            sensitive.append(key)

    nested = ""
    asserts = ""
    verify = ""
    if binaries:
        nested_lines = []
        verify_lines = []
        assert_lines = []
        for bare, label in binaries.items():
            dest = '"$$APP/Contents/MacOS/%s"' % bare
            ent_flag = ""
            if bare in entitlements:
                ent_flag = "--entitlements $(location %s) " % entitlements[bare]
            nested_lines.append(
                "cp $(location %s) %s\n" % (label, dest) +
                "chmod +wx %s\n" % dest +
                'codesign -s "$$IDENTITY" $$EXTRA_FLAGS %s--force %s\n' % (ent_flag, dest),
            )

            # Each nested Mach-O must validate on its own, so even an ad-hoc dev
            # build proves the inside-out seal — not just the outer bundle.
            verify_lines.append("codesign --verify --strict %s\n" % dest)
            own = entitlement_keys.get(bare)
            for key in sensitive:
                # grep -qF -- : exact fixed string, so the dots in the key aren't
                # regex wildcards and a key that is a prefix of another can't match.
                if key == own:
                    assert_lines.append(
                        'codesign -d --entitlements :- %s 2>/dev/null | grep -qF -- %s || { echo "%s is missing required entitlement %s" >&2; exit 1; }\n' % (dest, key, bare, key),
                    )
                else:
                    assert_lines.append(
                        'if codesign -d --entitlements :- %s 2>/dev/null | grep -qF -- %s; then echo "%s carries entitlement %s it must not" >&2; exit 1; fi\n' % (dest, key, bare, key),
                    )
        nested = "".join(nested_lines)
        asserts = "".join(assert_lines)

        # --deep here is verification, not signing (see the docstring): it walks
        # every nested seal to confirm validity. Paired with the per-binary
        # --verify --strict above so a missing nested signature fails the build.
        verify = "".join(verify_lines) + 'codesign --verify --deep --strict "$$APP"\n'

    native.genrule(
        name = name,
        srcs = srcs,
        outs = [name + ".zip"],
        cmd = """
            IDENTITY="$${{CODESIGN_IDENTITY:--}}"
            if [ "$$IDENTITY" = "-" ]; then
                EXTRA_FLAGS="--options runtime"
            else
                EXTRA_FLAGS="--options runtime --timestamp"
            fi
            WORK=$$(mktemp -d /tmp/bazel-sign-app.XXXXXX)
            trap 'rm -rf "$$WORK"' EXIT
            ditto -x -k $(location {app_zip}) "$$WORK"
            APP=$$(echo "$$WORK"/*.app)
            {nested}codesign -s "$$IDENTITY" $$EXTRA_FLAGS --force "$$APP"
            {asserts}{verify}ditto -c -k --keepParent "$$APP" "$@"
        """.format(app_zip = app_zip, nested = nested, asserts = asserts, verify = verify),
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
        cmd = ("""
            if [ -z "$${{NOTARY_KEY_B64:-}}" ]; then
                cp $(location {app_zip}) $@
                exit 0
            fi
""" + _NOTARY_KEY_SETUP + _NOTARY_SUBMIT + """
            ditto -x -k $(location {app_zip}) "$$NOTARY_TMPDIR/bundle"
            APP=$$(echo "$$NOTARY_TMPDIR/bundle"/*.app)
            xcrun stapler staple "$$APP"
            ditto -c -k --keepParent "$$APP" "$@"
        """).format(app_zip = app_zip, artifact = "$(location {})".format(app_zip)),
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
        cmd = ("""
            cp $(location {dmg}) $@
            if [ -z "$${{NOTARY_KEY_B64:-}}" ]; then
                exit 0
            fi
            chmod +w $@
""" + _NOTARY_KEY_SETUP + _NOTARY_SUBMIT + """
            xcrun stapler staple $@
        """).format(dmg = dmg, artifact = "$(location {})".format(dmg)),
        # no-sandbox: notarytool needs outbound HTTPS to Apple's notarization
        # service. no-remote: credentials in action-env must not reach a remote
        # executor.
        tags = kwargs.pop("tags", []) + ["no-sandbox", "no-remote"],
        target_compatible_with = ["@platforms//os:macos"],
        **kwargs
    )
