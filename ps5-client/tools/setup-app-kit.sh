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

echo "== host tools and libc.prx"
(cd "$KIT" && USE_CCACHE=0 bash tools/rebuild-libc.sh)
ls -l "$KIT/build/host/ps5-native-tool" "$KIT/runtime/libc.prx"
echo "kit ready: make app APP_KIT=$KIT"
