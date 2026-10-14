# Pinned by digest so every machine builds the same Chromium (154.0.8037.57). To update, change this
# digest together with ChromiumVersion and chromiumRev in internal/session/manager.go.
FROM ghcr.io/m1k1o/neko/chromium@sha256:2471119c7b4010067f76f3421a78a8bcf35e3c223b2d1f4db084914ec915fb30
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
ENV LOCALIMAGE_CHROMIUM_ARGS=""
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg \
 && rm -rf /var/lib/apt/lists/*
COPY chromium.conf /etc/neko/supervisord/chromium.conf
COPY cdp_forward.py /etc/neko/cdp_forward.py
COPY policies.json /etc/chromium/policies/managed/policies.json
COPY start-chromium.sh /etc/neko/start-chromium.sh
RUN chmod 755 /etc/neko/start-chromium.sh
RUN mkdir -p /home/neko/.config/chromium /home/neko/Downloads \
 && chown -R neko:neko /home/neko/.config /home/neko/Downloads

# A session is healthy only if the live view (Neko, served under a per-session path prefix) AND the
# browser itself answer. Neko's stock check requested /health at the root, which always failed, and
# did not look at the browser at all, so a crashed browser still showed as healthy.
HEALTHCHECK --interval=10s --timeout=5s --retries=8 \
  CMD wget -qO /dev/null "http://localhost:${NEKO_SERVER_BIND#*:}${NEKO_SERVER_PATH_PREFIX%/}/health" \
   && wget -qO /dev/null http://127.0.0.1:9223/json/version || exit 1
