# FLEX

A self-hosted movie server for your home PC with a native client for a jailbroken PS5, plus a browser UI for everything else. Works on the LAN out of the box and over Tailscale when you're away.

```
PS5 homebrew client ── LAN (UDP discovery) ─────────┐
PS5 homebrew client ── Tailscale via travel router ─┤
Phone / laptop (browser) ── Tailscale ──────────────┼──▶  FLEX desktop app (Windows) = server + config UI
                                                     │     • library scan, TMDB metadata, SQLite
                                                     │     • FFmpeg: direct play / remux / transcode → HLS
                                                     └     • JSON API, LAN discovery, embedded Tailscale (tsnet)
```

## Layout

| Path | What | Language |
| --- | --- | --- |
| `server/` | Library scanner, SQLite store, FFmpeg orchestration, HTTP API, UDP discovery, tsnet | Go |
| `cmd/flexd/` | Headless server binary (dev + servers without a desktop) | Go |
| `desktop/` | Windows tray app with the config window; embeds `server/` | Go + Wails |
| `web/` | Browser client, also used as the Wails frontend | HTML/JS |
| `ps5-client/` | Native PS5 homebrew player (ps5-payload-sdk, SDL2, FFmpeg) | C |
| `docs/api.md` | The JSON API contract every client follows | |

## Quick start (server)

Requirements: Go 1.23+, FFmpeg (`ffmpeg` and `ffprobe` on `PATH`), a free [TMDB API key](https://www.themoviedb.org/settings/api).

```bash
cp .env.example .env            # add your TMDB key and library folders
go run ./cmd/flexd
```

Then open `http://localhost:8484` in a browser. The PS5 client finds the server by itself on the same network.

## PS5 client

Built with the [ps5-payload-sdk](https://github.com/ps5-payload-dev/sdk) toolchain under WSL or Docker; see [ps5-client/README.md](ps5-client/README.md). During development the ELF is pushed straight to `elfldr` on the console; for day-to-day use it's installed under `/data/homebrew/FLEX/` and launched from the homebrew launcher.

## Remote access

The server can join your Tailscale network as its own node (`tsnet`), so phones and laptops running Tailscale reach it from anywhere with no port forwarding. The PS5 can't run Tailscale; away from home it needs a travel router that is on your tailnet. See [docs/remote.md](docs/remote.md).

## Status

Scaffold. See the build order in [docs/roadmap.md](docs/roadmap.md).
