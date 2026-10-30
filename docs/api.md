# FLEX API v1

Every client (PS5, browser, desktop UI) talks to the server through this contract. Keep it boring: plain HTTP, JSON, no cookies.

Base URL: `http://<host>:8484/api/v1`. Defaults: HTTP on **8484**, UDP discovery on **7555**.

Clients send `X-Flex-Client: ps5 | web | desktop` on every request so the server can pick defaults (e.g. the `ps5` transcode profile).

## Discovery (LAN only)

The client broadcasts the UDP datagram `FLEX_DISCOVER 1` to `255.255.255.255:7555`. Every server on the subnet answers from its own address with one JSON datagram:

```json
{"name":"Ryan's PC","id":"c0ffee…","port":8484,"version":"0.1.0"}
```

Broadcasts don't cross Tailscale. Clients keep a saved address (tailnet hostname or IP) and fall back to it when no server answers within ~1 s.

## Server

`GET /server`
```json
{"name":"Ryan's PC","id":"c0ffee…","version":"0.1.0","features":["hls","direct","tsnet"],"remote":false}
```
`remote` is true when the server sees the request arriving over the tailnet (source in 100.64.0.0/10); clients should then default to the `remote` profile.

## Movies

`GET /movies?sort=title|added|year&q=<text>`
```json
[{"id":42,"title":"Heat","year":1995,"runtime_s":10200,"overview":"…",
  "poster":"/api/v1/movies/42/poster","backdrop":"/api/v1/movies/42/backdrop",
  "resume_s":1830,"watched":false,"added_at":"2026-10-29T18:00:00Z"}]
```

`GET /movies/{id}` → the list item plus media info:
```json
{"…":"…","media":{"container":"matroska","size":14123456789,"duration_s":10210,
  "video":{"codec":"hevc","width":3840,"height":2160,"bit_depth":10,"hdr":"hdr10","fps":23.976},
  "audio":[{"index":1,"codec":"truehd","channels":8,"lang":"eng","default":true}],
  "subtitles":[{"index":3,"codec":"subrip","lang":"eng","forced":false}]}}
```

`GET /movies/{id}/poster`, `GET /movies/{id}/backdrop` → JPEG, cacheable.

## Playback

`GET /movies/{id}/play?profile=direct|ps5|remote&audio=<index>&sub=<index>&start=<seconds>`

The server decides how to deliver the file for that profile and returns:
```json
{"method":"hls","url":"/api/v1/hls/7f3a/index.m3u8","duration_s":10210,
 "video_codec":"h264","audio_codec":"aac","width":1920,"height":1080}
```
or, when the file already fits the profile:
```json
{"method":"direct","url":"/api/v1/movies/42/file","duration_s":10210,"container":"mp4", "…":"…"}
```

- `GET /movies/{id}/file` — the original file, `Accept-Ranges: bytes`, honours `Range`.
- `GET /hls/{session}/index.m3u8`, `GET /hls/{session}/seg{n}.ts` — FFmpeg output for one transcode session. 4 s MPEG-TS segments. Sessions are keyed by movie + profile + tracks + start; idle sessions are reaped after 5 min.
- `DELETE /hls/{session}` — client is done; stop FFmpeg now.

### Profiles

| Profile | Video | Audio | Subtitles | Why |
| --- | --- | --- | --- | --- |
| `direct` | as-is | as-is | as-is | browsers/desktop that can decode anything |
| `ps5` | H.264 High@4.1, ≤1920×1080, ≤12 Mbps, 8-bit SDR (HDR tonemapped) | AAC 2.0 48 kHz | burned in when selected | PS5 client decodes on the CPU; this is what it handles comfortably |
| `remote` | H.264, ≤1080p, ≤6 Mbps (720p/3 Mbps when the server is told the link is slow) | AAC 2.0 | burned in | sized to home upload bandwidth |

If the source is already within a profile's limits the server remuxes (`-c:v copy`) instead of re-encoding; if the container is also fine it answers `direct`.

## Progress

`PUT /movies/{id}/progress` body `{"position_s":1830,"duration_s":10210}` → `204`. Positions within the last 5 % mark the movie watched. Clients send this every ~10 s during playback and on stop.

## Library

- `POST /library/scan` → `202` starts a scan (no-op if one is running).
- `GET /library/status` → `{"scanning":true,"files_seen":812,"movies":790,"last_scan":"…"}`
- `GET /library/folders`, `PUT /library/folders` body `["D:\\Movies","E:\\More"]` — desktop UI only.
- `POST /movies/{id}/match` body `{"tmdb_id":949}` — fix a wrong metadata match.

## Auth

- LAN: open by default; optional 4-digit PIN (`X-Flex-Pin`) once the desktop UI exposes it.
- Tailnet: the server resolves the Tailscale identity of each request (tsnet WhoIs). Only devices on your tailnet can reach it at all, so no extra credentials.

## Errors

`{"error":"not_found","message":"movie 42 does not exist"}` with the matching HTTP status. Codes: `not_found`, `bad_request`, `ffmpeg_failed`, `scan_running`.
