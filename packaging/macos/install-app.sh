#!/bin/sh
# Install the built bundle into /Applications (or $1).
set -eu
SRC="${1:-dist/mpv-shim.app}"
DEST="${2:-/Applications}"
[ -d "$SRC" ] || { echo "build it first: ./packaging/macos/make-app.sh"; exit 1; }
cp -R "$SRC" "$DEST/"
xattr -dr com.apple.quarantine "$DEST/mpv-shim.app" 2>/dev/null || true
echo "installed $DEST/mpv-shim.app"
