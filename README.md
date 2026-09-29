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

## Configuration

`config.json` (all keys optional, defaults shown):

| key | default | meaning |
|---|---|---|
| `server` | — | Jellyfin URL, e.g. `http://localhost:8096` |
| `username` | — | last logged-in user |
| `player_name` | `mpv` | device name shown in the Jellyfin web UI |
| `client_uuid` | generated | stable device id |
| `mpv_path` | `mpv` | mpv binary |
| `mpv_config_dir` | mpv's own | our mpv config dir; `""` = the user's `~/.config/mpv` |
| `local_kbps` / `remote_kbps` | 10000 / 25000 | requested transcode bitrate |
| `transcode_h265` / `force_h264` | false / false | device-profile knobs |
| `skip_intro` / `skip_credits` | false / false | auto-seek past intro/outro segments |
| `idle_stop` | true | stop playback when idle |
| `idle_delay_s` | 3600 | seconds of idleness (playing nothing, or paused) before that stop |
| `pause_report` | true | report progress immediately on pause/unpause |
| `ignore_ssl` | false | skip TLS verification |
| `log_level` | `info` | mpv `--msg-level` (quiet/error/warn/info/debug) |
| `write_log` | false | also append to `<config dir>/mpv-shim.log` |
| `media_keys` | true | let mpv handle media keys |

## Keys (in the mpv window)

`c` menu · `arrows` seek (±5 s / ±60 s) · `SPACE` pause · `f` fullscreen ·
`q` stop · `<` `>` previous/next · `w` mark watched + next · `u` stop + mark
unwatched · `s` screenshot (saved to `<config dir>/screenshots/`). With the menu
open the same keys navigate it; the web/mobile remote drives the same actions.

## Release builds

```sh
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags "-X main.version=1.0.0" -o dist/mpv-shim-linux-amd64 .
CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -ldflags "-X main.version=1.0.0" -o dist/mpv-shim-linux-arm64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-X main.version=1.0.0" -o dist/mpv-shim-windows-amd64.exe .
# macOS needs cgo (Cocoa) for the tray — build on a Mac, or without it:
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -tags nosystray -ldflags "-X main.version=1.0.0" -o dist/mpv-shim-darwin-arm64 .
```

Status: M0–M5 implemented; see [progress.md](progress.md) for what is verified live
and what still needs a manual pass.
