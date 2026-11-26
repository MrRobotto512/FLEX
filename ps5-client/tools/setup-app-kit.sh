#!/usr/bin/env bash
# Builds what `make app` needs from the native-app kit inside Forwarder
# Manager (https://github.com/heydemoura/ps5-forwarder-manager, GPL-3.0-or-
# later), pinned to one commit:
#   - build/host/ps5-native-tool: turns the linked program into a PS5
#     executable and signs it as a fake SELF (eboot.bin)
#   - runtime/libc.prx: the clean-room loader module every such app ships in
#     sce_module/; the kit's own script rebuilds it and refuses to finish
#     unless it matches the digest the kit records
#   - tooling/native/ps5-pie.ld: the linker layout the converter expects
# Run after tools/setup-wsl.sh (it needs clang-18); idempotent.
#
#   bash ps5-client/tools/setup-app-kit.sh          # into ~/ps5-app-kit
#   APP_KIT=/some/dir bash ps5-client/tools/setup-app-kit.sh
set -euo pipefail

KIT="${APP_KIT:-$HOME/ps5-app-kit}"
REPO="https://github.com/heydemoura/ps5-forwarder-manager"
REV="7c1e744d80c3e1c19642be8407b2baf4be7282eb"
SUDO=""; [ "$(id -u)" -eq 0 ] || SUDO=sudo

missing=()
for tool in ninja wget make; do command -v "$tool" >/dev/null || missing+=("$tool"); done
if [ ${#missing[@]} -gt 0 ]; then
  echo "== apt packages"
  $SUDO apt-get update -qq
  $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ninja-build wget make
fi

echo "== kit $REV"
if [ ! -d "$KIT/.git" ]; then
  git init -q "$KIT"
  git -C "$KIT" remote add origin "$REPO"
fi
git -C "$KIT" config core.autocrlf false   # its checksums need LF line endings
git -C "$KIT" fetch -q --depth 1 origin "$REV"
git -C "$KIT" checkout -q --force FETCH_HEAD

# The kit's host tools build zlib 1.3.2 from a tarball it fetches from
# zlib.net alone, and stops if the checksum is off. zlib.net now and then
# hands CI machines something else, which failed releases; so the same
# tarball (same checksum, the kit's own) is put in the kit's cache first,
# from zlib's GitHub release and then zlib.net, and the kit finds it there.
ZLIB_FILE="$KIT/.deps/native/zlib/zlib-1.3.2.tar.gz"
ZLIB_SUM="bb329a0a2cd0274d05519d61c667c062e06990d72e125ee2dfa8de64f0119d16"
zlib_ok() { echo "$ZLIB_SUM  $ZLIB_FILE" | sha256sum --check --strict >/dev/null 2>&1; }
if ! zlib_ok; then
  echo "== zlib 1.3.2"
  mkdir -p "$(dirname "$ZLIB_FILE")"
  for url in https://github.com/madler/zlib/releases/download/v1.3.2/zlib-1.3.2.tar.gz \
             https://zlib.net/fossils/zlib-1.3.2.tar.gz https://zlib.net/fossils/zlib-1.3.2.tar.gz; do
    wget -q "$url" -O "$ZLIB_FILE" && zlib_ok && break
    rm -f "$ZLIB_FILE"
  done
  zlib_ok || echo "zlib: none of the downloads matched the checksum; the kit will try once more itself"
fi

echo "== host tools and libc.prx"
(cd "$KIT" && USE_CCACHE=0 bash tools/rebuild-libc.sh)
ls -l "$KIT/build/host/ps5-native-tool" "$KIT/runtime/libc.prx"
echo "kit ready: make app APP_KIT=$KIT"
