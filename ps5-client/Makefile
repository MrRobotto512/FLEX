# FLEX PS5 client. Build under WSL/Linux with the ps5-payload-sdk toolchain:
#   export PS5_PAYLOAD_SDK=/opt/ps5-payload-sdk
#   make            # build/FLEX.elf
#   make run        # send to elfldr on the console (port 9021) for a quick test
#   make deploy     # install to /data/homebrew/FLEX via the console's FTP server
#
# The SDK's sample Makefiles are the reference for the toolchain variables
# (prospero.mk, PS5_SYSROOT); adjust here if your SDK version differs.

ifdef PS5_PAYLOAD_SDK
    include $(PS5_PAYLOAD_SDK)/toolchain/prospero.mk
else
    $(error PS5_PAYLOAD_SDK is undefined)
endif

PS5_HOST ?= 192.168.0.176
PS5_FTP_PORT ?= 2121
PS5_ELFLDR_PORT ?= 9021

ELF    := build/FLEX.elf
SRCS   := src/main.c src/net.c src/player.c third_party/cJSON/cJSON.c
OBJS   := $(patsubst %.c,build/%.o,$(SRCS))

CFLAGS += -Wall -Wextra -O2 -std=gnu11 -Isrc -Ithird_party/cJSON \
          $(shell $(PS5_SYSROOT)/bin/sdl2-config --cflags)
LDADD  += $(shell $(PS5_SYSROOT)/bin/sdl2-config --libs) -lSDL2_ttf -lfreetype \
          -lavformat -lavcodec -lswscale -lswresample -lavutil -lm -lpthread

all: $(ELF)

$(ELF): $(OBJS)
	@mkdir -p $(dir $@)
	$(CC) $(LDFLAGS) -o $@ $^ $(LDADD)

build/%.o: %.c
	@mkdir -p $(dir $@)
	$(CC) $(CFLAGS) -c -o $@ $<

run: $(ELF)
	nc -q0 $(PS5_HOST) $(PS5_ELFLDR_PORT) < $(ELF)

deploy: $(ELF)
	curl --ftp-create-dirs -T $(ELF) ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/FLEX/eboot.elf
	curl --ftp-create-dirs -T sce_sys/icon0.png ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/FLEX/sce_sys/icon0.png
	curl --ftp-create-dirs -T assets/DejaVuSans.ttf ftp://$(PS5_HOST):$(PS5_FTP_PORT)/data/homebrew/FLEX/assets/DejaVuSans.ttf

clean:
	rm -rf build

.PHONY: all run deploy clean
