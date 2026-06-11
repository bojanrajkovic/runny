#!/usr/bin/env bash
# Sets the GitHub secrets the release pipeline can use (ADR-0010,
# CONTRIBUTING.md "Codesigning tiers"). Everything is optional — workflows
# skip gracefully when a secret is absent. Secrets land on the `release`
# environment, not the repo: only jobs that declare `environment: release`
# can read them (zizmor: secrets-outside-env). Run interactively or with
# flags:
#
#   tools/setup-secrets.sh \
#     --releaser-key ~/keys/releaser-bot.private-key.pem \
#     --cert-p12 ~/certs/developer-id.p12 --p12-password '...' \
#     --notary-key ~/keys/AuthKey_ABC123.p8 --notary-key-id ABC123 \
#     --notary-issuer-id 12345678-...-...
set -euo pipefail

REPO="bojanrajkovic/runny"
ENVIRONMENT="release"
releaser_key="" cert_p12="" p12_password="" notary_key="" notary_key_id="" notary_issuer_id=""

while [ $# -gt 0 ]; do
	case "$1" in
	--releaser-key) releaser_key="$2" && shift 2 ;;
	--cert-p12) cert_p12="$2" && shift 2 ;;
	--p12-password) p12_password="$2" && shift 2 ;;
	--notary-key) notary_key="$2" && shift 2 ;;
	--notary-key-id) notary_key_id="$2" && shift 2 ;;
	--notary-issuer-id) notary_issuer_id="$2" && shift 2 ;;
	*) echo "unknown flag $1" >&2 && exit 2 ;;
	esac
done

# The environment must exist before secrets can land on it (idempotent).
gh api -X PUT "repos/$REPO/environments/$ENVIRONMENT" --silent

set_secret() { # set_secret <name> <value>
	[ -z "$2" ] && echo "  - $1: skipped" && return 0
	printf '%s' "$2" | gh secret set "$1" --repo "$REPO" --env "$ENVIRONMENT"
	echo "  ✓ $1"
}

echo "Setting secrets on $REPO (environment: $ENVIRONMENT)"

# The releaser App's private key (release-please + tap push mint scoped
# installation tokens from it).
if [ -z "$releaser_key" ]; then
	read -r -p "Path to releaser App private key .pem (empty to skip): " releaser_key
fi
if [ -n "$releaser_key" ]; then
	set_secret RELEASER_APP_PRIVATE_KEY "$(cat "$releaser_key")"
else
	echo "  - RELEASER_APP_PRIVATE_KEY: skipped"
fi

# Developer ID signing (upgrade from ad-hoc; see CONTRIBUTING.md).
if [ -z "$cert_p12" ]; then
	read -r -p "Path to Developer ID Application .p12 (empty to skip): " cert_p12
fi
if [ -n "$cert_p12" ]; then
	set_secret BUILD_CERTIFICATE_BASE64 "$(base64 < "$cert_p12" | tr -d '\n')"
	if [ -z "$p12_password" ]; then
		read -rs -p "P12 password: " p12_password && echo
	fi
	set_secret P12_PASSWORD "$p12_password"
	# Throwaway keychain password for the CI-created temp keychain.
	set_secret KEYCHAIN_PASSWORD "$(openssl rand -hex 24)"
else
	echo "  - BUILD_CERTIFICATE_BASE64 / P12_PASSWORD / KEYCHAIN_PASSWORD: skipped"
fi

# Notarization (App Store Connect API key).
if [ -z "$notary_key" ]; then
	read -r -p "Path to App Store Connect API key .p8 (empty to skip): " notary_key
fi
if [ -n "$notary_key" ]; then
	set_secret NOTARY_KEY_P8 "$(cat "$notary_key")"
	if [ -z "$notary_key_id" ]; then
		read -r -p "Notary key ID: " notary_key_id
	fi
	set_secret NOTARY_KEY_ID "$notary_key_id"
	if [ -z "$notary_issuer_id" ]; then
		read -r -p "Notary issuer ID: " notary_issuer_id
	fi
	set_secret NOTARY_ISSUER_ID "$notary_issuer_id"
else
	echo "  - NOTARY_KEY_P8 / NOTARY_KEY_ID / NOTARY_ISSUER_ID: skipped"
fi

echo "Done. Current environment secrets:"
gh secret list --repo "$REPO" --env "$ENVIRONMENT"
