# Packaging

One static binary, three install paths. No runtime dependencies beyond `mpv`
itself; the desktop entry, icon and docs are plain files.

| platform | how | what you get |
|---|---|---|
| **Arch / pacman** | `makepkg -f packaging/arch/PKGBUILD` then `pacman -U ./mpv-shim-go-*.pkg.tar.zst` | `/usr/bin/mpv-shim`, launcher entry, 256 px icon. `pacman -R mpv-shim-go` removes it. |
| **Any Linux** | `sudo make install` (`PREFIX=/usr` to override) | same three files; `make uninstall` removes them |
| **macOS** | `./packaging/macos/make-app.sh` → `packaging/macos/install-app.sh` | `mpv-shim.app` in /Applications (unsigned; see below) |
| **Windows** | `packaging/windows/build-portable.sh` (any OS) → zip, or `iscc` on Windows for an installer | `mpv-shim.exe` (+ Start Menu shortcut with the Inno Setup script) |

## Desktop entry

`packaging/desktop/io.github.ikac.mpv-shim.desktop` is `Terminal=true` — the app
owns a terminal for its TUI, so the launcher opens one. It carries two extra
actions:

- **Start in background** → `mpv-shim --headless` (no TUI, logs only; this is
  what you want for a systemd unit or a WM startup entry)
- **Check connection and exit** → `mpv-shim -status`

The icon is upstream's 128/256 px artwork (`ui/assets/jellyfin-*.png`,
GPLv3, see `LICENSE`). The tray draws its own status dot on top at runtime.

## Arch specifics

The PKGBUILD builds from the checkout it is invoked in, so keep it inside the
repository:

```sh
cd ~/Desktop/mpv-shim-go
./packaging/arch/build.sh              # makepkg; runs `go vet` + `go test` first
sudo pacman -U ./dist/arch/mpv-shim-go-*.pkg.tar.zst
```

`build.sh` copies the PKGBUILD next to the sources (makepkg insists on that)
and removes it again afterwards; the package lands in `dist/arch/`.
`pkgver()` follows the newest git tag and `pkgrel()` carries the commit count,
the short sha and a `.dirty` flag, so a local build is always distinguishable
from a release one — and `git tag v0.1.0 && git push --tags` turns the next
build into a clean `0.1.0-N`.

Upgrade cycle: `git pull && ./packaging/arch/build.sh --install`. Two optdepends, both soft:

- `xdg-utils` — the tray's *Open Config Folder*
- `libappindicator-gtk3` — the tray on GNOME (KDE has StatusNotifier built in)

Config and credentials are per user (`~/.config/mpv-shim/`), so a system-wide
package never owns state and `pacman -R` leaves nothing behind.

Running it under systemd (optional):

```ini
# ~/.config/systemd/user/mpv-shim.service
[Service]
ExecStart=/usr/bin/mpv-shim --headless
Restart=on-failure
[Install]
WantedBy=default.target
```

## macOS specifics

The bundle is **unsigned and unnotarised**: Gatekeeper blocks a locally built
app on first launch. Clear it once:

```sh
xattr -dr com.apple.quarantine /Applications/mpv-shim.app
```

mpv itself is a separate install (`brew install mpv`). The tray needs cgo, so
build the bundle on a Mac for it to be included; the cross-compiled binary
falls back to the TUI only.

## Windows specifics

`build-portable.sh` produces a zip that runs anywhere without admin rights.
For a real installer, build on Windows with
[Inno Setup 6](https://jrsoftware.org/isinfo.php):

```powershell
go build -ldflags "-X main.version=0.1.0" -o build\mpv-shim.exe .
iscc /DMyAppVersion=0.1.0 packaging\windows\mpv-shim.iss
```

mpv is not bundled — point the user at `winget install mpv.sh`.
