# FLEX home-screen tile

`app/` is the small PS5 app behind the FLEX tile (title id `PPSA84840`). An
app may not start another app on the PS5, so when you open the tile it asks
websrv's homebrew loader (`127.0.0.1:8080/hbldr`, the request the Homebrew
Launcher sends) to start `/data/homebrew/FLEX/eboot.elf`; the loader closes
the tile app and runs FLEX. If the loader isn't running it shows a
notification. Each step goes to `/data/homebrew/FLEX/tile.log`.

Shadow Mount+ puts it on the home screen: copy the built folder to
`/data/homebrew/PPSA84840/` and it is installed within ~15 s. Shadow Mount+
1.7-beta5 only accepts title ids starting PPSA, CUSA, LAPY or FAKE.

## Building

A bare static `eboot.bin` (the payload SDK's `samples/install_app`) is refused
at launch on firmware 13.60 (CE-107750-0), so the app is built like the
working tiles: with the native-app kit inside
[Forwarder Manager](https://github.com/heydemoura/ps5-forwarder-manager)
(GPL-3.0-or-later; `main.c` is modelled on its forwarder program and is
GPL-3.0-or-later too). In WSL:

```sh
git clone --recurse-submodules https://github.com/heydemoura/ps5-forwarder-manager fm
cd fm && git config core.autocrlf false   # its checksums need LF line endings
bash tools/setup-native-dependencies.sh && make libc USE_CCACHE=0
mkdir -p flex-tile && cp -r <FLEX>/ps5-client/tile/app/. flex-tile/
APP_SOURCE_DIR=flex-tile APP_PARAM=flex-tile/param.json APP_SCE_SYS=flex-tile/sce_sys \
  APP_ASSETS= APP_INCLUDE_PATHS= APP_DEFINITIONS= APP_STATIC_ARCHIVES= APP_IMPORT_STUBS= \
  APP_WRAP_SYMBOLS= APP_RUNTIME_MODULES= APP_ROOT_FILES= APP_LAPY_HELPER=0 PACBREW_PACKAGES= \
  USE_CCACHE=0 bash tools/build.sh Folder
# -> dist/PPSA84840/{eboot.bin, sce_module/libc.prx, sce_sys/}
```

The icon comes from `tools/logo` (`go run ./tools/logo` at the repo root).
