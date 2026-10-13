const DEFAULT_ADDR = process.env.LOCALIMAGE_ADDR ?? '127.0.0.1:7777';

async function call(addr, apiKey, method, path, body, { raw = false } = {}) {
  const isBinary = body instanceof Uint8Array;
  let res;
  try {
    res = await fetch(`http://${addr}${path}`, {
      method,
      headers: {
        'Content-Type': isBinary ? 'application/octet-stream' : 'application/json',
        ...(apiKey ? { Authorization: `Bearer ${apiKey}` } : {}),
      },
      body: body === undefined ? undefined : isBinary ? body : JSON.stringify(body),
    });
  } catch (e) {
    throw new Error(`localimage daemon not reachable at ${addr}; run \`localimage start\` (${e.message})`);
  }
  if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error ?? `HTTP ${res.status}`);
  if (raw) return Buffer.from(await res.arrayBuffer());
  return res.status === 204 ? null : res.json();
}

export class LocalImage {
  constructor({ addr = DEFAULT_ADDR, apiKey = process.env.LOCALIMAGE_API_KEY } = {}) {
    this.addr = addr;
    this.apiKey = apiKey;
  }

  /**
   * Start a browser session.
   * Returns { id, browser, protocol, connectUrl, liveUrl, viewUrl, profile, expiresAt,
   *            connect(), screenshot(), startRecording(), stopRecording(), downloadRecording(), close() }.
   * `protocol` is 'cdp' (chromium) or 'playwright' (firefox).
   * `liveUrl` lets a viewer control the browser; `viewUrl` is watch-only.
   * `profile` names persistent browser state (chromium only).
   */
  async create({ browser = 'chromium', screen, ttlSeconds, profile, memoryMb, cpus, kiosk, extensions } = {}) {
    const s = await call(this.addr, this.apiKey, 'POST', '/v1/sessions', {
      browser,
      screen,
      ttl_seconds: ttlSeconds,
      profile,
      memory_mb: memoryMb,
      cpus,
      kiosk,
      extensions,
    });
    const computer = (action, body = {}) => post(`/computer/${action}`, body);
    const url = (p) => `/v1/sessions/${s.id}${p}`;
    const get = (p, raw) => call(this.addr, this.apiKey, 'GET', url(p), undefined, { raw });
    const post = (p, body) => call(this.addr, this.apiKey, 'POST', url(p), body);
    return {
      id: s.id,
      browser: s.browser,
      protocol: s.protocol,
      connectUrl: s.connect_url,
      liveUrl: s.live_url,
      viewUrl: s.view_url,
      profile: s.profile,
      extensions: s.extensions ?? [],
      expiresAt: new Date(s.expires_at),
      /** Connect with playwright-core (a peer dependency), or the module you pass in. */
      connect: async (pw) => {
        pw ??= await import('playwright-core');
        return s.protocol === 'cdp'
          ? pw.chromium.connectOverCDP(s.connect_url)
          : pw[s.browser].connect(s.connect_url);
      },
      /** PNG of the display; pass { x, y, width, height } to crop. */
      screenshot: (region) => get(`/screenshot${region ? `?${new URLSearchParams(region)}` : ''}`, true),
      // Computer controls act on the whole display (screen coordinates), not a page.
      click: (x, y, { button, numClicks } = {}) => computer('click', { x, y, button, num_clicks: numClicks }),
      moveMouse: (x, y) => computer('move', { x, y }),
      type: (text, { delayMs } = {}) => computer('type', { text, delay_ms: delayMs }),
      pressKey: (...keys) => computer('key', { keys }),
      scroll: (x, y, { deltaX = 0, deltaY = 0 } = {}) => computer('scroll', { x, y, delta_x: deltaX, delta_y: deltaY }),
      drag: (path, { button } = {}) => computer('drag', { path, button }),
      /** Begin a recording. Resolves to { replay_id, status }. One can run at a time. */
      startRecording: () => post('/recording/start'),
      /** Stop the running recording (or `replayId`). Resolves to { replay_id, status }. */
      stopRecording: ({ replayId } = {}) => post('/recording/stop', replayId ? { replay_id: replayId } : undefined),
      recordings: () => get('/recordings'),
      /** MP4 bytes of a recording; the latest if `replayId` is omitted. */
      downloadRecording: (replayId) => get(replayId ? `/recordings/${replayId}` : '/recording', true),
      /** Run a command in the session container as the browser user (no shell). */
      exec: (command, { cwd, timeoutSeconds } = {}) =>
        post('/process/exec', { command, cwd, timeout_seconds: timeoutSeconds }),
      uploadFile: (path, data) =>
        call(this.addr, this.apiKey, 'PUT', url(`/files?${new URLSearchParams({ path })}`), Buffer.from(data)),
      downloadFile: (path) => get(`/files?${new URLSearchParams({ path })}`, true),
      listFiles: (path) => get(`/files/list?${new URLSearchParams({ path })}`),
      close: () => this.delete(s.id),
    };
  }

  list() {
    return call(this.addr, this.apiKey, 'GET', '/v1/sessions');
  }

  /** Upload an unpacked Chromium extension as a zip (manifest.json at the root or in one folder). */
  uploadExtension(name, zip) {
    return call(this.addr, this.apiKey, 'POST', `/v1/extensions?${new URLSearchParams({ name })}`, Buffer.from(zip));
  }

  extensions() {
    return call(this.addr, this.apiKey, 'GET', '/v1/extensions');
  }

  deleteExtension(name) {
    return call(this.addr, this.apiKey, 'DELETE', `/v1/extensions/${name}`);
  }

  profiles() {
    return call(this.addr, this.apiKey, 'GET', '/v1/profiles');
  }

  deleteProfile(name) {
    return call(this.addr, this.apiKey, 'DELETE', `/v1/profiles/${name}`);
  }

  delete(id) {
    return call(this.addr, this.apiKey, 'DELETE', `/v1/sessions/${id}`);
  }
}
