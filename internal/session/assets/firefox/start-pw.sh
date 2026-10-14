#!/bin/sh
# Starts the Playwright server that owns Firefox. Every start is recorded in /tmp/browser-starts
# (milliseconds since the epoch), so the daemon can tell whether it restarted after a crash.
date +%s%3N >> /tmp/browser-starts
cd /opt/pw
exec node /opt/pw/server.mjs
