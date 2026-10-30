# Roadmap

Build order, each step produces something you can run.

1. **Server core** (`server/`, `cmd/flexd/`)
   - [x] scaffold: config, SQLite schema, scanner, ffprobe, API routes, discovery
   - [x] scan a folder, `GET /api/v1/movies` lists it (synthetic test library so far)
   - [x] direct play with range requests
   - [x] HLS transcode (`ps5` profile) with GPU encoder detection (NVENC verified)
   - [ ] TMDB matching, poster cache (code exists, not yet run with a key)
   - [x] progress / resume
2. **Desktop** (`desktop/`)
   - [ ] Wails shell around the server, tray icon, autostart
   - [ ] library folder picker, scan status, fix-match UI
   - [ ] show LAN + tailnet addresses, Tailscale toggle
3. **PS5 client** (`ps5-client/`)
   - [x] toolchain in WSL (`tools/setup-wsl.sh`), FLEX.elf builds and runs via elfldr
   - [x] SDL window, controller input, movie list from `/movies` (discovery works on the LAN)
   - [x] direct play on the console: FFmpeg decode → IYUV texture → software renderer, PCM → SDL audio, resume saved on stop
   - [x] HLS playback of a transcoded (NVENC) movie on the console; session torn down on stop
   - [ ] poster grid
   - [ ] resume, seek (restart session with `start=`), audio track pick
   - [ ] install to `/data/homebrew/FLEX/` + launcher icon
4. **Discovery + remote**
   - [ ] UDP discovery in both clients
   - [ ] saved server address on the PS5
   - [ ] tsnet listener, `remote` profile selection
5. **Later**
   - [ ] TV shows
   - [ ] subtitles rendered client-side (SDL2_ttf) instead of burned in
   - [ ] PIN for LAN guests
