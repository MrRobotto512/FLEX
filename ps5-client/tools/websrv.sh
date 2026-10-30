#!/usr/bin/env bash
# Starts websrv (the homebrew launcher) on the PS5 by sending its ELF to
# elfldr. Needed after every console restart: payloads are not persistent,
# and the Homebrew Launcher app shows "not connected to the internet" when
# websrv is not running behind it.
#
#   bash ps5-client/tools/websrv.sh [ps5-host]      (or: make websrv)
set -euo pipefail

HOST="${1:-${PS5_HOST:-192.168.0.176}}"
PORT="${PS5_PORT:-9021}"
TAG="${WEBSRV_TAG:-v0.34}"
SHA256="54730c867c6e1148536fdcb370e63a7762d989ea87b62488ad4caff64d43f263"   # v0.34
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/flex"
ELF="$CACHE/websrv-ps5-$TAG.elf"

if timeout 2 bash -c "</dev/tcp/$HOST/8080" 2>/dev/null; then
  echo "websrv already running on $HOST:8080"
  exit 0
fi
if ! timeout 2 bash -c "</dev/tcp/$HOST/$PORT" 2>/dev/null; then
  echo "elfldr is not listening on $HOST:$PORT - run the jailbreak / payload loader on the console first" >&2
  exit 1
fi

if [ ! -f "$ELF" ]; then
  mkdir -p "$CACHE"
  echo "downloading websrv $TAG"
  curl -sSL -o "$ELF.part" "https://github.com/ps5-payload-dev/websrv/releases/download/$TAG/websrv-ps5.elf"
  if [ "$TAG" = "v0.34" ]; then echo "$SHA256  $ELF.part" | sha256sum -c - >/dev/null; fi
  mv "$ELF.part" "$ELF"
fi

echo "sending websrv to $HOST:$PORT"
timeout 8 nc -q0 "$HOST" "$PORT" < "$ELF" >/dev/null 2>&1 || true   # elfldr keeps relaying stdout; cut it after 8s
for _ in 1 2 3 4 5 6 7 8 9 10; do
  sleep 1
  if timeout 2 bash -c "</dev/tcp/$HOST/8080" 2>/dev/null; then
    echo "websrv is up: http://$HOST:8080"
    exit 0
  fi
done
echo "websrv did not come up on $HOST:8080" >&2
exit 1
