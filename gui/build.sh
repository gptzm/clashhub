#!/usr/bin/env bash
# Build ClashHub.app from gui/main.swift.
#
#   ./build.sh                 -> ./ClashHub.app next to this script
#   ./build.sh ~/Applications  -> ~/Applications/ClashHub.app
#
# The explicit macOS 14 target matters: swiftc's default minimum
# (arm64-apple-macosx28.0 on recent toolchains) makes LaunchServices refuse to
# launch the bundle with error -10825.
set -euo pipefail
cd "$(dirname "$0")"

arch=$(uname -m)
case "$arch" in
  arm64|x86_64) ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
target="${arch}-apple-macosx14.0"

dest="${1:-$PWD}"
case "$dest" in
  *.app) app="$dest" ;;
  *) app="$dest/ClashHub.app" ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "==> swiftc ($target)"
swiftc -swift-version 5 -O -parse-as-library -target "$target" main.swift -o "$work/ClashHub"

echo "==> bundle $app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp Info.plist "$app/Contents/Info.plist"
cp Brand.icns "$app/Contents/Resources/Brand.icns"
cp "$work/ClashHub" "$app/Contents/MacOS/ClashHub"
chmod +x "$app/Contents/MacOS/ClashHub"

# When a daemon build is present, bundle it so the app is self-contained.
if [ -x ../clashhub ]; then
  cp ../clashhub "$app/Contents/Resources/clashhub"
  chmod +x "$app/Contents/Resources/clashhub"
fi

# Ad-hoc signature is enough for local use; replace with a Developer ID
# identity before distributing the bundle to other machines.
codesign --force --deep --sign - "$app"
echo "==> done: $app"
