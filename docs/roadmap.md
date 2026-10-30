# Roadmap

Build order, each step produces something you can run.

1. **Server core** (`server/`, `cmd/flexd/`)
   - [x] scaffold: config, SQLite schema, scanner, ffprobe, API routes, discovery
   - [x] scan a folder, `GET /api/v1/movies` lists it (real library: 325 files on the "Home" PC)
   - [x] direct play with range requests
   - [x] HLS transcode (`ps5` profile) with GPU encoder detection (NVENC verified)
   - [x] TMDB matching, poster cache (293/325 matched on the first real scan)
   - [ ] smarter title cleanup + retry for noisy filenames ("Extended Cut", "Remastered", "Watch …", site prefixes)
   - [x] progress / resume
   - [ ] expose the chosen encoder in `GET /server`
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
5. **TV shows**
   - [x] shows/episodes tables, `S01E02` parsing, TMDB series + per-season episode metadata
   - [x] `/shows`, `/shows/{id}` (seasons, next episode), `/episodes/{id}` play/file/progress
   - [x] web UI: TV tab, episode list, play
   - [x] PS5: L1/R1 Movies/TV, episode list, play
   - [x] verified on the console with the real library (Bob's Burgers S09, TMDB metadata, playback, resume saved)
6. **Comfort**
   - [x] PS5: seeking (±10 s / ±60 s, progress OSD), letterboxing, direct-play resume without transcoding
   - [x] PS5: remembers the server's Tailscale address; 3 s connect timeout
   - [x] server: Tailscale sign-in from the desktop app, `remote` profile for tailnet clients
   - [x] server: restarts websrv on known consoles after a PS5 reboot
   - [x] desktop: tray icon, start with Windows, single instance, close-to-tray
   - [ ] verified on hardware: seek, tray on the library PC, remote from a phone, reboot + rejailbreak
7. **Later**
   - [ ] subtitles rendered client-side (SDL2_ttf) instead of burned in
   - [ ] PIN for LAN guests
