# Roadmap

Build order, each step produces something you can run.

1. **Server core** (`server/`, `cmd/flexd/`)
   - [x] scaffold: config, SQLite schema, scanner, ffprobe, API routes, discovery
   - [ ] scan a real folder, `GET /api/v1/movies` lists it
   - [ ] direct play with range requests in a browser
   - [ ] HLS transcode (`ps5` profile) with GPU encoder detection
   - [ ] TMDB matching, poster cache
   - [ ] progress / resume
2. **Desktop** (`desktop/`)
   - [ ] Wails shell around the server, tray icon, autostart
   - [ ] library folder picker, scan status, fix-match UI
   - [ ] show LAN + tailnet addresses, Tailscale toggle
3. **PS5 client** (`ps5-client/`)
   - [ ] hello-world ELF via elfldr, controller input
   - [ ] SDL window, poster grid from `/movies`
   - [ ] FFmpeg playback of one HLS URL (decode → swscale → SDL texture, PCM → SDL audio)
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
