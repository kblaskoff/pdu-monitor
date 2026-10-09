#!/bin/bash
# Puts the Developer ID certificate of the repository Secrets into a temporary keychain of this runner and tells the next
# steps of the job to sign with it (MACOS_SIGN_IDENTITY in $GITHUB_ENV). Without the Secrets (a fork, a pull request from a
# fork) it does nothing and the scripts keep signing ad hoc, so that every build still works.
# Input from the environment: CERT_P12_BASE64, CERT_PASSWORD, SIGN_IDENTITY. Nothing is printed except the name of the identity.
set -euo pipefail
if [[ -z "${CERT_P12_BASE64:-}" || -z "${CERT_PASSWORD:-}" || -z "${SIGN_IDENTITY:-}" ]]; then
  echo "No signing certificate in the Secrets: the application is signed ad hoc."
  exit 0
fi
# Values typed or pasted by a person: without the quotes of the `find-identity` output and without spaces around them.
SIGN_IDENTITY="$(printf '%s' "$SIGN_IDENTITY" | sed -e 's/^[[:space:]"]*//' -e 's/[[:space:]"]*$//')"
KEYCHAIN="$RUNNER_TEMP/pdumonitor-signing.keychain-db"
KEYCHAIN_PASSWORD="$(uuidgen)"
security create-keychain -p "$KEYCHAIN_PASSWORD" "$KEYCHAIN"
security set-keychain-settings -lut 21600 "$KEYCHAIN"
security unlock-keychain -p "$KEYCHAIN_PASSWORD" "$KEYCHAIN"
printf '%s' "$CERT_P12_BASE64" | base64 --decode > "$RUNNER_TEMP/pdumonitor-cert.p12"
security import "$RUNNER_TEMP/pdumonitor-cert.p12" -P "$CERT_PASSWORD" -f pkcs12 -T /usr/bin/codesign -k "$KEYCHAIN" > /dev/null
rm -f "$RUNNER_TEMP/pdumonitor-cert.p12"
# The intermediate certificate of Developer ID, so that the chain to Apple can be built on a clean runner.
if curl -sSfL https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer -o "$RUNNER_TEMP/developer-id-g2.cer"; then
  security import "$RUNNER_TEMP/developer-id-g2.cer" -k "$KEYCHAIN" > /dev/null || true
fi
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$KEYCHAIN_PASSWORD" "$KEYCHAIN" > /dev/null
# shellcheck disable=SC2046
security list-keychains -d user -s "$KEYCHAIN" $(security list-keychains -d user | tr -d '"')
if ! security find-identity -v -p codesigning "$KEYCHAIN" | grep -F "$SIGN_IDENTITY" > /dev/null; then
  echo "The identity '$SIGN_IDENTITY' is not in the certificate of the Secrets. Found:" >&2
  security find-identity -v -p codesigning "$KEYCHAIN" >&2
  exit 1
fi
echo "MACOS_SIGN_IDENTITY=$SIGN_IDENTITY" >> "$GITHUB_ENV"
echo "Signing with: $SIGN_IDENTITY"
