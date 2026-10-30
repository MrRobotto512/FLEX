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
{"name":"Ryan's PC","id":"c0ffee…","version":"0.2.0","features":["direct","hls","tv","seek","tsnet"],"remote":false,
 "tailscale":{"state":"Running","dns":"flex.tail1234.ts.net","ip":"100.101.102.103"}}
```
`remote` is true when the request arrives over the tailnet (source in 100.64.0.0/10); `/play` then uses the `remote` profile even when a client asks for `ps5`. `tailscale` is present while the server is on the tailnet; clients remember `ip` for use away from home. The discovery reply carries the same address as `ts_ip`.

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

## TV shows

Episodes are recognised from `S01E02` / `1x02` in the file name (double episodes `S01E01E02` count as the first). The series name comes from the text before it, or from the folder above a `Season 01` folder.

`GET /shows?q=<text>`
```json
[{"id":3,"title":"Bob's Burgers","year":2011,"overview":"…","poster":"/api/v1/shows/3/poster",
  "backdrop":"/api/v1/shows/3/backdrop","seasons":1,"episodes":22,"unwatched":20,"added_at":"…"}]
```

`GET /shows/{id}` → the list item plus every episode grouped by season and the next one to watch (first not marked watched):
```json
{"…":"…","season_list":[{"season":9,"episodes":[
   {"id":41,"show_id":3,"season":9,"episode":1,"title":"Just One of the Boyz 4 Now for Now","overview":"…",
    "runtime_s":1320,"still":"/api/v1/episodes/41/still","resume_s":0,"watched":true,"added_at":"…"}]}],
 "next_episode":{"id":42,"season":9,"episode":2,"…":"…"}}
```

`GET /shows/{id}/poster`, `GET /shows/{id}/backdrop` → JPEG.

`GET /episodes/{id}` → the episode plus `show_title` and `media` (same shape as a movie). `GET /episodes/{id}/still` → JPEG.

Playback and progress work exactly like movies with the `/episodes/{id}` prefix: `GET /episodes/{id}/play`, `GET /episodes/{id}/file`, `PUT /episodes/{id}/progress`.

## Playback

`GET /movies/{id}/play?profile=direct|ps5|remote&audio=<index>&sub=<index>&start=<seconds>&upscale=0|1` (or `/episodes/{id}/play`)

`upscale=1` scales video below ~1000 lines up to the profile's height on the server (Lanczos, light unsharp, NVENC/x264); the answer says `"upscaled":true` and is always `hls`. Every transcode deinterlaces frames flagged interlaced.

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
- `GET /transcodes` → `[{"key","file","start_s","mode":"remux|transcode|upscale","started","last_used"}]` — running sessions. ffmpeg converts the first 60 s at full speed, then at 1.5× playback speed. With NVENC the whole chain runs on the GPU (falls back to CPU filters if the GPU can't decode the file). Encoder order: NVENC, QuickSync, AMF, then `h264_mf` (Media Foundation: the GPU driver's own encoder, for cards whose driver is too old for ffmpeg's NVENC), then libx264; a job whose hardware encoder fails is retried with libx264. `GET /server` on the home network adds `gpus` and `encoder_tests` (each hardware encoder → `ok` or ffmpeg's reason).

### Profiles

| Profile | Video | Audio | Subtitles | Why |
| --- | --- | --- | --- | --- |
| `direct` | as-is | as-is | as-is | browsers/desktop that can decode anything |
| `ps5` | H.264 High@4.1, ≤1920×1080, ≤12 Mbps, 8-bit SDR (HDR tonemapped) | AAC 2.0 48 kHz | burned in when selected | PS5 client decodes on the CPU; this is what it handles comfortably |
| `remote` | H.264, ≤1080p, ≤6 Mbps (720p/3 Mbps when the server is told the link is slow) | AAC 2.0 | burned in | sized to home upload bandwidth |

If the source is already within a profile's limits the server remuxes (`-c:v copy`) instead of re-encoding; if the container is also fine it answers `direct`. A `direct` answer ignores `start`: the client seeks in the file itself. An `hls` session begins at `start` and cannot seek; clients restart it with a new `start`.

## Progress

`PUT /movies/{id}/progress` body `{"position_s":1830,"duration_s":10210}` → `204`. Positions within the last 5 % mark the movie watched; positions under 30 s are stored as 0 (no resume point). Clients send this every ~10 s during playback and on stop.

## Library

- `POST /library/scan` → `202` starts a scan (no-op if one is running).
- `GET /library/status` → `{"scanning":true,"files_seen":812,"movies":790,"shows":12,"episodes":240,"last_scan":"…"}`
- `GET /library/folders`, `PUT /library/folders` body `["D:\\Movies","E:\\More"]` — desktop UI only.
- `POST /movies/{id}/match` body `{"tmdb_id":949}` — fix a wrong metadata match.
- `GET /library/unmatched` → `{"movies":[{"id","title","year","path"}],"shows":[{"id","title","year","path","episodes"}]}` — what TMDB could not identify, with file paths. Home network only.

## Client updates
The server checks the latest GitHub release every 6 h (unless `FLEX_NO_UPDATES=1`) and keeps its PS5 build, verified against the release's `SHA256SUMS`, in `<data>/ps5/client/`.

- `GET /client/ps5` → `{"version":"0.3.0","sha256":"…","size":35927528,"url":"/api/v1/client/ps5/eboot.elf"}`, or `404 no_build` before the first download.
- `GET /client/ps5/eboot.elf` — the build itself (`Range` works).

The PS5 client offers it when `version` is newer than its own (any release is newer than a dev build) and checks `size` and `sha256` before swapping it in.

## Auth

- LAN: open by default; optional 4-digit PIN (`X-Flex-Pin`) once the desktop UI exposes it.
- Tailnet: the server resolves the Tailscale identity of each request (tsnet WhoIs). Only devices on your tailnet can reach it at all, so no extra credentials.

## Errors

`{"error":"not_found","message":"movie 42 does not exist"}` with the matching HTTP status. Codes: `not_found`, `bad_request`, `ffmpeg_failed`, `scan_running`.
