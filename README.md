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
mpv-shim login http://localhost:8096 admin mypassword   # log in, persist credentials
mpv-shim                                               # status (reuses saved credentials)
mpv-shim accounts                                     # list saved accounts
mpv-shim accounts rm 0                                # remove account by index
```

Status: M0 (skeleton + login) done. M1–M5 in [PLAN.md](PLAN.md) §6.
