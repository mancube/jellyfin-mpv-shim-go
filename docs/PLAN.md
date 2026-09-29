# mpv-shim-go — implementation plan

Go rewrite of `jellyfin/jellyfin-mpv-shim` v2.10 (last v2 release) as a minimal single
binary: **Jellyfin v12-compatible** cast surface → local mpv, with a **TUI + systray
for setup/status** and an **OSD menu** for audio/subtitle switching.

Scope deliberately excludes the v3 feature set (in-mpv library browser, offline sync,
music/books, display mirroring, SyncPlay, shader packs/SVP, trickplay thumbnails,
bulk subtitles, Discord presence, i18n).

Specs this plan was researched against (all in `RESEARCH.md` unless noted):
- v2.10.0 source tree (protocol + state-machine reference)
- `jellyfin-apiclient-python` 1.19.0 (the client lib v3 pins; v12-compatible)
- v3.0.0 shim (the officially v12-compatible upstream — used to extract the v12 deltas)
- Jellyfin server `SessionMessageType` enum (wire message types)

---

## 1. Requirements

| # | Requirement | Source |
|---|---|---|
| R1 | Appear as a castable device in the Jellyfin web UI (v12) and receive Play/Playstate/GeneralCommand | user |
| R2 | Play video via mpv: direct play (local path or `/Videos/{id}/stream`) and transcode (HLS), with media-source fallback | user (cast surface) |
| R3 | Full remote control from web/mobile: play, pause, seek, next/prev, stop, volume, mute, audio/subtitle track switching | user |
| R4 | Report playback state: start, 5 s progress, stop; mark watched; queue advance | upstream parity |
| R5 | Lightweight UI: TUI (setup: add/remove accounts, password + Quick Connect; live status) + desktop systray. No webview / window toolkit | user |
| R6 | OSD menu: open with `c`, navigate with keyboard + remote, switch audio & subtitle tracks (incl. external subs), render like upstream (OSD text + background box) | user |
| R7 | Jellyfin v12 protocol (non-legacy auth), v11 where possible | user |
| R8 | Resilience: WS reconnect w/ backoff + health check, mpv crash restart, transcode teardown | upstream parity |
| R9 | Config file (JSON) + CLI login fallback for headless use | ponytail |
| R10 | Intro/outro skip (server segments), idle stop, graceful shutdown | upstream parity (cheap) |

Out of scope: music, Live TV, multi-account concurrency (multi-account *storage* yes,
active playback is single-player like upstream), SyncPlay, GUI browsing.

---

## 2. Jellyfin v12 protocol notes (verified)

These are the v12-relevant facts; everything else matches v2.10-era behavior and is
documented in RESEARCH.md §1.

1. **Auth header (all REST + WS):**
   `Authorization: MediaBrowser Client="Jellyfin MPV Shim", Device="<name>", DeviceId="<uuid>", Version="<ver>", Token="<token>"`
   — the non-legacy scheme; unchanged in v12.
2. **Legacy query-token is dead by default** (`EnableLegacyAuthorization` off in v12):
   never put a token in URLs ourselves. Instead, pass mpv the auth header via
   `--http-header-fields='Authorization: ...'` so mpv's own media/subtitle requests
   authenticate without a token in the URL/`ps` (same approach apiclient 1.19 recommends
   via `request_url(include_apikey=False)`).
3. **Login:** `POST /Users/AuthenticateByName` body `{"username": ..., "Pw": ...}`.
4. **Quick Connect** (for the GUI): `GET /QuickConnect/Enabled`,
   `POST /QuickConnect/Initiate` → `{Secret, Code}`, then poll
   `GET /QuickConnect/Exchange?code=<Code>&Secret=<secret>` → credentials.
5. **WebSocket:** `GET {server}/socket` with the MediaBrowser header; envelope
   `{"MessageType","Data","MessageId"}`; `ForceKeepAlive` → periodic `KeepAlive`;
   reconnect backoff 1→100 s; health check `GET /Sessions` (our DeviceId present?).
6. **Sessions REST:** `POST /Sessions/Capabilities/Full` (on (re)connect),
   `POST /Sessions/Playing`, `POST /Sessions/Playing/Progress`,
   `POST /Sessions/Playing/Stopped`.
7. **Items:** `GET /Users/{uid}/Items/{id}?Fields=...`,
   `POST /Items/{id}/PlaybackInfo` (body carries `UserId`, `DeviceProfile`,
   optional `AudioStreamIndex`/`SubtitleStreamIndex`/`MediaSourceId`/`StartTimeTicks`),
   `POST|DELETE /Users/{uid}/PlayedItems/{id}`,
   `GET /MediaSegments/{id}?includeSegmentTypes=Intro&includeSegmentTypes=Outro`.
8. **Teardown:** `DELETE /Videos/ActiveEncodings?DeviceId=..&PlaySessionId=..`,
   `POST /LiveStreams/Close?liveStreamId=..`.
9. **Capabilities** sent on connect: as v3's `constants.CAPABILITIES` (adds
   `ToggleContextMenu`, `GoToSearch` vs v2.10) — the web UI draws remote buttons from
   this list, so the remote panel matches.
10. **Device identity:** stable per-install `DeviceId` UUID + `ClientName`/`Version`
    (v12 shows the device in the web UI from this; keep the same name so UI affordances
    like the "Play on" panel behave identically).

---

## 3. Architecture

```
┌────────────────────────────────────────────────────────────────────┐
│ mpv-shim (single binary)                                          │
│                                                                    │
│  main ── config ── credstore                                       │
│    │                                                               │
│    ├─ jfin.Client (REST, MediaBrowser auth) ──► /socket (WS)      │
│    │        │                     ▲                               │
│    │        │                     │ Play/Playstate/GeneralCommand │
│    │        ▼                     │                               │
│    │   jfin.Media (PlaybackInfo,  └── session.go: /Sessions/*     │
│    │     media-source pick, URL,     (start/progress/stop)        │
│    │     intros)                                                 │
│    │        │                                                   │
│    │        ▼                                                   │
│    └─ player.Player ──► mpv (spawn, JSON IPC over unix socket)   │
│           ├─ state.go: queue, watched, intro skip                │
│           └─ menu.go: OSD menu (show-text + osd-* props,         │
│                       input-command-hook key claiming)           │
│                                                                    │
│  ui: TUI (setup + live status, bubbletea) + systray (status/menu) │
└────────────────────────────────────────────────────────────────────┘
```

Threading model: goroutine per concern (WS reader, IPC reader, timeline ticker, mpv
process monitor). Shared player state behind one mutex (upstream uses RLock + task
queues; Go makes the queues unnecessary — call directly under the lock, IPC is fast).

## 4. Project layout

```
mpv-shim/
├── go.mod                     # module github.com/<you>/mpv-shim
├── main.go                    # flags, wiring, signals, graceful shutdown        ~200
├── config.go                  # Settings struct + defaults + JSON load/save     ~150
│                              #   (XDG/AppData path; ~25 keys)
├── jfin/
│   ├── types.go               # Item, MediaSource, MediaStream, PlaybackInfo,   ~250
│   │                          #   session payload structs, capabilities JSON
│   ├── client.go              # REST client: base URL, auth header,             ~200
│   │                          #   Get/Post/Delete, timeouts, light retry
│   ├── auth.go                # AuthenticateByName, QuickConnect,               ~150
│   │                          #   POST capabilities, health check
│   ├── cred.go                # credentials store (JSON file, N accounts),      ~120
│   │                          #   active-account selection
│   ├── ws.go                  # dial /socket, keepalive, MessageId dedupe,      ~250
│   │                          #   backoff reconnect, dispatch table
│   ├── media.go               # item fetch, PlaybackInfo, media-source pick,    ~350
│   │                          #   URL building (3 cases), intro segments,
│   │                          #   fallback loop
│   ├── profile.go             # DeviceProfile builder (static + h265/HDR opts)  ~100
│   └── jfin_test.go           # unit: profile golden, URL building, source pick
├── player/
│   ├── mpv.go                 # spawn mpv, JSON IPC: request/response           ~500
│   │                          #   correlation, event loop, restart-on-crash,
│   │                          #   config dir, --http-header-fields
│   ├── state.go               # queue (PlayNow/Next/Last), play/stop,          ~450
│   │                          #   watched, pause/seek guards, intro skip,
│   │                          #   transcode track-switch refetch
│   ├── session.go             # timeline: start/5s progress/stop, teardown      ~200
│   ├── menu.go                # OSD menu: render (show-text, osd-back-color,   ~350
│   │                          #   osd-font-size, osd-border-style save/restore),
│   │                          #   nav state machine, input-command-hook,
│   │                          #   audio/sub menus w/ track switching
│   └── player_test.go         # unit: track index mapping, queue transitions
├── ui/
│   ├── tui.go                 # Bubble Tea app: setup flow (add/remove account,   ~350
│   │                          #   password + Quick Connect), live status screen
│   │                          #   (connection, playing, log tail)
│   └── tray.go                # systray: icon + status tooltip, account menu,    ~150
│                              #   quit
└── README.md
```

**Dependencies (3 modules, all pure Go; only the macOS systray touches cgo/Cocoa):**
- `nhooyr.io/websocket` (or `gorilla/websocket`) — the WS client.
- `github.com/charmbracelet/bubbletea` (+ its `bubbles`/`lipgloss` companions) — the TUI.
  The standard pure-Go TUI stack; zero system packages.
- `github.com/energye/systray` — maintained systray fork: Win32 API (Windows),
  StatusNotifier-DBus (Linux, pure Go), Cocoa cgo (macOS). No GUI toolkit.

Everything else stdlib, including mpv JSON IPC.
Estimated total: **~3.7–4.0k LOC Go** (jfin ≈1.4k, player ≈1.5k, config+main+ui ≈0.8k).
Lightest variant of all: no WebKit system package, no HTML, no window toolkit —
a static single binary with zero system dependencies on Linux/Windows.

### Config keys (subset of upstream's ~130)

`server` (URL), `username`, `player_name`, `client_uuid` (stable device id, auto-generated),
`mpv_path`, `mpv_config_dir` (default: own dir; `""` = use user's `~/.config/mpv`),
`local_kbps` / `remote_kbps` (default 10000/25000), `transcode_h265` / `force_h264`
(profile knobs), `skip_intro` / `skip_credits`, `idle_stop` (idle → stop after N),
`idle_delay_s`, `pause_report`, `ignore_ssl`, `log_level`, `write_log`, `media_keys`
bool, `http_header_fields` auto. (Each maps 1:1 to an upstream key where it exists.)

---

## 5. Component design notes

### 5.1 jfin (protocol)
- `Client` = `http.Client` wrapper. Every request: JSON + the MediaBrowser auth header
  (token optional pre-login). `{UserId}` placeholder substitution like the Python lib.
- WS: on open → `post_capabilities`; on `ForceKeepAlive` start a ticker at
  `TimeoutInterval/1000`s sending `KeepAlive`; on close → exponential backoff
  (1,2,4,…,100 s cap; reset only on successful connect — the upstream fix); every
  `health_check_interval` (default 300 s) `GET /Sessions` and force-reconnect if our
  `DeviceId` is missing. `MessageId` set for dedupe.
- `media.go`: port the decision logic verbatim — weight
  `SupportsDirectPlay*50000 + Bitrate/1000`, honor `MediaSourceId`, then
  (local file exists → path | `SupportsDirectPlay` → `/Videos/{id}/stream?static=true&MediaSourceId=..&ApiKey=..`
  [+`LiveStreamId`] | `SupportsTranscoding` → `TranscodingUrl`), fallback loop over
  remaining sources. mpv URL case: header-auth via `--http-header-fields` (R7/§2.2).
  `map_streams` audio/sub index mapping ported 1:1 (embedded vs external vs encode).
- Intros: fetch `MediaSegments` once per item when skip enabled; skip window triggers
  seek at window end (and "skip to start" when inside), with the upstream debounce.

### 5.2 player/mpv.go
- Spawn: `mpv --input-ipc-server=unix:<tmp>/mpv.sock --config-dir=<dir>`
  (or user config dir when `mpv_config_dir` empty) `--http-header-fields=<auth>`;
  retry loop (10×3 s) until socket answers `get_property`. Kill on shutdown
  (SIGTERM → 3 s → SIGKILL).
- IPC: newline-delimited JSON; `request`/`request_id`/`error` correlation; reader
  goroutine dispatches events: `file-loaded`, `end-file`, `shutdown`, `idle`,
  `property-change` (`pause`, `seeking`, `time-pos` sampled, not event-driven),
  `log-message` (→ our logger).
- All ops are thin IPC calls: `loadfile`, `stop`, `seek {ms,flags}`, `set_property`
  (`pause`,`volume`,`mute`,`audio-stream`,`sub-stream`), `get_property`,
  `show-text`, `sub-add`, `input-command-hook`.

### 5.3 player/state.go (the fiddly part — port these guards from v2.10)
- Single active playback; queue = current `Media`'s parent list (PlayNow replaces,
  PlayNext/PlayLast insert). `end-file` → `play_next` (with `is_last` check),
  mark watched at ≥90 % (and on `end-file` when not seeking to end intentionally —
  upstream `watched_skip`/`unwatched_quit` semantics, simplified: mark watched at 90 %
  or on clean end; unwatch on early stop only if `unwatched_quit` setting).
- Pause/seek feedback suppression: `pause_ignore` window + `do_not_handle_pause`
  flag so our own `set_property pause` events don't re-trigger; `last_seek` to ignore
  seek echoes.
- Transcode switch: if current source is a transcode and a track changes,
  re-`POST PlaybackInfo` with the new `AudioStreamIndex`/`SubtitleStreamIndex` and
  `loadfile` the new `TranscodingUrl` (teardown old `PlaySessionId` first). Direct
  play: `audio-stream`/`sub-stream` index, or `sub-add <external-url>` for external
  subtitles (keep per-subtitle-id mapping).
- Intro/outro: from `MediaSegments`; trigger once per window, debounce per upstream.
- Idle: no playback + `idle_delay` → `stop` (stop report with `finished=false`),
  matching `stop_idle`.

### 5.4 player/menu.go (R6)
- Trigger: `c` (input binding) and remote `GeneralCommand` navigation while open.
- Render: exactly upstream's mechanism — `set_property` `osd-back-color=#CC333333`,
  `osd-font-size=40`, `osd-border-style` (save original on open, restore on close,
  tolerate missing property on old mpv), then `show-text <menu lines> -1 1`
  (sticky, level 1 so it sits over OSC). Close: `show-text "" 0 0` + restore.
- Menu tree (minimal):
  ```
  root:  ◄ Audio  |  Subtitles  |  Quit ►   (left/right + ok)
  audio: [0] English (embedded)   ◄ current marker
  subs : [0] Off / [1] English (external) / …
  ```
  built from `MediaStreams` (names + language codes, indexed exactly as
  `map_streams` maps them to mpv track numbers).
- Key claiming while open: `input-command-hook <enable-flag> <key> <cmd>` for
  up/down/left/right/Enter/Esc + our media keys, with the enable-flag property
  toggled on open/close; remote `GeneralCommand`s (Back/Select/MoveUp/MoveDown/…)
  route to the menu while it's open (same as upstream `menu_action`).
  Fallback if `input-command-hook` misbehaves on an old mpv: ship a 30-line
  `menukeys.lua` that forwards keys over IPC (upstream's `mouse.lua` precedent).
- Track switch actions per §5.3.

### 5.5 ui (R5) — TUI + systray, no webview
- **TUI** (Bubble Tea, pure Go) — the setup *and* status surface:
  - first run (no accounts) or `mpv-shim setup`: add account — server URL, username,
    password (hidden input) → Connect; or Quick Connect → shows the 4-char code,
    polls `QuickConnect/Exchange`. Account list with remove.
  - while running in a terminal: live status screen — connection state per account,
    current playback (title, time/duration, pause/seek state), scrolling log tail.
    Hotkeys: `a` add account, `r` remove, `q` quit app.
  - no TTY / `--headless`: no TUI at all; logs to stdout/file (daemon mode).
  - Components: `bubbles/textinput` ×3, small account `list`, `viewport` for logs.
- **Systray** (energye/systray): icon + tooltip (server / playing title); menu =
  status line (disabled item), per-account submenu (connect/disconnect, remove),
  "Add account → run `mpv-shim setup`" (a tray can't take text input — the TUI is the
  input surface), Quit. ~150 LOC.
- CLI parity for headless: `mpv-shim login <url> <user> <pass>` / `--server --username --password`
  flags (upstream CLI-login parity), `mpv-shim accounts` (list/remove).

### 5.6 main / shutdown
- Flags: `--config <path>`, `--debug`, `--headless`, `--server`, `--username`,
  `--password`, `--login-only`; subcommands `setup` (TUI setup flow), `accounts`,
  `login <url> <user> <pass>`.
- Shutdown order (port from `mpv_shim.py` finally-block): stop accepting commands →
  `Sessions/Playing/Stopped` if playing → `stop` mpv (3 s grace) → kill mpv → close
  WS → close client. SIGINT/SIGTERM + Windows console ctrl-c via `os/signal`.

---

## 6. Milestones

**M0 — skeleton & login (≈0.5–1 d)**
Module, config load/save, cred store, REST client, `AuthenticateByName`,
`mpv-shim login` CLI.
✅ `login` against a live v12 docker prints the authenticated user; cred persisted;
re-run reuses it.

**M1 — device on the wire (≈1–2 d)**
WS client (keepalive, dedupe, backoff, health check), capabilities POST, device
identity.
✅ Device appears in web UI (cast icon shows it, Settings → Devices lists it); survives
server restart; survives network blip (watch reconnect log); `GET /Sessions` contains
DeviceId. **First v12-protocol verification gate.**

**M2 — playback (≈2–4 d)**
`Play` event → media pipeline → URL → spawn mpv; start/progress/stop reports; queue
(PlayNext/PlayLast, auto-advance); watched marking; mpv crash restart; transcode
teardown.
✅ Cast a movie from web UI: direct-play and forced-transcode paths both play; web UI
progress advances; next episode auto-plays; killing mpv → restarts with state restored;
watched updates in the library.

**M3 — remote control + OSD menu (≈2–3 d)**
`Playstate` (pause/seek/next/prev/stop), `GeneralCommand` (volume/mute/track switch),
pause/seek echo guards, intro skip, OSD menu (R6) with audio/sub switching incl.
external subs and transcode refetch.
✅ Full remote panel from web UI works (volume slider, tracks, seek bar, next/prev,
play/pause/stop); `c` opens the menu; navigate by keyboard and by remote; switching
audio/sub track updates mpv and the web UI's track UI; external subtitle switch works;
transcode track switch refetches and reloads without leaking ActiveEncodings.

**M4 — TUI + systray (≈1–2 d)**
Bubble Tea setup flow (password + Quick Connect), live status screen, account CRUD,
systray with account menu + status + quit, `--headless` mode, first-run auto-setup.
✅ Fresh machine: run binary → TUI setup → Quick Connect code → enter in browser →
device appears → cast works; status screen shows live playback; tray reflects state
and can disconnect/remove accounts; `--headless` parity.

**M5 — hardening & release (≈2–3 d)**
Reconnect/crash matrix (server restart while playing, WS drop mid-seek, mpv OOM,
simultaneous remote commands), idle-stop, config docs, README, go build for
linux/amd64+arm64, darwin, windows. v11 docker smoke test alongside v12.
✅ Checklist in §7 green on v12 and v11.

Total: **~1.5–2.5 weeks** part-time for one developer (M2 is the long pole).

## 7. Testing strategy

No upstream tests exist to port; validation is three-layered:

1. **Unit (stdlib `testing`):** DeviceProfile golden JSON; media-source pick + URL
   building (all 3 cases + fallback); track index mapping (embedded/external/encode
   tables); queue transitions; menu state machine transitions; WS MessageId dedupe.
2. **Integration (fake server):** `net/http/httptest` + a websocket endpoint in Go
   that fakes `AuthenticateByName`, `PlaybackInfo`, `/socket` (inject Play/Playstate/
   GeneralCommand), `/Sessions/*`. Drives the full play→progress→stop cycle with a
   stubbed mpv (IPC socket pair; fake `end-file`/`property-change` events). This is the
   regression net for the state machine.
3. **Manual matrix (docker jellyfin v11 + v12 + demo media + real mpv):**
   | # | Case | M |
   |---|---|---|
   | 1 | Cast from web (direct) | 2 |
   | 2 | Cast forced transcode (tiny bitrate) | 2 |
   | 3 | Next/prev episode, auto-advance | 2 |
   | 4 | Progress/watched in web UI | 2 |
   | 5 | kill -9 mpv mid-play | 2 |
   | 6 | Remote: volume, mute, seek, pause, play/pause | 3 |
   | 7 | Remote track switch (direct + transcode + external sub) | 3 |
   | 8 | OSD menu: open/nav/switch/close, keyboard + remote | 3 |
   | 9 | Intro skip (plugin + demo content) | 3 |
   | 10 | Server restart mid-play → reconnect, state intact | 5 |
   | 11 | WS drop (firewall test) → backoff reconnect | 5 |
   | 12 | TUI: fresh login (password + Quick Connect), remove account; tray menu actions | 4 |
   | 13 | Idle stop + idle command hooks | 5 |
   | 14 | v11 docker smoke (cases 1–4) | 5 |

## 8. Risks & mitigations

| Risk | Mitigation |
|---|---|
| v12 surface differences beyond auth (device discovery quirks) | M1 gate = "visible & controllable in real v12 web UI"; mirror v3 shim's capabilities/identity exactly (it's the reference v12 client) |
| `input-command-hook` key-claim semantics on mpv < 0.35 | min-mpv-version note (≥ 0.35 / ≥ 1.0 recommended); lua fallback script ready |
| Linux systray needs a StatusNotifier host (KDE/XFCE have one; GNOME needs the AppIndicator extension) | tray degrades gracefully: startup log hint, TUI remains the full surface |
| macOS systray needs cgo + Xcode CLT (Cocoa) | document; optional `-tags nosystray` build for a pure-Go mac binary |
| Playback state-machine subtleties (watched, echoes, teardown) | port v2.10 guard variables explicitly (`pause_ignore`, `do_not_handle_pause`, `last_seek`); fake-server integration test per transition |
| Scope creep toward v3 features (browser, sync, music) | §1 out-of-scope list is the contract; anything else = new issue |
| Single active player limitation | same as upstream; document |

## 9. Build & release

- `go build .` → one static binary, zero system packages: Windows native tray,
  Linux tray on StatusNotifier-host desktops, macOS tray via Cocoa cgo (no external
  packages).
- No installer in v0.1: tarball + GitHub release notes; config dir auto-created
  (`~/.config/mpv-shim/` / `%appdata%\mpv-shim\` / `~/Library/Application Support/mpv-shim/`).
- Version stamped via `-ldflags -X main.version=`.
