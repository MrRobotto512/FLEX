# FLEX PS5 client

Native homebrew player for a jailbroken PS5. C, SDL2 (video out, audio, controller), SDL2_ttf (text), FFmpeg (demux + CPU decode). It finds the server over UDP, lists the library and plays the `ps5` profile the server prepares (≤1080p H.264 + AAC) so software decoding keeps up.

## Toolchain (once, in WSL Ubuntu or any Debian-like Linux)

```bash
bash ps5-client/tools/setup-wsl.sh
```

That installs clang-18/lld-18 and friends with apt, unpacks the **prebuilt ports** from [pacbrew-repo](https://github.com/ps5-payload-dev/pacbrew-repo) (SDL2, SDL2_ttf, freetype, FFmpeg 7.0.1 and ~80 more as static libs) and the [ps5-payload-sdk](https://github.com/ps5-payload-dev/sdk) release into `/opt/ps5-payload-sdk`, exports `PS5_PAYLOAD_SDK` in `~/.bashrc`, and copies the DejaVu font into `assets/`. Nothing is compiled from source; the whole thing takes a few minutes. Versions and checksums are pinned at the top of the script.

Layout after setup: `$PS5_PAYLOAD_SDK/toolchain/prospero.mk` (defines `CC`, `PKG_CONFIG`, `PS5_DEPLOY`, `PS5_SYSROOT`), wrappers in `$PS5_PAYLOAD_SDK/bin/prospero-*`, ports under `$PS5_PAYLOAD_SDK/target/user/homebrew/{include,lib}`.

## Build, test, install

```bash
cd ps5-client
make                 # -> build/FLEX.elf (~38 MB with debug info; FFmpeg is linked statically)
make deploy          # upload ELF + icon + font to /data/homebrew/FLEX over FTP (port 2121)
make run             # deploy, then start it on the TV via websrv's /hbldr (port 8080); stdout is relayed to your shell
```

`PS5_HOST` defaults to `192.168.0.176`; override with `make run PS5_HOST=…`.

**websrv must be running on the console.** It is the homebrew launcher from ps5-payload-dev. Only its `/hbldr` endpoint (what the launcher's tiles use) starts an ELF as a real app with the screen; payloads sent to `elfldr` (port 9021, `make run-headless`) or to websrv's `/elfldr` page run as background processes, which the launcher reports as "running in the background" with nothing on the TV.

When FLEX is started from the launcher tile, the launcher first shows its log console ("The homebrew is running in the background…") on top of the app. Press **Circle** once to close that dialog; FLEX is underneath.

websrv does not survive a console restart. The symptom is the Homebrew Launcher app opening with a "not connected to the internet" error (its web view can't reach 127.0.0.1:8080). Bring it back with:

```bash
make websrv          # tools/websrv.sh: downloads websrv once (pinned version + checksum) and sends it to elfldr
```

`make deploy` must run at least once before `make run`, so the font is in `/data/homebrew/FLEX/assets/`; without it the app exits with "font missing". After `make deploy` FLEX also appears, with its icon, in the websrv launcher page at `http://192.168.0.176:8080` (open it in the PS5 browser to start FLEX without a PC).

The FLEX server must be running on the PC (`go run ./cmd/flexd` or the desktop app). The first time it listens, Windows asks whether to allow `flexd.exe` on the network; say yes for private networks or the PS5 will report "No server found".

## Controls

| Where | Button | Action |
| --- | --- | --- |
| Grid | D-pad | move (hold to repeat) |
| Grid | L1 / R1 | switch between Movies and TV |
| Grid | Cross | play a movie / open a show |
| Grid | Triangle | re-discover the server and refresh |
| Grid | Circle | quit |
| Episodes | D-pad up/down | move; the list opens on the next unwatched episode |
| Episodes | Cross | play (resumes if partially watched) |
| Episodes | Circle | back to the shows |
| Player | Cross | pause / resume |
| Player | D-pad left / right | back / forward 10 s (hold to repeat) |
| Player | D-pad down / up | back / forward 60 s |
| Player | Square | picture shape: Original → 16:9 → 4:3 → 2.35:1 → Zoom → Stretch (remembered per movie/episode) |
| Player | Triangle | upscaling on/off: the server scales SD and 720p video to 1080p (Lanczos + light sharpening, deinterlaced) before sending it; restarts the stream where you are; remembered for everything |
| Player | L1 / R1 | volume down / up in 10 % steps, 0–200 % (hold to repeat; above 100 % boosts quiet files); remembered for everything |
| Player | Circle | stop (progress is saved) |

Seeks add up: the jump happens 0.7 s after the last press, with a progress bar showing the target. Direct-play files seek in place; a transcoded movie restarts its server session at the new time (a second or two).

The OSD's bottom line shows how playback keeps up: picture size, average decode and draw time per frame, frames dropped, decoder threads. A frame that is already late is decoded but not drawn, so a picture too heavy for the console drops frames instead of stuttering the sound; after repeated drops the player also skips H.264 deblocking ("fast decode").

Away from home the client falls back to the server's Tailscale address, which it learns while on the home network (second line of `/data/homebrew/FLEX/server.txt`).

## How it works

- `net.c`: UDP discovery (`FLEX_DISCOVER` → JSON reply) and a tiny HTTP/1.0 client with the `X-Flex-Client: ps5` header.
- `main.c`: SDL window, controller input, 6×2 poster grid (posters fetched from `/movies/{id}/poster`, decoded with SDL2_image, one per frame, cache capped at 48), saved server address at `/data/homebrew/FLEX/server.txt` (used when discovery finds nothing, e.g. over Tailscale).
- `player.c`: `avformat_open_input` on the URL the server returned (direct file or HLS), decodes with all cores, converts audio to S16 stereo for SDL, presents YUV frames in sync with the audio clock.
- `third_party/cJSON/`: vendored cJSON 1.7.18.

## Next steps

- On-screen keyboard for entering a Tailscale address (`SceImeDialog` is already linked by the SDL port).
- Separate decode thread and a frame queue if 1080p stutters.
- Seek: restart the HLS session with `start=`.
- Audio track selection (`audio=` on `/play`).
