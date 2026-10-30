# FLEX desktop (Windows)

A Wails v2 window around the server: library folders, scan, TMDB key, encoder, Tailscale. Settings live in `%APPDATA%\FLEX\settings.json`; the database and posters in `%APPDATA%\FLEX\data`.

## Build

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@latest
wails doctor          # checks Go, WebView2, gcc (needed by Wails on Windows)
cd desktop
wails dev             # live-reload window
wails build           # desktop/build/bin/FLEX.exe
```

The frontend is plain HTML in `frontend/dist/` with no build step; `window.go.main.App.*` are the methods on `App` in `app.go`, bound by Wails.

## Not yet

- Tray icon and "start with Windows" (Wails v2 has no tray; use `github.com/energye/systray` or move to Wails v3 once stable).
- Fix-match UI for wrong TMDB matches (`POST /api/v1/movies/{id}/match` exists on the server).
