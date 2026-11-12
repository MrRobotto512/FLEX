# Setting up FLEX

## The PC with the movies (Windows)

1. Download `FLEX.exe` from the [latest release](https://github.com/MrRobotto512/FLEX/releases/latest) and put it in a folder of its own, e.g. `C:\FLEX`.
2. Run it. On the first start FLEX downloads FFmpeg (about 260 MB, once; the window shows the progress) and then starts the server. If `ffmpeg.exe` and `ffprobe.exe` are already next to `FLEX.exe`, those are used instead.
3. In the window:
   - **Add movie folder…** for movies, **Add TV folder…** for TV shows (one subfolder per show; season folders optional). Don't list a TV folder as a movie folder too.
   - Paste your TMDB API key ([themoviedb.org/settings/api](https://www.themoviedb.org/settings/api)) for posters and descriptions.
   - Optional, under **Home screen**: pick Claude, ChatGPT or Gemini and paste an API key from that provider ([console.anthropic.com](https://console.anthropic.com), [platform.openai.com](https://platform.openai.com), [aistudio.google.com](https://aistudio.google.com)). About once a week it sorts your library into themed rows for the home screen, the time of year first. Each refresh is billed to your account with that provider (with Claude, roughly $0.30). Without a key the rows come from TMDB genres. With several FLEX PCs, one key is enough.
   - **Save & restart server**, then **Scan now**.
   - When Windows Firewall asks, allow FLEX on **private** networks (the PS5 needs it).
   - Tick **Start FLEX with Windows** so it is always running.

After that the library keeps itself up to date: movies and episodes copied into (or deleted from) a library folder show up (or go) about half a minute after the copying stops, and a full scan runs every night at 4:00. **Scan now** is still there for a drive that doesn't report changes (some network drives).

To take a folder out of the library, press **Remove** next to it, then **Save & restart server**: its movies and shows (and their resume points) leave the library right away.

Closing the window keeps FLEX running in the tray (on Windows 11 maybe under the ^ arrow). Settings, the database and the downloaded FFmpeg live in `%APPDATA%\FLEX`.

**Updates:** FLEX checks GitHub every few hours. The tray menu and the window's Updates section show "Update to …"; installing restarts FLEX. FLEX.exe must sit in a folder it can write to (not Program Files) for that.

**More than one PC with movies:** run FLEX on each. They find each other on the home network and each lists the others' movies and shows next to its own (the window shows them under **Other FLEX PCs**), so the PS5, browsers and phones see the whole library whichever PC they reach. A movie plays from the PC that has it, converted by that PC's GPU, so that PC has to be on. Untick **Show the movies and shows of the other FLEX PCs** to keep a PC to itself. PCs on different networks (one on the internet provider's Wi-Fi, the other behind your own router) still find each other when both have Tailscale (the app, or FLEX's own **Remote access**): FLEX remembers where each answered and asks the devices on the tailnet, at their direct address first, so no settings are needed; the window says "over Tailscale" when that is the only way through.

**Away from home:** tick **Enable remote access through Tailscale**, save, then **Sign in to Tailscale…**. Phones and laptops with Tailscale open `http://flex:8484`. See [remote.md](remote.md) for the PS5.

Headless instead of the window: `flexd.exe` with a `.env` (see `.env.example`); it downloads FFmpeg the same way.

Transcoding uses the GPU when it can: NVIDIA NVENC, Intel QuickSync, AMD AMF, then the GPU through Windows (older NVIDIA cards), and only then the CPU (the window shows which: "converts with …"). Only files the PS5 can't play as they are get converted, and only a little ahead of what is being watched. Ports: TCP 8484 (HTTP), UDP 7555 (discovery).

**Season prep:** under **Season prep** in the window, FLEX upscales the season's movies and episodes ahead of time (the AI's holiday row picks them; you can add any title), so the PS5 plays finished 1080p files instead of converting live, and removes them once the season is over. If Topaz Video AI is installed on that PC, enter its folder (e.g. `C:\Program Files\Topaz Labs LLC\Topaz Video AI`) and model (`prob-3` is Proteus; copy the model name from Topaz's "Show export command") and FLEX uses it; otherwise the built-in AI shaders do the work. Each PC prepares its own files with its own GPU.

**Upscaling:** Triangle on the PS5 turns server upscaling on or off (remembered). At home each FLEX PC upscales old SD and 720p video with AI on its own GPU, when the GPU is fast enough: Anime4K for cartoons, FSRCNNX for everything else. FLEX downloads both (about 1 MB) on the first start and tests them; the window shows "AI upscaling: …" when they passed. Over Tailscale, and on GPUs that are too slow, it upscales the plain way.

## A Windows PC under the TV (media PC)

The same `FLEX.exe` is the player. Install it on the media PC as above (it needs no movie folders of its own: with **Show the movies and shows of the other FLEX PCs** ticked it lists the other PCs' libraries and they do the converting), then:

- **Open player (full screen)** in the window or the tray menu shows the library on the TV: arrow keys or a game controller move around, Enter plays, Backspace/Escape goes back, Tab switches Home → Movies → TV, **C** casts to a Chromecast, **Q** or Alt+F4 leaves.
- Tick **Open the player when FLEX starts** together with **Start FLEX with Windows** and the TV goes straight to FLEX after the PC boots.

The player decodes with the PC's GPU (through the Edge engine Windows ships with): 4K and H.264 play as they are, HEVC too once Microsoft's "HEVC Video Extensions" is installed from the Store; everything else is converted by the PC that has the file (up to 4K for the player).

## Chromecast

Press **C** on a title in the browser, the Android app or the player (or while it plays) and pick a Chromecast or a TV with Chromecast built in. The FLEX PC finds the devices on the home network itself and sends the stream (H.264/AAC, converted when the file isn't an MP4 the device plays), so this works from any client, not just Chrome. The same screen is the remote: Enter pauses, arrows seek, Backspace stops. Progress is saved as usual, so the title can be picked up on the PS5 or anywhere else.

## Android TV boxes, phones and tablets

1. Download `FLEX-android.apk` from the same release and open it on the device (a USB stick or the box's browser, or `adb install FLEX-android.apk`). Android asks once to allow installing from that source.
2. Start FLEX. It finds the server on the same network by itself (with more than one FLEX PC it asks which; either one shows the whole library), or you can type an address such as `192.168.0.205:8484`. It remembers the server's Tailscale address too, so away from home it still opens with Tailscale on the device.

**Away from home:** turn on **Enable remote access through Tailscale** on the FLEX PC (see [remote.md](remote.md)), install the [Tailscale app](https://tailscale.com/download) on the phone or tablet and sign in to the same tailnet. The FLEX app remembers the PC's Tailscale address whenever it reaches the PC (at home or away), and uses it by itself when the home address doesn't answer; the status line says "remote" and streams are sized for your home's upload speed. The address can also be typed by hand: hold Back → *Enter address* → `flex:8484` (the PC's Tailscale name). The PS5 can't run Tailscale; the phone can, which is why this works for the app and not for the console.

It is the same screen as the browser, full screen. On a TV: the D-pad and OK move and open, **Back** closes the player or an episode list, **Menu** switches Home → Movies → TV and **holding Menu** opens *Cast to…*, the remote's play/pause and forward/rewind keys work in the player, **holding Back** opens the app's menu (search for FLEX PCs, enter an address, check for updates). On a phone or tablet: tap a poster to play, tap the picture for the control bar (pause, ±10 s, ±60 s, Cast, stop), **Cast** and **Back** buttons are on the page, and the **⋮** button at the top right opens the app's menu (so does holding Back). **Updates:** when the FLEX PC runs a newer FLEX than the app, a bar at the top says so with an **Update the app** button, and the app offers the APK by itself too; *Check for updates* in the menu asks GitHub directly and works anywhere. The page itself always comes from the FLEX PC, so a PC that hasn't updated yet shows the older page.

## The PS5 (jailbroken)

1. Download `FLEX-ps5.zip` from the same release and copy its `FLEX` folder to the console so it becomes `/data/homebrew/FLEX/eboot.elf`, `.../sce_sys/icon0.png` and `.../assets/DejaVuSans.ttf` (FTP, or a USB stick and a file manager).
2. Start FLEX from the Homebrew Launcher (websrv must be running). Press Circle once to close the launcher's log screen. FLEX finds the server by itself on the same network.

After a PS5 restart, run the jailbreak as usual; within about a minute the server restarts the Homebrew Launcher on the console. From then on the PS5 updates itself: when the server has a newer version the grid says "Update … ready" and Options installs it.
