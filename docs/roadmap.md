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
   - [x] PS5: picture shape (Square), remembered per item; seek no longer stalls waiting for audio
   - [x] TV folders (everything inside is an episode), many numbering styles, season folders in movie folders
   - [x] title cleanup + TMDB retry ladder (leading index, year-first, " - Genre", editions, "Part 1")
   - [x] show names from "X Season 3 Complete" / "The Complete Series" folders, season folders right under a TV folder, double episodes, shows merged by TMDB id, episodes re-placed when parsing improves
   - [x] server-side upscaling (Triangle on the PS5), deinterlacing for every transcode
   - [x] `/library/unmatched` with file paths
   - [x] scanner: 4 workers, "current" activity in the status, unreadable drives don't wipe their items, "TV Shows" in the movie list treated as TV, duplicate shows merged after every scan
   - [x] transcoding: all-GPU pipeline with NVENC (CUDA decode, bwdif_cuda, scale_cuda), -hwaccel auto otherwise, CPU fallback; paced to 1.5x playback after a 60 s burst; `/transcodes`, encoder shown in the app
   - [x] GPU encoding through Windows (`h264_mf`) for GPUs ffmpeg's NVENC rejects (GTX 1070 Ti on the 580 driver); `/server` reports GPUs and why each encoder failed
   - [x] season folders named with air years ("1993-1994 - Season 1"), "0000 - Shorts" as specials, segment files ("201-a"); same-name series (TMNT 1987/2003/2012) told apart by an episode title, existing shows re-matched once
   - [x] verified on hardware: seek
   - [ ] verified on hardware: tray on the library PC, remote from a phone, reboot + rejailbreak
7. **Later**
   - [ ] subtitles rendered client-side (SDL2_ttf) instead of burned in
   - [ ] PIN for LAN guests
