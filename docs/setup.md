# Setting up FLEX

## The PC with the movies (Windows)

1. Download `FLEX.exe` from the [latest release](https://github.com/MrRobotto512/FLEX/releases/latest) and put it in a folder of its own, e.g. `C:\FLEX`.
2. Run it. On the first start FLEX downloads FFmpeg (about 115 MB, once; the window shows the progress) and then starts the server. If `ffmpeg.exe` and `ffprobe.exe` are already next to `FLEX.exe`, those are used instead.
3. In the window:
   - **Add movie folder…** for movies, **Add TV folder…** for TV shows (one subfolder per show; season folders optional). Don't list a TV folder as a movie folder too.
   - Paste your TMDB API key ([themoviedb.org/settings/api](https://www.themoviedb.org/settings/api)) for posters and descriptions.
   - **Save & restart server**, then **Scan now**.
   - When Windows Firewall asks, allow FLEX on **private** networks (the PS5 needs it).
   - Tick **Start FLEX with Windows** so it is always running.

Closing the window keeps FLEX running in the tray (on Windows 11 maybe under the ^ arrow). Settings, the database and the downloaded FFmpeg live in `%APPDATA%\FLEX`.

**Updates:** FLEX checks GitHub every few hours. The tray menu and the window's Updates section show "Update to …"; installing restarts FLEX. FLEX.exe must sit in a folder it can write to (not Program Files) for that.

**Away from home:** tick **Enable remote access through Tailscale**, save, then **Sign in to Tailscale…**. Phones and laptops with Tailscale open `http://flex:8484`. See [remote.md](remote.md) for the PS5.

Headless instead of the window: `flexd.exe` with a `.env` (see `.env.example`); it downloads FFmpeg the same way.

Transcoding uses the GPU when it can: NVIDIA NVENC, Intel QuickSync, AMD AMF, then the GPU through Windows (older NVIDIA cards), and only then the CPU (the window shows which: "converts with …"). Only files the PS5 can't play as they are get converted, and only a little ahead of what is being watched. Ports: TCP 8484 (HTTP), UDP 7555 (discovery).

## The PS5 (jailbroken)

1. Download `FLEX-ps5.zip` from the same release and copy its `FLEX` folder to the console so it becomes `/data/homebrew/FLEX/eboot.elf`, `.../sce_sys/icon0.png` and `.../assets/DejaVuSans.ttf` (FTP, or a USB stick and a file manager).
2. Start FLEX from the Homebrew Launcher (websrv must be running). Press Circle once to close the launcher's log screen. FLEX finds the server by itself on the same network.

After a PS5 restart, run the jailbreak as usual; within about a minute the server restarts the Homebrew Launcher on the console. From then on the PS5 updates itself: when the server has a newer version the grid says "Update … ready" and Options installs it.
