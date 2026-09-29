# jellyfin-mpv-shim v2.10 → minimal Go rewrite: scope research

Source analyzed: `jellyfin/jellyfin-mpv-shim` at tag **v2.10.0** (the last v2 release;
v3.0.0 is a separate rewrite with Library Browser / Offline Sync / Jellyfin v12 support and
is out of scope per request).

## 1. Headline finding: there is no HTTP server to rewrite

A first read of the project makes it look like you'd need to reimplement the "cast
endpoint" the web page POSTs to. **That doesn't exist in v2.10.** The shim is a pure
*client* of the Jellyfin server:

- It logs in as a device (`POST /Users/AuthenticateByName`) and persists
  `{Server, DeviceId, AccessToken, ...}` to `cred.json`.
- It keeps a **WebSocket connection to `{server}/socket`** open. All cast commands
  (play, pause, seek, volume, tracks) arrive as WS messages from the server.
- It reports playback state back via plain **REST** (`/Sessions/Playing*`).
- It spawns mpv and talks to it over **mpv's JSON IPC socket**.

So the Go binary binds **no port at all**. The "cast surface" is: be a well-behaved
Jellyfin device (WS client + REST) + drive a local mpv process. That makes the Go port
smaller than the headline 7.5k LOC suggests.

Verified against the actual wire format:
- `jellyfin-apiclient-python` 1.19 (the shim's client library, 4.2k LOC; only a small
  slice is used) and the Jellyfin **server** source (`MediaBrowser.Model/Session/SessionMessageType.cs`).

### WebSocket protocol (server ↔ shim)

| Item | Value |
|---|---|
| URL | `{server}/socket` (`wss:` for https) |
| Auth | HTTP header `Authorization: MediaBrowser Client="<name>", Device="<device>", DeviceId="<uuid>", Version="<ver>", Token="<access-token>"` |
| Envelope | `{"MessageType": "...", "Data": {...}, "MessageId": "..."}` (dedupe by MessageId) |
| Keepalive | Server sends `ForceKeepAlive` → shim sends `{"MessageType":"KeepAlive"}` on the given interval |
| Server→client types used | `Play`, `Playstate`, `GeneralCommand` (full enum also has `SyncPlayCommand`, `SyncPlayGroupUpdate`, `UserDataChanged`, `Sessions`, `RestartRequired`, `ServerShuttingDown`, …) |
| Reconnect | Exponential backoff 1→100 s (capped), plus periodic health check `GET /Sessions` (is our DeviceId still listed?) and retry of half-connected clients |

### REST endpoints (client → server)

| Purpose | Endpoint |
|---|---|
| Login | `POST /Users/AuthenticateByName` |
| Session health / capabilities | `GET /Sessions`, `POST /Sessions/Capabilities/Full` (sent on (re)connect; static JSON: media types, supported commands) |
| Item fetch | `GET /Users/{userId}/Items/{id}` (+ `Fields=Trickplay` for trickplay) |
| **Playback decision** | `POST /Items/{id}/PlaybackInfo` with a `DeviceProfile` JSON body → returns `MediaSources[]` with `SupportsDirectPlay`, `SupportsTranscoding`, `TranscodingUrl`, `MediaStreams[]` |
| Start | `POST /Sessions/Playing` (`PlaybackStartInfo`) |
| Progress (5 s tick) | `POST /Sessions/Playing/Progress` |
| Stop | `POST /Sessions/Playing/Stopped` (`PlaybackStopInfo`, carries `LiveStreamId`/transcode id for teardown) |
| Mark watched | `POST /Users/{userId}/PlayedItems/{itemId}` |
| Intro/outro skip | `GET /MediaSegments/{id}?includeSegmentTypes=...` (skip-intro plugin) |
| Transcode teardown | `DELETE /Videos/ActiveEncodings?DeviceId=..&PlaySessionId=..` |
| Live stream teardown | `POST /LiveStreams/Close` |

### Playback URL construction (`media.py`)

Given an `ItemId`: fetch item → `POST PlaybackInfo` (with device profile) → pick media
source (highest `SupportsDirectPlay*50000 + Bitrate/1000` weight; honor requested
`MediaSourceId`) → URL is one of:

1. local file path (direct, server is local and file exists),
2. `{server}/Videos/{id}/stream?static=true&MediaSourceId=..&api_key=..` (direct stream;
   `+LiveStreamId` for live TV),
3. `{server}{TranscodingUrl}` (HLS transcode — profile says: video → `ts`/hls,
   audio → hls, plus subtitle profiles srt/ass/ssa/smi external+embedded, optional
   CodecProfiles for 10-bit/DV/HDR/HEVC constraints).

Track mapping: mpv's track indices are mapped to Jellyfin's `MediaStreams` indices via
`audio_seq`/`subtitle_seq` dicts (embedded vs external vs encoded subtitles handled
differently; external subtitle URLs come pre-signed in `DeliveryUrl`).

### mpv control (the other side of the socket)

Default backend in v2.10: in-process **libmpv** (python-mpv); fallback / macOS default:
**external mpv over JSON IPC** (`python-mpv-jsonipc`, i.e. spawn `mpv --input-ipc-server`).
For the Go port the external model is the only sensible one and it's all stdlib:

- Spawn: `mpv --input-ipc-server=<unix socket or 127.0.0.1:port> [--config-dir=...]`;
  shim creates its own config dir (`~/.config/jellyfin-mpv-shim/` with `mpv.conf`,
  `input.conf`) unless told to use the user's; retry loop on start (10×3 s default).
- JSON IPC: one JSON object per line; correlate responses by `request`/`request_id`/`error`;
  events: `file-loaded`, `end-file`, `shutdown`, `property-change`, `idle`, `log-message`.
- Ops used (all trivial IPC): `loadfile`, `stop`, `seek`, `set_property`
  (`pause`, `volume`, `mute`, `audio-stream`, `sub-stream`), `get_property`
  (`time-pos`, `duration`, `pause`, `seek-percentage`, ...), `script-message` (menu only),
  key bindings via `input_default_bindings`/`input_media_keys`.

## 2. What v2.10 actually contains (7,471 LOC Python, no test suite)

| Module | LOC | Needed for minimal cast surface? |
|---|---|---|
| `player.py` (mpv wrapper, queue, state, keybindings, watched, intro skip, timeline) | 1427 | **core** — but ~40 % of it is hooks for menu/syncplay/trickplay/discord; the core state machine is ~800–900 |
| `syncplay.py` (timed co-play, requires server-side SyncPlay license) | 643 | **cut** |
| `media.py` (items, PlaybackInfo, URL building, intros, chapters) | 611 | **core** |
| `menu.py` (OSD-drawn navigation menu: tracks, transcode override, shader packs…) | 577 | **cut** (mpv's own OSC/shortcuts remain) |
| `clients.py` (login, WS lifecycle, health check, multi-server) | 531 | **core** (single-server subset) |
| `gui_mgr.py` (pywebview + systray GUI) | 508 | **cut** |
| `utils.py` (device-profile JSON, helpers) | 369 | **core** (~150: profile builder is a static dict) |
| `display_mirror.py` (chromecast-like preview window) | 398 | **cut** |
| `bulk_subtitle.py` (season-wide subtitle reconfig) | 280 | **cut** |
| `conf.py` (settings, ~130 keys) | 228 | **subset** (~15–25 keys) |
| `language_config.py` (mpv-style alang/slang rules) | 224 | **cut** (v1: honor server default + cast-time overrides only) |
| `video_profile.py` (shader packs) | 204 | **cut** |
| `event_handler.py` (WS event → player dispatch) | 179 | **core** |
| `svp_integration.py` | 181 | **cut** |
| `trickplay.py` + 3× `.lua` | 148 | **cut** |
| `mpv_shim.py` (wiring/main) | 134 | **core** |
| `i18n`, `rich_presence`, `update_check`, `win_utils`, `action_thread`, `timeline`, `bifdecode`, `args`, `conffile`, `constants`, `log_utils` | ~600 | mostly **cut**; `timeline` (68) + logging are core |

**Cuts ≈ 3.4k LOC (≈45 % of the codebase). Core to reimplement ≈ 2.9–3.2k LOC Python.**

Note on `timeline.py`: the "timeline" is not WS — it's the 5-second
`POST /Sessions/Playing/Progress` loop plus idle handling; playback *start/stop*
notifications are REST too.

## 3. Complexity assessment

### Straightforward (mechanical, well-specified)
- Auth/login + credential persistence.
- WS client: header auth, envelope, keepalive, MessageId dedupe, backoff reconnect,
  health check. One small dependency (`nhooyr.io/websocket` or `gorilla/websocket`);
  stdlib for everything else.
- REST client: ~10 endpoints, JSON in/out.
- PlaybackInfo → URL building (string + struct work).
- DeviceProfile: copy the static JSON dict from `utils.py` verbatim.
- mpv subprocess + JSON IPC: stdlib `os/exec`, line-delimited JSON, request-id
  correlation, event dispatch. mpv's IPC is stable and documented.
- Config: one JSON file, ~20 typed fields.

### Genuinely fiddly (where upstream's years of bug fixes live)
1. **Playback lifecycle state machine** (`player.py`): ordering of
   start/progress/stop reports vs. mpv `end-file`/user-close; auto-advance queue
   (PlayNext/PlayLast); watched-marking threshold; intro/outro skip windows;
   transcode session teardown on stop/switch; "mpv died mid-playback" recovery.
   Upstream guards against feedback loops explicitly (`pause_ignore`, `do_not_handle_pause`,
   `last_seek`) — port those patterns, don't rediscover them.
2. **Track index mapping** (mpv index ↔ Jellyfin stream index, external vs embedded
   subtitles, `SetAudioStreamIndex`/`SetSubtitleStreamIndex` from the web UI).
3. **Connection edge cases**: half-connected clients (upstream retries 3×),
   WS drop while playing, server restart mid-session, health-check-driven reconnect.
4. **Direct vs transcode fallback**: "if selected source is unplayable, try the
   others" loop in `get_playback_url`.

### Estimated Go budget (minimal scope)

| File (suggested layout) | ~LOC |
|---|---|
| `main.go` (flags, wiring, graceful shutdown, logging) | 200 |
| `config.go` (settings struct + load/save + defaults) | 150 |
| `cred.go` (login, credential store) | 120 |
| `jfin/client.go` (REST: auth, sessions, items, playback, teardown) | 300 |
| `jfin/ws.go` (socket, keepalive, reconnect, dispatch) | 300 |
| `jfin/media.go` (PlaybackInfo, media source pick, URL build, profile, intros) | 400 |
| `player/mpv.go` (subprocess, JSON IPC, events, ops, restart) | 550 |
| `player/queue.go` (playlist, next/prev, watched, timeline loop) | 350 |
| **Total** | **~2.4k** |

Single binary, **one dependency** (websocket lib), stdlib for the rest.

## 4. Recommended minimal scope

**Keep:** single-account login + persistence; WS client (keepalive/reconnect/health);
`Play` (PlayNow/PlayNext/PlayLast), `Playstate` (pause/play/seek/next/prev/stop),
`GeneralCommand` (volume/mute/track switch); direct play + transcode + fallback;
timeline start/progress/stop; watched marking; queue; intro skip (settings-gated);
local/remote bitrate profiles; mpv crash restart; config file subset
(server, player name, mpv path, config dir, local/remote kbps, force h265, intro skip,
idle behavior, log level).

**Cut (add later if wanted):** SyncPlay, OSD menu, GUI/systray, display mirror,
trickplay thumbnails, shader packs/SVP, bulk subtitles, language_config, Discord
presence, i18n, update check, multi-server, mDNS discovery.

## 5. Risks / notes

- **Server version**: v2.10 targets the 10.8–11.x protocol; v3.0.0 added Jellyfin v12
  support. The `/socket` + `MediaBrowser` header protocol has been stable for years, but
  test against the server version you'll actually run.
- **No upstream tests** to port — validate against a live Jellyfin (docker + demo
  content), exercising: cast from web, mobile-style remote (volume/tracks), pause/seek,
  next-episode, stop, reconnect (restart server mid-play), transcode path (force small
  bitrate), intro skip.
- **Credentials format** is internal to the Python app; the Go binary can start fresh
  (one login flow, like upstream's CLI: URL → user → password).
- macOS default is external mpv (`mpv_ext=true` because libmpv is unreliable there) —
  the Go port's subprocess model matches that path exactly.
- Subtitle URLs: external ones arrive pre-built (`DeliveryUrl`); embedded ones ride
  inside the container; only `Encode`-method subtitles need `api_key` handling via the
  normal auth header.

## 6. Bottom line

- Scope: **medium-small**. ~2.4–3k LOC Go, 1 dependency, no server port, no auth UI.
- The protocol is fully specified by this report + the v2.10 source as a reference
  implementation; the real work is the ~800-line playback state machine and its edge
  cases, not networking.
- Rough effort (one developer, part-time, using upstream as spec): **~1–2 weeks** to a
  cast surface that covers play/pause/seek/queue/volume/tracks/progress/transcode;
  first milestone (appears as device in web UI, plays one item) is ~2–3 days.
