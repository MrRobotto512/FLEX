# FLEX PS5 client. Build under WSL/Linux with the ps5-payload-sdk toolchain
# (tools/setup-wsl.sh installs it):
#   export PS5_PAYLOAD_SDK=/opt/ps5-payload-sdk
#   make            # build/FLEX.elf
#   make run        # send to elfldr on the console (port 9021) for a quick test
#   make deploy     # install to /data/homebrew/FLEX via the console's FTP server

ifdef PS5_PAYLOAD_SDK
    include $(PS5_PAYLOAD_SDK)/toolchain/prospero.mk
else
    $(error PS5_PAYLOAD_SDK is undefined)
endif

PS5_HOST     ?= 192.168.0.176
PS5_PORT     ?= 9021
PS5_FTP_PORT ?= 2121

ELF  := build/FLEX.elf
SRCS := src/main.c src/net.c src/player.c third_party/cJSON/cJSON.c
OBJS := $(patsubst %.c,build/%.o,$(SRCS))

# prospero.mk gives us CC, PKG_CONFIG and PS5_DEPLOY; the ports live under
# $(PS5_SYSROOT)/user/homebrew. The SDL2 port ships sdl2-config but no
# sdl2.pc, so SDL2 comes from sdl2-config and the rest from pkg-config.
SDL2_CONFIG := $(PS5_PAYLOAD_SDK)/bin/prospero-sdl2-config
FFLIBS      := libavformat libavcodec libswscale libswresample libavutil

CFLAGS += -Wall -Wextra -O2 -std=gnu11 -Isrc -Ithird_party/cJSON \
          $(shell $(SDL2_CONFIG) --cflags) \
          $(shell $(PKG_CONFIG) --cflags $(FFLIBS) freetype2)
LDADD  += $(shell $(SDL2_CONFIG) --libs) -lSDL2_ttf \
          $(shell $(PKG_CONFIG) --libs --static $(FFLIBS) freetype2) \
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

run: $(ELF)
	$(PS5_DEPLOY) -h $(PS5_HOST) -p $(PS5_PORT) $(ELF)

deploy: $(ELF)
	curl --ftp-create-dirs -T $(ELF) ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/FLEX/eboot.elf
	curl --ftp-create-dirs -T sce_sys/icon0.png ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/FLEX/sce_sys/icon0.png
	curl --ftp-create-dirs -T assets/DejaVuSans.ttf ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/FLEX/assets/DejaVuSans.ttf

clean:
	rm -rf build

.PHONY: all run deploy clean
