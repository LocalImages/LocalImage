FROM ghcr.io/m1k1o/neko/chromium:latest
USER root
ENV LOCALIMAGE_CHROMIUM_ARGS=""
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg \
 && rm -rf /var/lib/apt/lists/*
COPY chromium.conf /etc/neko/supervisord/chromium.conf
COPY cdp_forward.py /etc/neko/cdp_forward.py
COPY policies.json /etc/chromium/policies/managed/policies.json
RUN mkdir -p /home/neko/.config/chromium && chown -R neko:neko /home/neko/.config

# Neko's stock check hits /health at the root, but sessions serve everything
# under a per-session path prefix (NEKO_SERVER_PATH_PREFIX), so it always failed.
HEALTHCHECK --interval=10s --timeout=5s --retries=8 \
  CMD wget -qO /dev/null "http://localhost:${NEKO_SERVER_BIND#*:}${NEKO_SERVER_PATH_PREFIX%/}/health" || exit 1
