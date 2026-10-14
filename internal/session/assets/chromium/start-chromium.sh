#!/bin/sh
# Starts Chromium for a session. Every start is recorded in /tmp/browser-starts (milliseconds
# since the epoch), so the daemon can tell whether the browser has restarted after a crash.
date +%s%3N >> /tmp/browser-starts

# Software rendering by default. LOCALIMAGE_WEBGL=1 turns on WebGL through SwiftShader, which
# costs CPU but lets map, chart and 3D pages work.
GL="--disable-gpu --disable-software-rasterizer"
if [ "$LOCALIMAGE_WEBGL" = "1" ]; then
  GL="--use-gl=angle --use-angle=swiftshader --enable-unsafe-swiftshader --ignore-gpu-blocklist"
fi

# BackForwardCache is off so that back and forward navigation reloads pages the way a test expects,
# instead of restoring a frozen copy.
#
# LOCALIMAGE_LANG sets the browser language (for example de-DE): the UI language and the
# accept-languages list that navigator.language and the Accept-Language header come from; LOCALIMAGE_CHROMIUM_ARGS holds
# extra flags such as --kiosk and --load-extension.
exec /usr/bin/chromium \
  --no-sandbox \
  --window-position=0,0 \
  --display="$DISPLAY" \
  --user-data-dir=/home/neko/.config/chromium \
  --no-first-run \
  --start-maximized \
  --disable-dev-shm-usage \
  --remote-debugging-port=9223 \
  --remote-allow-origins='*' \
  --disable-features=BackForwardCache \
  ${LOCALIMAGE_LANG:+--lang=$LOCALIMAGE_LANG --accept-lang=$LOCALIMAGE_LANG} \
  $GL \
  $LOCALIMAGE_CHROMIUM_ARGS
