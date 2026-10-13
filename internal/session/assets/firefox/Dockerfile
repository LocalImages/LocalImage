FROM ghcr.io/m1k1o/neko/base:latest
USER root
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

# Neko's stock check hits /health at the root, but sessions serve everything
# under a per-session path prefix (NEKO_SERVER_PATH_PREFIX), so it always failed.
HEALTHCHECK --interval=10s --timeout=5s --retries=8 \
  CMD wget -qO /dev/null "http://localhost:${NEKO_SERVER_BIND#*:}${NEKO_SERVER_PATH_PREFIX%/}/health" || exit 1
