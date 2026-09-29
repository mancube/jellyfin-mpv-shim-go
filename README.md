# mpv-shim-go

A **Jellyfin cast client for [mpv](https://mpv.io/)**, written in Go.

Your Jellyfin server sees it as a second player in the web UI: cast to it from
the browser or a phone, and the video plays in a local mpv window. Full remote
control (play/pause, seek, volume, mute, audio & subtitle tracks), an in-player
OSD menu, a terminal UI for setup and status, and a desktop tray icon.

It is a focused rewrite of
[jellyfin-mpv-shim](https://github.com/jellyfin/jellyfin-mpv-shim) v2.10 — same
protocol behaviour and the same in-player UX, in one static binary with a much
smaller feature surface.

```
jellyfin-web / mobile app
        │  WebSocket  /socket          REST /Sessions/Playing…
        ▼
   mpv-shim  ─────────────────────────►  mpv (JSON IPC over a unix socket)
        │
        └── TUI (setup + live status) · desktop tray · OSD menu
```

## Features

| | |
|---|---|
| **Casting** | Appears as a cast device in the Jellyfin UI; direct play (local file or `/Videos/{id}/stream`) and HLS transcode, with media-source fallback. Works with Jellyfin v11 and v12. |
| **Remote control** | Play/pause, seek, next/previous, stop, volume, mute, audio & subtitle track switching, fullscreen, screenshot — from the web UI, the mobile apps, or mpv keybindings. |
| **State sync** | Position, pause, mute, volume and track changes are reported to the server as they happen (mpv property observers), so the remote panel follows the player within ~1 s. |
| **OSD menu** | `c` opens a menu drawn over the video (audio, subtitles, screenshot, quit) — driven by the keyboard, the mpv OSC or the remote's navigation buttons. |
| **Queue** | PlayNext/PlayLast, auto-advance on end-of-file, mark watched/unwatched, intro & credits skipping. |
| **Resilience** | WebSocket reconnect with exponential backoff and a `/Sessions` health check; mpv crash → respawn and resume at the last position; transcode teardown; bounded crash-restart loop; idle stop. |
| **Setup & status** | Bubble Tea TUI: add accounts with a password or Quick Connect, watch connection state and live playback, tail the log. Desktop systray with the same menu as upstream. |
| **Headless** | `--headless` runs as a plain daemon: logs to stdout/file, no TUI, no tray. |

Deliberately **not** included: music, live TV, the in-mpv library browser,
offline sync, SyncPlay, display mirroring, shader packs/SVP, trickplay
thumbnails, bulk subtitles, Discord presence, i18n.

## Install

One static binary, plus a launcher entry. Requires an `mpv` binary on the
machine (tested against mpv 0.41); everything else is optional.

**Arch / pacman** — builds from this checkout and installs a launcher entry:

```sh
./packaging/arch/build.sh --install     # makepkg + pacman -U
sudo pacman -R mpv-shim-go              # and remove it again
```

**Any Linux** — binary + `.desktop` + icon:

```sh
sudo make install            # PREFIX=/usr/local by default
sudo make uninstall
```

**macOS** — `.app` bundle (unsigned; see [packaging/README.md](packaging/README.md)):

```sh
./packaging/macos/make-app.sh && ./packaging/macos/install-app.sh
```

**Windows** — a portable zip from any OS, or an Inno Setup installer built on
Windows:

```sh
./packaging/windows/build-portable.sh
```

Details, systemd unit and per-platform caveats: [packaging/README.md](packaging/README.md).

## Build from source

Requires Go 1.24+.

```sh
make            # build ./mpv-shim
make test       # go vet + go test -race
```

Cross-compiles cleanly (the tray is the only platform-specific part; drop it
with `-tags nosystray`):

```sh
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "-X main.version=1.0.0" -o dist/mpv-shim-linux-amd64 .
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags "-X main.version=1.0.0" -o dist/mpv-shim-linux-arm64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-X main.version=1.0.0" -o dist/mpv-shim-windows-amd64.exe .
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -tags nosystray -ldflags "-X main.version=1.0.0" -o dist/mpv-shim-darwin-arm64 .
```

macOS builds **with** the tray need cgo (Cocoa), so build those on a Mac.

## Usage

```sh
mpv-shim                                              # play: session loop + TUI + tray
mpv-shim --headless                                   # daemon mode, logs only
mpv-shim setup                                        # TUI: add/remove accounts
mpv-shim login http://localhost:8096 admin mypass     # non-interactive login
mpv-shim accounts                                     # list saved accounts
mpv-shim accounts rm 0                                # remove one
mpv-shim -status                                      # connection check and exit
mpv-shim -debug                                       # …and log every remote command
```

On first run without an account, the wizard opens automatically when there is a
terminal.

### Keys in the mpv window

| key | action | key | action |
|---|---|---|---|
| `c` | OSD menu | `←` `→` | seek ∓5 s |
| `↑` `↓` | seek ±60 s (or skip the intro) | `SPACE` | play/pause |
| `f` | fullscreen | `ESC` | leave fullscreen / close the menu |
| `q` | stop | `<` `>` | previous / next item |
| `w` | mark watched + next | `u` | stop + mark unwatched |
| `s` | screenshot | | |

### Tray menu

Status and now-playing lines, **Configure Servers…** (the TUI wizard),
**Player Menu (OSD)**, **Open Config Folder**, **Open Log File** (when
`write_log` is on) and **Quit**. The icon is upstream's artwork with a status
dot: green connected, amber reconnecting, grey offline.

## Configuration

`<config dir>/config.json` — every key is optional:

| key | default | meaning |
|---|---|---|
| `server` | — | Jellyfin URL, e.g. `http://localhost:8096` |
| `username` | — | last logged-in user |
| `player_name` | hostname | device name shown in the Jellyfin UI |
| `client_uuid` | generated | stable device id |
| `mpv_path` | `mpv` | mpv binary |
| `mpv_config_dir` | mpv's own | our mpv config dir; `""` = the user's `~/.config/mpv` |
| `local_kbps` / `remote_kbps` | 10000 / 25000 | requested transcode bitrate |
| `transcode_h265` / `force_h264` | false / false | device-profile knobs |
| `skip_intro` / `skip_credits` | false / false | auto-seek past intro/outro segments |
| `idle_stop` | true | stop playback when idle |
| `idle_delay_s` | 3600 | seconds idle (nothing playing, or paused) before that stop |
| `pause_report` | true | report progress immediately on pause/unpause |
| `ignore_ssl` | false | skip TLS verification |
| `log_level` | `info` | mpv `--msg-level` (quiet/error/warn/info/debug) |
| `write_log` | false | also append to `<config dir>/mpv-shim.log` |
| `media_keys` | true | let mpv handle media keys |

Config and credentials live in `~/.config/mpv-shim/`, `%appdata%\mpv-shim\` or
`~/Library/Application Support/mpv-shim/`. `cred.json` is mode 0600 and holds
one entry per server+user (multiple accounts can be stored; one plays at a
time).

## How it works

| package | what lives there |
|---|---|
| `jfin/` | The Jellyfin protocol: REST client with non-legacy `MediaBrowser` auth, WebSocket (`/socket`) with keepalive, `MessageId` dedupe, backoff reconnect and health check, `PlaybackInfo`/media-source selection, device profile, session reports, Quick Connect, credential store. |
| `player/` | The mpv side: JSON IPC over a unix socket, request/response correlation, property observers, process supervision (crash → respawn + resume), the playback state machine (queue, watched, intro skip, idle stop, transcode teardown) and the OSD menu. |
| `ui/` | Bubble Tea TUI (account wizard, live status), the log ring buffer, styling and the systray (with a `-tags nosystray` stub). |
| `main.go`, `config.go` | Flags and wiring, settings, CLI subcommands, signal handling and the shutdown order. |

Design notes worth knowing:

- **One player.** Like upstream, a single active playback; the queue is the
  current item's parent list.
- **Events, not polling.** State reaches the server through mpv's
  `property-change` events (pause/mute/volume/seeking/time-pos/aid/sid), with a
  1.5 s throttle for position updates and echo suppression for our own changes.
- **No global lock maze.** A single `sync.Mutex` per `Player`; the OSD menu has
  its own lock and the order is always `menu → player`.
- **Secrets stay out of URLs.** Jellyfin v12 has legacy query tokens disabled,
  so mpv gets the `Authorization` header via `--http-header-fields` and the
  token never appears in a URL or in `ps`.

More detail lives in [PLAN.md](PLAN.md) (scope and milestones) and
[RESEARCH.md](RESEARCH.md) (protocol/wire notes). [progress.md](progress.md) is
the development log: what was verified against a live server, and what is still
worth a manual pass.

## Development

```sh
go vet ./... && go test -race -count=1 ./...   # the gate before every commit
make test                                       # the same, as a target
```

Tests are stdlib `testing` + `httptest` + a fake mpv — no frameworks, no
network. They cover URL/source selection, stream mapping, queue transitions,
the WS lifecycle, the playback state machine (including the crash/end-of-file
paths), the OSD menu, the TUI models and the tray icon.

Verified against a real Jellyfin 12 server and real mpv 0.41: direct play,
forced transcode, queue advance, progress reporting, crash respawn + resume,
remote control, the OSD menu and the TUI/tray. Known gaps are listed in
[progress.md](progress.md).

## Credits

Protocol, behaviour and the icon assets are ported from
[Jellyfin MPV Shim](https://github.com/jellyfin/jellyfin-mpv-shim) by Izzie
Walton, Weston Nielson and contributors (GPLv3 — see [LICENSE](LICENSE)).
