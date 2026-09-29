#!/bin/sh
# Build mpv-shim.app from a darwin binary. Run from the repository root:
#   ./packaging/macos/make-app.sh [arch]        # arch: arm64 | amd64 (default: host)
#
# The app bundle is unsigned: macOS will refuse to launch a downloaded copy
# until you clear the quarantine flag (xattr -dr com.apple.quarantine).
set -eu

APP="mpv-shim.app"
BUNDLE_ID="io.github.ikac.mpv-shim"
VERSION="${VERSION:-0.1.0}"
ARCH="${1:-$(uname -m)}"

cd "$(dirname "$0")/../.."
# The systray needs cgo on macOS, so a cross build from Linux gets the TUI
# only; build on a Mac to get the tray into the bundle.
TAGS="nosystray"
[ "$(uname -s)" = "Darwin" ] && TAGS=""

CGO_ENABLED=0 GOOS=darwin GOARCH="$ARCH" \
  go build -trimpath ${TAGS:+-tags "$TAGS"} \
  -ldflags "-s -w -X main.version=$VERSION" -o "dist/$APP/Contents/MacOS/mpv-shim" .

mkdir -p "dist/$APP/Contents/Resources"
cp packaging/icon/io.github.ikac.mpv-shim.png "dist/$APP/Contents/Resources/icon.png"

cat > "dist/$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Jellyfin MPV Shim</string>
  <key>CFBundleDisplayName</key><string>Jellyfin MPV Shim</string>
  <key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
  <key>CFBundleExecutable</key><string>mpv-shim</string>
  <key>CFBundleIconFile</key><string>icon.png</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$VERSION</string>
  <key>CFBundleVersion</key><string>$VERSION</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

echo "built dist/$APP  (install: cp -R dist/$APP /Applications/)"
