# Updates

The app uses [Sparkle](https://sparkle-project.org). It checks a signed feed (an *appcast*) and installs a newer version after the person agrees.

A build only has updates switched on when it was built with both values:

| Repository variable (Settings → Secrets and variables → Actions → Variables) | Meaning |
| --- | --- |
| `UPDATE_FEED_URL` | HTTPS address of the appcast, for example `https://example.com/pdu-monitor/appcast.xml` |
| `SPARKLE_PUBLIC_KEY` | Public Ed25519 key (base64) that the appcast and the archives are signed with |

Without them the app shows "Updates are not set up in this build" and never contacts anything.

## Setting it up

1. Generate the signing key once with Sparkle's `generate_keys` (in the Sparkle release archive, `bin/`); it prints the public key and keeps the private one in the login Keychain. Back the private key up (`generate_keys -x file`).
2. Choose where the appcast and the zip files are hosted (any HTTPS location the Macs can reach, **not** a private GitHub repository: Sparkle cannot log in to GitHub).
3. Set the two repository variables, tag a release (`git tag v0.1.1 && git push --tags`), take the zip, sign it with `sign_update` and add an `<item>` to the appcast.

A helper that publishes the signed appcast automatically (like the one in AdiumOne) is added once the hosting is decided.

## Signing and notarization

`scripts/import-certificate.sh` imports a Developer ID certificate when these repository Secrets exist, otherwise the app is signed ad hoc:
`MACOS_CERT_P12_BASE64`, `MACOS_CERT_PASSWORD`, `MACOS_SIGN_IDENTITY`. Notarization can be added the same way as in AdiumOne (`scripts/notarize.sh`).
