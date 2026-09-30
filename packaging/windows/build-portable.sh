#!/bin/sh
# Portable Windows build: a zip with the exe, the icon and the docs. No
# installer, no admin rights — unzip anywhere and run mpv-shim.exe.
# Run from the repository root on any OS with Go installed.
set -eu
VERSION="${VERSION:-1.0.0}"
OUT="dist/mpv-shim-${VERSION}-windows-amd64"

cd "$(dirname "$0")/../.."
mkdir -p "$OUT"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/mpv-shim.exe" .
cp packaging/icon/io.github.ikac.mpv-shim.png "$OUT/"
cp README.md LICENSE "$OUT/"
( cd dist && zip -qr "$(basename "$OUT").zip" "$(basename "$OUT")" )
echo "built dist/$(basename "$OUT").zip"
