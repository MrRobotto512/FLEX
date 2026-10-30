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
make run             # push to elfldr (port 9021) and run it now; the console's stdout is relayed to your shell
make deploy          # upload ELF + icon + font to /data/homebrew/FLEX over FTP (port 2121)
```

`PS5_HOST` defaults to `192.168.0.176`; override with `make run PS5_HOST=…`. `make run` alone is enough for a quick test **once `make deploy` has put the font in `/data/homebrew/FLEX/assets/`**; without it the app exits with "font missing". After `make deploy` the app also appears in the websrv homebrew launcher with its icon.

The FLEX server must be running on the PC (`go run ./cmd/flexd` or the desktop app). The first time it listens, Windows asks whether to allow `flexd.exe` on the network; say yes for private networks or the PS5 will report "No server found".

## Controls

| Where | Button | Action |
| --- | --- | --- |
| List | D-pad up/down | move |
| List | Cross | play (resumes if partially watched) |
| List | Triangle | re-discover the server and refresh |
| List | Circle | quit |
| Player | Cross | pause / resume |
| Player | Circle | stop (progress is saved) |

## How it works

- `net.c`: UDP discovery (`FLEX_DISCOVER` → JSON reply) and a tiny HTTP/1.0 client with the `X-Flex-Client: ps5` header.
- `main.c`: SDL window, controller input, movie list, saved server address at `/data/homebrew/FLEX/server.txt` (used when discovery finds nothing, e.g. over Tailscale).
- `player.c`: `avformat_open_input` on the URL the server returned (direct file or HLS), decodes with all cores, converts audio to S16 stereo for SDL, presents YUV frames in sync with the audio clock.
- `third_party/cJSON/`: vendored cJSON 1.7.18.

## Next steps

- Poster grid (SDL2_image is in the prebuilt ports).
- On-screen keyboard for entering a Tailscale address (`SceImeDialog` is already linked by the SDL port).
- Separate decode thread and a frame queue if 1080p stutters.
- Seek: restart the HLS session with `start=`.
- Audio track selection (`audio=` on `/play`).
