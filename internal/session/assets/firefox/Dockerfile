# Pinned by digest, like the Chromium image. Update it together with firefoxRev.
FROM ghcr.io/m1k1o/neko/base@sha256:37514a57b987b8987937f335fb24e59c81ce4eb218a3f4fcf2bd1c3dc59b0ad3
USER root
# Optional reproducibility: pass APT_SNAPSHOT=20260930T000000Z (the daemon does so from the
# LOCALIMAGE_APT_SNAPSHOT environment variable) to install the packages below from that dated
# Debian snapshot instead of the current archive, so rebuilds give identical packages. It is off by
# default because snapshot.debian.org can be slow.
ARG APT_SNAPSHOT=""
RUN if [ -n "$APT_SNAPSHOT" ]; then \
      for f in /etc/apt/sources.list.d/*.sources /etc/apt/sources.list; do \
        [ -f "$f" ] && sed -i \
          -e "s|https\?://deb.debian.org/debian-security|http://snapshot.debian.org/archive/debian-security/$APT_SNAPSHOT|g" \
          -e "s|https\?://deb.debian.org/debian|http://snapshot.debian.org/archive/debian/$APT_SNAPSHOT|g" \
          -e "s|https\?://security.debian.org/debian-security|http://snapshot.debian.org/archive/debian-security/$APT_SNAPSHOT|g" "$f"; \
      done; \
      echo 'Acquire::Check-Valid-Until "false";' > /etc/apt/apt.conf.d/99snapshot; \
    fi
ARG PW_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends nodejs npm openbox ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/pw
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers
RUN npm init -y >/dev/null \
 && npm i playwright-core@${PW_VERSION} \
 && npx playwright-core install --with-deps firefox \
 && rm -rf /var/lib/apt/lists/* \
 && chmod -R a+rX /opt/pw-browsers
COPY server.mjs /opt/pw/server.mjs
COPY pw.conf /etc/neko/supervisord/pw.conf
COPY openbox.xml /etc/neko/openbox.xml
COPY start-pw.sh /etc/neko/start-pw.sh
RUN chmod 755 /etc/neko/start-pw.sh
RUN mkdir -p /home/neko/Downloads && chown neko:neko /home/neko/Downloads

# A session is healthy only if the live view (Neko, served under a per-session path prefix) AND the
# browser itself answer. Neko's stock check requested /health at the root, which always failed, and
# did not look at the browser at all, so a crashed browser still showed as healthy.
HEALTHCHECK --interval=10s --timeout=5s --retries=8 \
  CMD wget -qO /dev/null "http://localhost:${NEKO_SERVER_BIND#*:}${NEKO_SERVER_PATH_PREFIX%/}/health" \
   && python3 -c "import socket; socket.create_connection(('127.0.0.1', 3000), 2)" || exit 1
