# Sourced by build-app.sh and make-universal.sh. SIGN is the list of arguments for codesign: a Developer ID identity with the
# hardened runtime and a secure timestamp when MACOS_SIGN_IDENTITY is set (import-certificate.sh), else an ad hoc signature.
if [[ -n "${MACOS_SIGN_IDENTITY:-}" ]]; then
  SIGN=(--force --sign "$MACOS_SIGN_IDENTITY" --options runtime --timestamp)
else
  SIGN=(--force --sign -)
fi
