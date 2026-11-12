# FLEX PS5 client. Build under WSL/Linux with the ps5-payload-sdk toolchain
# (tools/setup-wsl.sh installs it):
#   export PS5_PAYLOAD_SDK=/opt/ps5-payload-sdk
#   make            # build/FLEX.elf
#   make run        # send to elfldr on the console (port 9021) for a quick test
#   make deploy     # install to /data/homebrew/FLEX via the console's FTP server
#   make release    # build/FLEX-ps5.elf (no debug info) + FLEX-ps5.zip, what CI attaches to a release
#   make app        # the same as its own PS5 app (see below; needs tools/setup-app-kit.sh)
#   make app-release  # build/FLEX-ps5-app.zip, the app as a release attaches it

ifdef PS5_PAYLOAD_SDK
    include $(PS5_PAYLOAD_SDK)/toolchain/prospero.mk
else
    $(error PS5_PAYLOAD_SDK is undefined)
endif

PS5_HOST     ?= 192.168.0.176
PS5_PORT     ?= 9021
PS5_FTP_PORT ?= 2121

ELF  := build/FLEX.elf
SRCS := src/main.c src/blackbox.c src/net.c src/player.c src/posters.c third_party/cJSON/cJSON.c
OBJS := $(patsubst %.c,build/%.o,$(SRCS))

# Shown on the grid and compared with the update the server offers. CI sets
# it from the release tag; local builds get the commit ("5f165c9-dirty"),
# which counts as older than any release.
VERSION ?= $(or $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//'),dev)

# prospero.mk gives us CC, PKG_CONFIG and PS5_DEPLOY; the ports live under
# $(PS5_SYSROOT)/user/homebrew. The SDL2 port ships sdl2-config but no
# sdl2.pc, so SDL2 comes from sdl2-config and the rest from pkg-config.
SDL2_CONFIG := $(PS5_PAYLOAD_SDK)/bin/prospero-sdl2-config
FFLIBS      := libavformat libavcodec libswscale libswresample libavutil

CFLAGS += -Wall -Wextra -O2 -std=gnu11 -Isrc -Ithird_party/cJSON \
          $(shell $(SDL2_CONFIG) --cflags) \
          $(shell $(PKG_CONFIG) --cflags $(FFLIBS) freetype2 SDL2_image)
LDADD  += $(shell $(SDL2_CONFIG) --libs) -lSDL2_ttf \
          $(shell $(PKG_CONFIG) --libs --static SDL2_image $(FFLIBS) freetype2) \
          -lm -lpthread

all: $(ELF)

$(ELF): $(OBJS)
	@mkdir -p $(dir $@)
	$(CC) $(LDFLAGS) -o $@ $^ $(LDADD)

build/%.o: %.c
	@mkdir -p $(dir $@)
	$(CC) $(CFLAGS) -c -o $@ $<

# cJSON's isinf() trips a generic-selection warning in the SDK's math.h
build/third_party/%.o: CFLAGS += -Wno-unreachable-code-generic-assoc

# the version lives only in main.o; rebuild it when it changes
build/src/main.o: CFLAGS += -DFLEX_VERSION='"$(VERSION)"'
build/src/main.o: build/version-$(VERSION)
build/version-$(VERSION):
	@mkdir -p build
	@rm -f build/version-*
	@touch $@

release: $(ELF)
	$(STRIP) --strip-debug -o build/FLEX-ps5.elf $(ELF)
	rm -rf build/zip build/FLEX-ps5.zip
	mkdir -p build/zip/FLEX/sce_sys build/zip/FLEX/assets
	cp build/FLEX-ps5.elf build/zip/FLEX/eboot.elf
	cp sce_sys/icon0.png build/zip/FLEX/sce_sys/
	cp assets/DejaVuSans.ttf build/zip/FLEX/assets/
	sed 's/@VERSION@/$(VERSION)/' tools/INSTALL.txt > build/zip/INSTALL.txt
	cd build/zip && zip -qr ../FLEX-ps5.zip INSTALL.txt FLEX

# `run` deploys, then asks websrv's homebrew loader (/hbldr, port 8080) to
# start the installed copy as a real app with the screen, relaying its stdout
# here (Ctrl-C only stops the relay). websrv's /elfldr and plain elfldr
# (`make run-headless`) start payloads as background processes: fine for
# daemons, invisible for anything with a window.
run: deploy
	curl -sS -N "http://$(PS5_HOST):8080/hbldr?path=/data/homebrew/FLEX/eboot.elf&pipe=1&daemon=0"

run-headless: $(ELF)
	$(PS5_DEPLOY) -h $(PS5_HOST) -p $(PS5_PORT) $(ELF)

# After a console restart websrv is gone; this brings it back.
websrv:
	PS5_HOST=$(PS5_HOST) PS5_PORT=$(PS5_PORT) bash tools/websrv.sh

deploy: $(ELF)
	curl -sS --ftp-create-dirs -T $(ELF) ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/FLEX/eboot.elf
	curl -sS --ftp-create-dirs -T sce_sys/icon0.png ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/FLEX/sce_sys/icon0.png
	curl -sS --ftp-create-dirs -T assets/DejaVuSans.ttf ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/FLEX/assets/DejaVuSans.ttf

# ---------- FLEX as its own PS5 app ----------
#
# `make app` builds the same program as a real PS5 app, so the home-screen
# tile *is* FLEX (no Homebrew Launcher in between): build/app/$(APP_ID)/ with
# eboot.bin, sce_module/libc.prx, sce_sys/ and the font. Copied to
# /data/homebrew/$(APP_ID)/ (`make deploy-app`), Shadow Mount+ puts it on the
# home screen. The payload SDK has no tools for apps; the app kit inside
# Forwarder Manager (github.com/heydemoura/ps5-forwarder-manager, GPL-3.0)
# supplies its linker layout, the converter that turns the linked program
# into a PS5 executable signed as a fake SELF, and the libc.prx module the
# system loader requires. tools/setup-app-kit.sh builds those into APP_KIT.
APP_KIT  ?= $(HOME)/ps5-app-kit
APP_ID   ?= PPSA84840
APP_NAME ?= FLEX
APP_OUT  := build/app/$(APP_ID)
APP_OBJS := $(patsubst %.c,build/app/obj/%.o,$(SRCS) app/start.c app/heap.c)
APP_TOOL := $(APP_KIT)/build/host/ps5-native-tool
# The payload's libraries, minus compiler-only flags, then the system
# modules (the app's own libkernel, not the web one the payload borrows).
# The payload SDK's libc.a comes last, for what the system's C library lacks
# (getaddrinfo, dlopen, ...): its mmap/mprotect make raw system calls and
# change page rights through the payload's kernel access, which an app has
# neither of, so the system's own must win.
APP_LIBS := $(filter-out -pthread -D%,$(LDADD)) -L$(PS5_SYSROOT)/lib
APP_SYS  := -lkernel -lSceLibcInternal -lSceNet
# System calls that may not be bound in an app; app/start.c guards them.
APP_WRAP := sceKeyboardInit sceKeyboardOpen sceKeyboardClose sceKeyboardReadState \
            sceImeDialogInit sceImeDialogGetStatus sceImeDialogGetResult sceImeDialogTerm \
            sceKernelAllocateMainDirectMemory sceKernelMapDirectMemory sceVideoOutOpen \
            malloc calloc realloc posix_memalign free
APP_LDFLAGS ?=

app: $(APP_OUT)/eboot.bin

build/app/obj/%.o: %.c
	@mkdir -p $(dir $@)
	$(CC) $(CFLAGS) -DFLEX_APP -Iapp -c -o $@ $<

build/app/obj/third_party/%.o: CFLAGS += -Wno-unreachable-code-generic-assoc
build/app/obj/src/main.o: CFLAGS += -DFLEX_VERSION='"$(VERSION)"' -DFLEX_APP_ID='"$(APP_ID)"'
build/app/obj/src/main.o: build/version-$(VERSION) build/app/id-$(APP_ID)
build/app/id-$(APP_ID):
	@mkdir -p build/app
	@rm -f build/app/id-*
	@touch $@

$(APP_OBJS): app/app.h

build/app/pie.elf: $(APP_OBJS) app/symbols.map
	$(LD) -T $(APP_KIT)/tooling/native/ps5-pie.ld --version-script app/symbols.map -e _start $(addprefix --wrap=,$(APP_WRAP)) $(APP_LDFLAGS) -o $@ \
	    $(APP_OBJS) --start-group $(APP_LIBS) --end-group --as-needed $(APP_SYS) -lc

$(APP_OUT)/eboot.bin: build/app/pie.elf app/param.json
	$(APP_TOOL) link --in $< --out build/app/eboot.elf --stub-dir $(PS5_SYSROOT)/lib \
	    --module-sdk 0x02000009 --companion-sdk 0x08050001 --file-name eboot.elf
	rm -rf $(APP_OUT)
	mkdir -p $(APP_OUT)/sce_module $(APP_OUT)/sce_sys $(APP_OUT)/assets
	$(APP_TOOL) self --sign --in build/app/eboot.elf --out $@ --magic 0x1D3D154F
	cp $(APP_KIT)/runtime/libc.prx $(APP_OUT)/sce_module/
	sed 's/@ID@/$(APP_ID)/g; s/@CONCEPT@/$(patsubst PPSA%,%,$(APP_ID))/; s/@NAME@/$(APP_NAME)/' \
	    app/param.json > $(APP_OUT)/sce_sys/param.json
	cp app/sce_sys/icon0.png $(APP_OUT)/sce_sys/
	cp assets/DejaVuSans.ttf $(APP_OUT)/assets/

# The app's folder as PPSA84840 plus how to install it: what CI attaches to
# a release, and what FLEX servers unpack to update consoles' apps.
app-release:
	$(MAKE) app APP_ID=PPSA84840 APP_NAME=FLEX
	rm -rf build/appzip build/FLEX-ps5-app.zip
	mkdir -p build/appzip
	cp -r build/app/PPSA84840 build/appzip/
	sed 's/@VERSION@/$(VERSION)/' tools/INSTALL-app.txt > build/appzip/INSTALL.txt
	cd build/appzip && zip -qr ../FLEX-ps5-app.zip INSTALL.txt PPSA84840

deploy-app: app
	cd $(APP_OUT) && for f in eboot.bin sce_module/libc.prx sce_sys/param.json sce_sys/icon0.png \
	    assets/DejaVuSans.ttf; do \
	    curl -sS --ftp-create-dirs -T $$f ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/$(APP_ID)/$$f || exit 1; \
	done

clean:
	rm -rf build

.PHONY: all run run-headless websrv deploy release app app-release deploy-app clean
