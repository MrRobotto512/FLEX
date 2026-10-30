# FLEX PS5 client

Native homebrew player for a jailbroken PS5. C, SDL2 (video out, audio, controller), SDL2_ttf (text), FFmpeg (demux + CPU decode). It finds the server over UDP, lists the library and plays the `ps5` profile the server prepares (≤1080p H.264 + AAC) so software decoding keeps up.

## Toolchain (once, in WSL or Linux)

1. Install the [ps5-payload-sdk](https://github.com/ps5-payload-dev/sdk) to `/opt/ps5-payload-sdk` following its README (`export PS5_PAYLOAD_SDK=/opt/ps5-payload-sdk`).
2. Build or install the SDK ports for **SDL2**, **SDL2_ttf** and **freetype** into `$PS5_SYSROOT` (the ps5-payload-dev org has recipes for these).
3. Cross-compile FFmpeg into the sysroot. Starting point:
   ```bash
   ./configure --enable-cross-compile --target-os=freebsd --arch=x86_64 \
     --cc="$CC" --sysroot="$PS5_SYSROOT" --prefix="$PS5_SYSROOT" \
     --disable-programs --disable-doc --disable-shared --enable-static \
     --enable-network --disable-everything \
     --enable-protocol=http,tcp,file,hls --enable-demuxer=hls,mpegts,mov,matroska \
     --enable-decoder=h264,aac,ac3,eac3 --enable-parser=h264,aac,ac3 \
     --enable-swscale --enable-swresample
   make -j && make install
   ```
   Expect to iterate on this; the SDK's own port recipes show what flags its libc needs.
4. Vendor cJSON: copy `cJSON.c` and `cJSON.h` from https://github.com/DaveGamble/cJSON into `third_party/cJSON/`.
5. Put a font at `assets/DejaVuSans.ttf` (on Debian/Ubuntu: `/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf`) and an icon at `sce_sys/icon0.png` (512×512).

## Build, test, install

```bash
make                 # -> build/FLEX.elf
make run             # push to elfldr (port 9021) for a one-off run
make deploy          # upload to /data/homebrew/FLEX via FTP (port 2121)
```

`PS5_HOST` defaults to `192.168.0.176`; override with `make run PS5_HOST=…`. After `make deploy`, the app appears in the websrv homebrew launcher.

## How it works

- `net.c` — UDP discovery (`FLEX_DISCOVER` → JSON reply) and a tiny HTTP/1.0 client with the `X-Flex-Client: ps5` header.
- `main.c` — SDL window, controller input, movie list, saved server address at `/data/homebrew/FLEX/server.txt` (used when discovery finds nothing, e.g. over Tailscale).
- `player.c` — `avformat_open_input` on the URL the server returned (direct file or HLS), decodes with all cores, converts audio to S16 stereo for SDL, presents YUV frames in sync with the audio clock.

## Next steps

- Poster grid (needs SDL2_image + HTTP image fetch).
- On-screen keyboard for entering a Tailscale address.
- Separate decode thread and a frame queue if 1080p stutters.
- Seek: restart the HLS session with `start=`.
- Audio track selection (`audio=` on `/play`).
