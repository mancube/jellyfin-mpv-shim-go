#!/bin/sh
# Build the Arch package from this checkout.
#
#   ./packaging/arch/build.sh            # build the .pkg.tar.zst
#   ./packaging/arch/build.sh --install  # …and install it with pacman
#
# makepkg insists on a PKGBUILD in the working directory, so we copy ours next
# to the sources for the duration of the build and clean up afterwards.
set -eu

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO"

install_flag=""
[ "${1:-}" = "--install" ] && install_flag=1

cleanup() { rm -f "$REPO/PKGBUILD" "$REPO/mpv-shim-go.install"; }
trap cleanup EXIT INT TERM

cp packaging/arch/PKGBUILD packaging/arch/mpv-shim-go.install "$REPO/"

# Keep the build tree out of the repository.
srcdir="$REPO"
export PKGDEST="$REPO/dist/arch"
mkdir -p "$PKGDEST"
makepkg --noconfirm --nodeps "$@"

pkg="$(ls -t "$PKGDEST"/mpv-shim-go-*.pkg.tar.* 2>/dev/null | head -1)"
[ -n "${pkg:-}" ] || { echo "no package produced"; exit 1; }
echo
echo "built $pkg"
if [ -n "$install_flag" ]; then
  sudo pacman -U --noconfirm "$pkg"
  echo "installed: mpv-shim  (remove with: sudo pacman -R mpv-shim-go)"
fi
