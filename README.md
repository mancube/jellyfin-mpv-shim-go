# mpv-shim-go

Jellyfin v12-compatible cast client → local mpv. A Go rewrite of
[jellyfin-mpv-shim](https://github.com/jellyfin/jellyfin-mpv-shim) v2.10 with a
reduced feature set.

- [PLAN.md](PLAN.md) — scope, architecture, milestones
- [RESEARCH.md](RESEARCH.md) — protocol research (wire formats, endpoints)

## Build

```sh
go build -o mpv-shim .
```

## Usage

```sh
mpv-shim                                                 # play: TUI status + tray, WS session loop
mpv-shim --headless                                      # no TUI/tray, logs only (daemon/systemd)
mpv-shim setup                                           # TUI wizard: add account (password or Quick Connect)
mpv-shim login http://localhost:8096 admin mypassword     # log in from the CLI, persist credentials
mpv-shim accounts                                        # list saved accounts
mpv-shim accounts rm 0                                   # remove account by index
mpv-shim -status                                         # connection check and exit
```

In the player window: `c` opens the OSD menu (audio/subtitles), arrows seek,
`SPACE` pauses, `f` fullscreen, `q` stops, `<`/`>` previous/next.

Build without the desktop tray (e.g. macOS without Xcode CLT):

```sh
go build -tags nosystray -o mpv-shim .
```

Config lives in `<config dir>/config.json`, credentials in `cred.json`
(`~/.config/mpv-shim/`, `%appdata%\mpv-shim\`, `~/Library/Application Support/mpv-shim/`).

Status: M0–M4 done. M5 (hardening + release) in [PLAN.md](PLAN.md) §6.
