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
make release         # build/FLEX-ps5.elf without debug info + FLEX-ps5.zip (what a GitHub release carries)
```

The version shown on the grid comes from `git describe` (`VERSION=0.3.0 make` overrides it). Pushing a tag `v*` runs `.github/workflows/release.yml`, which builds the PS5 client, `FLEX.exe` and `flexd.exe` and publishes them; FLEX servers download the PS5 build from there and consoles update from their server (Options on the grid).

To see how the player keeps up, add `&env=FLEX_PLAYER_TRACE%3D1` to the `/hbldr` URL (websrv passes it on as an environment variable): the player then prints its clock, buffer and frame queue once a second.

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
| Grid | L1 / R1 | previous / next tab: Continue → Movies → TV → Search (Continue only when there is something to continue; FLEX opens on it) |
| Grid | Cross | play a movie / open a show; on Continue, resume the movie or play the next episode |
| Grid | Square | search |
| Grid | Triangle | re-discover the server and refresh |
| Grid | Options | install the update the server offers ("Update … ready" in the top line) |
| Grid | Circle | quit |
| Search | D-pad / Cross | move on the keyboard / type (Space, Delete, Clear on the bottom row); results update as you type |
| Search | Square | delete the last letter |
| Search | Down from the keyboard / Up from the results | move between the keyboard and the results |
| Search | Circle | results → keyboard → Movies |
| Episodes | D-pad up/down | move; the list opens on the next unwatched episode |
| Episodes | Cross | play (resumes if partially watched) |
| Episodes | Circle | back to the shows |
| Player | Cross | pause / resume |
| Player | D-pad left / right | back / forward 10 s (hold to repeat) |
| Player | D-pad down / up | back / forward 60 s |
| Player | Square | picture shape: Original → 16:9 → 4:3 → 2.35:1 → Zoom → Stretch (remembered per movie/episode) |
| Player | Triangle | upscaling on/off: the server scales SD and 720p video to 1080p (Lanczos + light sharpening, deinterlaced) before sending it; restarts the stream where you are; remembered for everything |
| Player | L1 / R1 | volume down / up in 10 % steps, 0–200 % (hold to repeat; above 100 % boosts quiet files); remembered for everything |
| Player | Options | picture panel: Up/Down picks Saturation (0–200 %), Brightness (−30…+30) or Contrast (50–150 %), Left/Right changes it (hold to repeat), Square resets it, Options or Circle closes; applied in the decoder's colour conversion, so it costs nothing; remembered for everything (`picture.txt`) |
| Player | Circle | stop (progress is saved) |

Seeks add up: the jump happens 0.7 s after the last press, with a progress bar showing the target. Direct-play files seek in place; a transcoded movie restarts its server session at the new time (a second or two).

The OSD's bottom line shows how playback keeps up: picture size (`3840x2160 → 1920x1080` when it is scaled), how full the decoded-frame queue is (8/8: the decoder is ahead; near 0: it can't keep up), draw time per frame, seconds read ahead, frames dropped, decoder threads. A frame that is already late is decoded but not drawn, so a picture too heavy for the console drops frames instead of stuttering the sound; after repeated drops the player also skips H.264 deblocking ("fast decode"). When the network can't keep up and the read-ahead runs dry, playback pauses ("Buffering…") until 3 s are buffered again, instead of dropping frames.

### Picture quality and 4K

The console's video output through this SDL port is 1920×1080 only, and decoding is on the CPU (16 threads; the SDK has no hardware decoder). So 4K files are decoded in full and scaled down to 1080p with Lanczos on the decode threads, which looks sharper than a 1080p file; SD and 720p are scaled up the same way, and the main thread only copies finished pictures. 4K HEVC 10-bit decodes in real time. The client tells the server what it decodes (`caps=`) and how fast its link measured (`max_kbps=`, kept in `link.txt`): files within that play as they are, everything else, including all HDR (the server tone maps it), is converted at the highest bitrate the link carries (see docs/api.md).

`FLEX_PLAY_PROFILE=direct` (through websrv's `env=`) plays every file as it is, for measuring the decoder.

Away from home the client falls back to the server's Tailscale address, which it learns while on the home network (second line of `/data/homebrew/FLEX/server.txt`).

## How it works

- `net.c`: UDP discovery (`FLEX_DISCOVER` → JSON reply) and a tiny HTTP/1.0 client with the `X-Flex-Client: ps5` header.
- `main.c`: SDL window, controller input, 6×2 poster grid, saved server address at `/data/homebrew/FLEX/server.txt` (used when discovery finds nothing, e.g. over Tailscale).
- `posters.c`: four worker threads fetch posters (`/movies/{id}/poster`) and decode them with SDL2_image, so the grid never waits on the network; the rows on screen are asked for last and served first. Every poster is kept in `/data/homebrew/FLEX/cache/` (named kind, id and TMDB id, so a fixed match gets its new poster), and after the library loads the workers quietly fill that cache with the whole library (~65 KB a poster). At most 48 textures stay in memory.
- `player.c`: four threads. A reader demuxes the URL the server returned (direct file or HLS) up to 30 s / 64 MB ahead, so slow links and segment requests don't stall decoding; a video thread decodes with all cores into a queue of 8 frames; an audio thread converts to S16 stereo for SDL; the main thread presents YUV frames in sync with the audio clock. After a seek the audio waits for the first video frame, so the frames between the keyframe and the target aren't counted late.
- Updates: on connect the client asks `/api/v1/client/ps5`; Options downloads it to `eboot.elf.new`, checks size and SHA-256, keeps the old one as `eboot.elf.bak` and asks websrv (`127.0.0.1:8080/hbldr`) to start the new one, which also ends the running copy.
- `third_party/cJSON/`: vendored cJSON 1.7.18.

## Next steps

- On-screen keyboard for entering a Tailscale address (`SceImeDialog` is already linked by the SDL port).
- Seek: restart the HLS session with `start=`.
- Audio track selection (`audio=` on `/play`).
