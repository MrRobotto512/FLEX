#!/usr/bin/env bash
# Installs the PS5 homebrew toolchain into WSL (Ubuntu/Debian) so `make` in
# ps5-client/ works. Idempotent; re-run after an SDK release to upgrade.
#
#   bash ps5-client/tools/setup-wsl.sh
#
# What it does:
#   1. apt: clang-18/lld-18/llvm-18 (the SDK's wrappers call them), unzip, zip,
#      pkg-config, meson, socat, pyelftools, DejaVu fonts
#   2. prebuilt ports from ps5-payload-dev/pacbrew-repo (SDL2, SDL2_ttf,
#      freetype, FFmpeg, ... as static libs) -> /opt/ps5-payload-sdk/target/user/homebrew
#   3. the ps5-payload-dev/sdk release zip on top (toolchain + SDK libs)
#   4. PS5_PAYLOAD_SDK in ~/.bashrc and the font copied into ps5-client/assets
set -euo pipefail

SDK_TAG="${SDK_TAG:-v0.45}"            # https://github.com/ps5-payload-dev/sdk/releases
PORTS_TAG="${PORTS_TAG:-v0.41.1}"      # https://github.com/ps5-payload-dev/pacbrew-repo/releases
SDK_SHA256="5fac64093d0e3e69f2584ce7935f24f703f622d9681f1495c85b9f00d1291a2c"
PORTS_SHA256="e811bc2e4676c5bb7f80a8fe928d2985dce599996a5ca6b54da3e72699287887"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"   # ps5-client/
DL="$(mktemp -d)"
trap 'rm -rf "$DL"' EXIT
SUDO=""; [ "$(id -u)" -eq 0 ] || SUDO=sudo

echo "== apt packages"
export DEBIAN_FRONTEND=noninteractive
$SUDO apt-get update -qq
# llvm-18 provides llvm-config-18, which the SDK's wrappers use to find clang;
# without it every compile fails with "/llvm-clang: No such file or directory"
$SUDO apt-get install -y -qq clang-18 lld-18 llvm-18 unzip zip pkg-config meson socat python3-pyelftools fonts-dejavu-core curl

echo "== prebuilt ports ($PORTS_TAG)"
curl -sSL -o "$DL/ports.tar.gz" \
  "https://github.com/ps5-payload-dev/pacbrew-repo/releases/download/$PORTS_TAG/ps5-payload-dev.tar.gz"
if [ "$PORTS_TAG" = "v0.41.1" ]; then echo "$PORTS_SHA256  $DL/ports.tar.gz" | sha256sum -c -; fi
$SUDO tar xzf "$DL/ports.tar.gz" -C /          # unpacks to /opt/ps5-payload-sdk

echo "== SDK ($SDK_TAG)"
curl -sSL -o "$DL/sdk.zip" \
  "https://github.com/ps5-payload-dev/sdk/releases/download/$SDK_TAG/ps5-payload-sdk.zip"
if [ "$SDK_TAG" = "v0.45" ]; then echo "$SDK_SHA256  $DL/sdk.zip" | sha256sum -c -; fi
$SUDO unzip -o -q -d /opt "$DL/sdk.zip"       # overlays /opt/ps5-payload-sdk

echo "== environment"
grep -q 'PS5_PAYLOAD_SDK' ~/.bashrc || echo 'export PS5_PAYLOAD_SDK=/opt/ps5-payload-sdk' >> ~/.bashrc
export PS5_PAYLOAD_SDK=/opt/ps5-payload-sdk

echo "== assets"
cp -n /usr/share/fonts/truetype/dejavu/DejaVuSans.ttf "$HERE/assets/DejaVuSans.ttf" || true

echo "== sanity: hello_world sample"
make -s -C "$PS5_PAYLOAD_SDK/samples/hello_world" >/dev/null && echo "toolchain OK"
echo
echo "Done. Open a new shell (or: export PS5_PAYLOAD_SDK=/opt/ps5-payload-sdk), then:"
echo "  cd $HERE && make && make run PS5_HOST=<ps5-ip>"
