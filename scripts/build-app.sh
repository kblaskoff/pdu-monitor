#!/bin/bash
# Builds build/PDUMonitor.app (a Universal binary: Apple Silicon and Intel) and build/PDUMonitor-<version>.zip.
# Needs macOS 14+ with Xcode or the Command Line Tools. Signing: see sign-arguments.sh (ad hoc unless MACOS_SIGN_IDENTITY is set).
# Updates: when UPDATE_FEED_URL and SPARKLE_PUBLIC_KEY are set they go into Info.plist, otherwise the build says "updates not set up".
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/sign-arguments.sh
if [[ "$(uname -s)" != Darwin ]]; then
  echo "Build this application on macOS 14+ with Xcode Command Line Tools." >&2
  exit 1
fi
swift build -c release --arch arm64 --arch x86_64
BIN_DIR="$(swift build -c release --arch arm64 --arch x86_64 --show-bin-path)"
APP="build/PDUMonitor.app"
VERSION="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' scripts/Info.plist)"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources" "$APP/Contents/Frameworks"
cp "$BIN_DIR/PDUMonitor" "$APP/Contents/MacOS/PDUMonitor"
cp scripts/Info.plist "$APP/Contents/Info.plist"
if [[ -n "${UPDATE_FEED_URL:-}" && -n "${SPARKLE_PUBLIC_KEY:-}" ]]; then
  /usr/libexec/PlistBuddy -c "Add :SUFeedURL string $UPDATE_FEED_URL" "$APP/Contents/Info.plist"
  /usr/libexec/PlistBuddy -c "Add :SUPublicEDKey string $SPARKLE_PUBLIC_KEY" "$APP/Contents/Info.plist"
fi
if [[ -n "${GITHUB_RUN_NUMBER:-}" ]]; then
  /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $((1000 + GITHUB_RUN_NUMBER))" "$APP/Contents/Info.plist"
fi
swift scripts/make-icon.swift build/AppIcon.iconset
iconutil -c icns build/AppIcon.iconset -o "$APP/Contents/Resources/AppIcon.icns"
while IFS= read -r -d '' bundle; do
  cp -R "$bundle" "$APP/Contents/Resources/"
done < <(find "$BIN_DIR" -maxdepth 1 -name '*.bundle' -print0)
SPARKLE_FRAMEWORK="$(find .build/artifacts -path '*/macos-arm64_x86_64/Sparkle.framework' -type d -print -quit)"
[[ -n "$SPARKLE_FRAMEWORK" ]] || { echo 'Sparkle framework not found' >&2; exit 1; }
ditto "$SPARKLE_FRAMEWORK" "$APP/Contents/Frameworks/Sparkle.framework"
# Re-sign the nested helpers of Sparkle from the inside out; --deep alone cannot repair all of them.
while IFS= read -r -d '' nested; do
  codesign "${SIGN[@]}" "$nested"
done < <(find "$APP/Contents/Frameworks/Sparkle.framework" -depth \( -name '*.xpc' -o -name '*.app' -o -name 'Autoupdate' \) -print0)
codesign "${SIGN[@]}" "$APP/Contents/Frameworks/Sparkle.framework"
install_name_tool -add_rpath @executable_path/../Frameworks "$APP/Contents/MacOS/PDUMonitor"
codesign "${SIGN[@]}" --deep "$APP"
codesign --verify --deep --strict "$APP"
lipo "$APP/Contents/MacOS/PDUMonitor" -verify_arch arm64 x86_64
ditto -c -k --sequesterRsrc --keepParent "$APP" "build/PDUMonitor-$VERSION.zip"
echo "Built: $APP"
