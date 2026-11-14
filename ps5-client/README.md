# FLEX PS5 client

Native homebrew player for a jailbroken PS5. C, SDL2 (video out, audio, controller), SDL2_ttf (text), FFmpeg (demux + CPU decode). It finds the server over UDP, lists the library and plays the `ps5` profile the server prepares (≤1080p H.264 + AAC) so software decoding keeps up.

The same sources build two ways: a payload (`build/FLEX.elf`) that websrv's Homebrew Launcher starts, and a PS5 app (`build/app/PPSA84840/`, see [As its own app](#as-its-own-app)) whose home-screen tile is FLEX itself.

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

**Black box.** Every run writes `/data/homebrew/FLEX/blackbox.txt` (the two runs before it are `blackbox.1.txt` and `blackbox.2.txt`): start-up, every play and stop, and once a second while playing (every 5 s on the grid) where each frame's time goes (upload, render, present, and how much of it was CPU work), the decoder and RGB times, the read-ahead, this app's CPU use and page faults, the console's CPU use, malloc's figures and how much memory the system will still give the app. Lines are written as they happen and synced every second, so after a freeze and a restart the end of `blackbox.1.txt` shows what led up to it; read it over FTP.

**Self-test.** A `bench.txt` in the app folder (removed once read) or `FLEX_BENCH` in websrv's `env=`, e.g. `movies/56 start=600 secs=30 runs=new,old; movies/60 start=600`, makes FLEX play each title for `secs` once per run, log the figures to the black box and quit. Runs are settings joined by `+`: `new` (as shipped), `old` (FFmpeg's automatic decoder threads and the full Lanczos scaler at every size), `t<N>` (N decoder threads), `buf<N>` (N MB read-ahead), `lanczos`. Nothing is saved (no progress, no settings).

Only one FLEX runs at a time: a second copy waits 10 s for the first to end (as during an update) and otherwise quits before touching the screen.

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
| Grid | Circle (hold) | close FLEX: a toast fills up while it's held, letting go early cancels |
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

The OSD's bottom line shows how playback keeps up: picture size (`3840x2160 → 1920x1080` when it is scaled), how full the decoded-frame queue is (8/8: the decoder is ahead; near 0: it can't keep up), draw time per frame, seconds read ahead, frames dropped, decoder threads. A frame that is already late is decoded but not drawn, so a picture too heavy for the console drops frames instead of stuttering the sound; after repeated drops the player also skips H.264 deblocking ("fast decode", a slightly blockier picture) and turns it back on after 30 s without a drop. When the network can't keep up and the read-ahead runs dry, playback pauses ("Buffering…") until 3 s are buffered again, instead of dropping frames.

### Picture quality and 4K

The console's video output through this SDL port is 1920×1080 only, and decoding is on the CPU (the SDK has no hardware decoder): 3 threads up to 720p, 4 up to 1080p, 8 above, leaving the rest of the console's 16 to the system. So 4K files are decoded in full and scaled down to 1080p with Lanczos on the video thread, which looks sharper than a 1080p file; SD and 720p are scaled up the same way; a picture already the screen's size only goes YUV to RGB; and the main thread only copies finished pictures. 4K HEVC 10-bit decodes in real time. The client tells the server what it decodes (`caps=`) and how fast its link measured (`max_kbps=`, kept in `link.txt`): files within that play as they are, everything else, including all HDR (the server tone maps it), is converted at the highest bitrate the link carries (see docs/api.md).

`FLEX_PLAY_PROFILE=direct` (through websrv's `env=`) plays every file as it is, for measuring the decoder.

Away from home the client falls back to the server's Tailscale address, which it learns while on the home network (second line of `/data/homebrew/FLEX/server.txt`).

## How it works

- `net.c`: UDP discovery (`FLEX_DISCOVER` → JSON reply) and a tiny HTTP/1.0 client with the `X-Flex-Client: ps5` header.
- `main.c`: SDL window, controller input, 6×2 poster grid, saved server address at `/data/homebrew/FLEX/server.txt` (used when discovery finds nothing, e.g. over Tailscale).
- `posters.c`: four worker threads fetch posters (`/movies/{id}/poster`) and decode them with SDL2_image, so the grid never waits on the network; the rows on screen are asked for last and served first. Every poster is kept in `/data/homebrew/FLEX/cache/` (named kind, id and TMDB id, so a fixed match gets its new poster), and after the library loads the workers quietly fill that cache with the whole library (~65 KB a poster). At most 48 textures stay in memory.
- `player.c`: four threads. A reader demuxes the URL the server returned (direct file or HLS) up to 5 min / 512 MB ahead (less if malloc's footprint passes 1 GB or memory runs out), so bitrate peaks, slow links and segment requests play out of memory instead of stalling; the converted pictures come from a fixed pool (an 8 MB picture allocated per frame made malloc hold ~15× what was read ahead, which froze the console); a video thread decodes (3-8 threads by picture size) into a queue of 8 frames; an audio thread converts to S16 stereo for SDL; the main thread presents YUV frames in sync with the audio clock. After a seek the audio waits for the first video frame, so the frames between the keyframe and the target aren't counted late.
- Updates: on connect the client asks `/api/v1/client/ps5`; Options downloads it to `eboot.elf.new`, checks size and SHA-256, keeps the old one as `eboot.elf.bak` and asks websrv (`127.0.0.1:8080/hbldr`) to start the new one, which also ends the running copy. The app instead takes the response's `app` files: each one that differs from its own (outside `sce_sys/`, which the home screen copied at install) is fetched to `<file>.new` in `/data/homebrew/<TITLEID>/` and checked, then all are swapped in (old ones kept as `.bak`) and FLEX closes, to be opened again.
- `third_party/cJSON/`: vendored cJSON 1.7.18.

## As its own app

```bash
bash tools/setup-app-kit.sh   # once: the app kit's converter and libc.prx, into ~/ps5-app-kit
make app                      # -> build/app/PPSA84840/{eboot.bin, sce_module/libc.prx, sce_sys/, assets/}
make deploy-app               # upload it to /data/homebrew/PPSA84840 (Shadow Mount+ adds the tile)
make app-release              # build/FLEX-ps5-app.zip, what a release carries
make content-version VERSION=0.8.4   # before tagging: the PlayStation version in app/sce_sys/param.json; commit it
make app APP_ID=PPSA84841 APP_NAME="FLEX Test"   # a second tile for trying a build
```

The payload SDK builds payloads that a loader injects into a running app; it has no tools for apps. The native-app kit inside [Forwarder Manager](https://github.com/heydemoura/ps5-forwarder-manager) (GPL-3.0-or-later, pinned in `tools/setup-app-kit.sh`) has the rest: its linker layout, a converter that turns the linked program into a PS5 executable signed as a fake SELF, and the clean-room `libc.prx` the system loader requires in `sce_module/`. The program itself is the same sources compiled with `FLEX_APP`, the same ports, and:

- `app/start.c`: the app's own startup (an app is started by the system, not injected), closing the app when `main` returns (`sceSystemServiceLoadExec("exit")`); `stdout`/`stderr` go to `/data/homebrew/FLEX/app-log.txt`.
- System modules an app doesn't get loaded leave their imports at address 0: SDL's USB keyboard and text-input dialog calls go through guards (`--wrap`, see `APP_WRAP`) that fail politely, and the start of the black box lists any other unbound import.
- `app/heap.c`: the C library's heap in an app refuses even 8 MB blocks, so FLEX takes 1.5 GB of the app's direct memory (it has 12 GB) and runs the library's own allocator ("mspace") on it; `malloc` and friends are routed there.
- The payload SDK's `libc.a` is linked after the system modules: its `mmap`/`mprotect` make raw system calls and change page rights with the payload's kernel access, which an app has neither of.

Its settings, cache and black box are the payload's (`/data/homebrew/FLEX/`). Shadow Mount+ only accepts title IDs starting PPSA, CUSA, LAPY or FAKE.

For the [homebrew catalog](https://github.com/blackbearreloaded/ps5-homebrew-catalog) behind ProsperoStore, `app/sce_sys/param.json` is committed as FLEX's own (title `PPSA84840`) with a `contentVersion` that rises with every release (0.8.4 is `01.008.004`; the release build fails if the committed value doesn't match the tag), and each release carries `PPSA84840.zip`, the app folder alone. `FLEX-ps5-app.zip` is the same folder plus install notes, and is what FLEX servers fetch.

The home screen's pictures for the tile are `app/sce_sys/pic0.dds` (while FLEX is highlighted) and `pic1.dds` (while it starts): 3840x2160 BC7 DDS files that `go run ./tools/logo` draws and encodes itself, no Windows converter needed (`go run ./tools/logo -backgrounds <dir>` writes PNG previews). The FLEX window on the PC installs the whole app folder, pictures included, onto a console over its FTP server (`server/ps5install.go`); the app's own updates skip `sce_sys/`.

## Next steps

- On-screen keyboard for entering a Tailscale address (`SceImeDialog` is already linked by the SDL port).
- Seek: restart the HLS session with `start=`.
- Audio track selection (`audio=` on `/play`).
