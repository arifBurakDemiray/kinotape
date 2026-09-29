#!/bin/bash
# Builds Kinotape into dist/: Kinotape-macOS.zip (a universal Kinotape.app) and Kinotape.exe (Windows).
# The app bundle is assembled in a temporary folder so Spotlight and Launchpad never list a second copy.
# Usage: ./build.sh            build both
#        ./build.sh install    build, then put Kinotape.app in ~/Applications
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
VERSION="${VERSION:-2.0.0}"
DIST=dist
WORK="$(mktemp -d)"
APP="$WORK/Kinotape.app"
LDFLAGS="-s -w"
trap 'rm -rf "$WORK"' EXIT

rm -rf "$DIST"
mkdir -p "$DIST"
go run ./tools/icon "$WORK/icon.png"

echo "macOS: building arm64 and amd64"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$WORK/kinotape-arm64" .
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$WORK/kinotape-amd64" .
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
lipo -create -output "$APP/Contents/MacOS/Kinotape" "$WORK/kinotape-arm64" "$WORK/kinotape-amd64"
cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Kinotape</string>
  <key>CFBundleDisplayName</key><string>Kinotape</string>
  <key>CFBundleIdentifier</key><string>io.github.arifburakdemiray.kinotape</string>
  <key>CFBundleExecutable</key><string>Kinotape</string>
  <key>CFBundleIconFile</key><string>Kinotape</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$VERSION</string>
  <key>CFBundleVersion</key><string>$VERSION</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST
ICONSET="$WORK/Kinotape.iconset"
mkdir -p "$ICONSET"
for px in 16 32 128 256 512; do
  sips -z "$px" "$px" "$WORK/icon.png" --out "$ICONSET/icon_${px}x${px}.png" >/dev/null
  sips -z "$((px * 2))" "$((px * 2))" "$WORK/icon.png" --out "$ICONSET/icon_${px}x${px}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/Kinotape.icns"
codesign --force --deep --sign - "$APP" >/dev/null 2>&1 || true
ditto -c -k --norsrc --noextattr --keepParent "$APP" "$DIST/Kinotape-macOS.zip"

echo "Windows: building amd64"
go run github.com/tc-hib/go-winres@v0.3.3 simply --icon "$WORK/icon.png" --manifest gui \
  --product-name Kinotape --file-description Kinotape --product-version "$VERSION" --file-version "$VERSION" --arch amd64
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS -H=windowsgui" -o "$DIST/Kinotape.exe" .
rm -f rsrc_windows_*.syso

if [ "${1:-}" = "install" ]; then
  "$HOME/Applications/Kinotape.app/Contents/MacOS/Kinotape" stop 2>/dev/null || true
  rm -rf "$HOME/Applications/Kinotape.app"
  mkdir -p "$HOME/Applications"
  ditto "$APP" "$HOME/Applications/Kinotape.app"
  echo "Installed ~/Applications/Kinotape.app"
fi
ls -la "$DIST"
