#!/usr/bin/env bash
# Build a distributable macOS installer image.
#
#   ./package.sh                 -> ../dist/ClashHub-<version>.dmg (+ .zip)
#
# The app bundle embeds the clashhub daemon and gen_config.py, so the installed
# app can start the proxy without a separate ~/clashhub checkout. mihomo and
# Python/PyYAML are still runtime dependencies and are not bundled.
set -euo pipefail
cd "$(dirname "$0")"
root=$(cd .. && pwd)
version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' Info.plist)
out="$root/dist"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

echo "==> build ClashHub.app"
./build.sh "$stage" >/dev/null

echo "==> embed daemon"
(cd "$root" && go build -o "$stage/ClashHub.app/Contents/Resources/clashhub" ./cmd/clashhub)
cp "$root/gen_config.py" "$stage/ClashHub.app/Contents/Resources/gen_config.py"
chmod +x "$stage/ClashHub.app/Contents/Resources/clashhub"
# Re-sign after adding executables to the bundle.
codesign --force --deep --sign - "$stage/ClashHub.app"

mkdir -p "$out"
echo "==> stage installer"
ln -s /Applications "$stage/Applications"

echo "==> create DMG"
dmg="$out/ClashHub-$version.dmg"
rm -f "$dmg"
hdiutil create -quiet -volname "ClashHub $version" -srcfolder "$stage" -ov -format UDZO "$dmg"

echo "==> create ZIP"
zip="$out/ClashHub-$version.zip"
rm -f "$zip"
ditto -c -k --sequesterRsrc --keepParent "$stage/ClashHub.app" "$zip"

echo "==> done"
echo "  $dmg"
echo "  $zip"
echo "Ad-hoc signed: on first launch use right-click -> Open, or run"
echo '  xattr -dr com.apple.quarantine /Applications/ClashHub.app'
