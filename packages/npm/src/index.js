import { spawn } from 'node:child_process';
import { closeSync, openSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { readKeyFile } from './apikey.js';
import { makeExecutable, resolveBinary } from './binary.js';
import { ERROR_CODES, LocalImageError } from './errors.js';
import { request as call } from './transport.js';

export { ERROR_CODES, LocalImageError };

/** The daemon API revision this SDK speaks. assertCompatible() checks the daemon against it. */
export const SUPPORTED_API_VERSION = 1;

const abortReason = (signal) => signal.reason ?? new DOMException('This operation was aborted', 'AbortError');
// The options every network method accepts: cancel with `signal`, or give up after `timeoutMs`.
const pick = ({ raw, signal, timeoutMs } = {}) => ({ raw, signal, timeoutMs });

const DEFAULT_ADDR = process.env.LOCALIMAGE_ADDR ?? '127.0.0.1:7777';
const daemonURL = (addr) => `${addr.includes('://') ? addr : `http://${addr}`}/healthz`;
const listenAddr = (addr) => new URL(addr.includes('://') ? addr : `http://${addr}`).host;

// Resolves to { pid } when a daemon answers at addr (pid is null for an old daemon), else null.
async function healthy(addr) {
  try {
    const r = await fetch(daemonURL(addr), { signal: AbortSignal.timeout(1500) });
    if (!r.ok) return null;
    const body = await r.json().catch(() => ({}));
    return { pid: body.pid ?? null };
  } catch {
    return null;
  }
}

/**
 * Make sure a daemon is running at `addr`, starting one if needed, and wait until it answers.
 * An already-running daemon is reused. The daemon is started detached and keeps running after
 * this process exits, so parallel workers and later runs share it.
 *
 * Resolves to { started, addr, pid?, logFile? }. Rejects with `daemon_start_failed`,
 * `docker_unavailable` (the daemon could not reach Docker) or `binary_not_found`.
 * `env` is passed to a daemon started here, for example { LOCALIMAGE_MAX_SESSIONS: '20' };
 * it has no effect on a daemon that is already running.
 */
export async function ensureDaemon({ addr = DEFAULT_ADDR, apiKey, env = {}, timeoutMs = 30_000, logFile, signal } = {}) {
  if (signal?.aborted) throw abortReason(signal);
  if (await healthy(addr)) return { started: false, addr };

  const bin = resolveBinary();
  if (!bin) {
    throw new LocalImageError(
      `no localimage binary for ${process.platform}-${process.arch}; install the matching @localimage package or set LOCALIMAGE_BIN`,
      { code: 'binary_not_found' },
    );
  }
  makeExecutable(bin);

  const log = logFile ?? join(tmpdir(), `localimage-daemon-${addr.replace(/[^\w.-]/g, '_')}.log`);
  const fd = openSync(log, 'a');
  let exitCode = null;
  let child;
  try {
    child = spawn(bin, ['start', '-addr', listenAddr(addr)], {
      detached: true,
      stdio: ['ignore', fd, fd],
      windowsHide: true,
      env: { ...process.env, ...env, ...(apiKey ? { LOCALIMAGE_API_KEY: apiKey } : {}) },
    });
  } finally {
    closeSync(fd);
  }
  child.on('error', () => { exitCode = -1; });
  child.on('exit', (code) => { exitCode = code ?? -1; });
  child.unref();

  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (signal?.aborted) throw abortReason(signal);
    const h = await healthy(addr);
    if (h) {
      // Several processes may start a daemon at once; only one binds the port and the others exit.
      // `started` is true only for the process whose daemon is the one answering.
      const ours = h.pid === null || h.pid === child.pid;
      return ours ? { started: true, addr, pid: child.pid, logFile: log } : { started: false, addr };
    }
    if (exitCode !== null) {
      // Our process exited. If another process started a daemon at the same moment, that one
      // answers; otherwise this really failed.
      await new Promise((r) => setTimeout(r, 300));
      if (await healthy(addr)) return { started: false, addr };
      break;
    }
    await new Promise((r) => setTimeout(r, 100));
  }

  let tail = '';
  try {
    tail = readFileSync(log, 'utf8').trim().split('\n').slice(-6).join('\n');
  } catch {
    /* no log */
  }
  const dockerDown = /cannot reach docker/i.test(tail);
  throw new LocalImageError(
    `${dockerDown ? 'Docker is not available' : 'the localimage daemon did not start'} at ${addr} (log ${log})${tail ? `:\n${tail}` : ''}`,
    { code: dockerDown ? 'docker_unavailable' : 'daemon_start_failed' },
  );
}

export class LocalImage {
  /**
   * @param {object} [opts]
   * @param {string} [opts.addr] daemon address (default LOCALIMAGE_ADDR or 127.0.0.1:7777)
   * @param {string} [opts.apiKey] daemon API key (default LOCALIMAGE_API_KEY)
   * @param {boolean} [opts.autoStart] start the daemon on the first call if none is running
   *   (default: true when LOCALIMAGE_AUTOSTART=1)
   * @param {object} [opts.daemonEnv] environment for a daemon started by autoStart
   */
  constructor({
    addr = DEFAULT_ADDR,
    apiKey,
    autoStart = process.env.LOCALIMAGE_AUTOSTART === '1',
    daemonEnv = {},
  } = {}) {
    this.addr = addr;
    this._apiKey = apiKey;
    this.autoStart = autoStart;
    this.daemonEnv = daemonEnv;
    this._daemon = null;
  }

  /**
   * The key sent to the daemon: the `apiKey` option, else LOCALIMAGE_API_KEY, else the key file the
   * daemon creates (read when a call is made, so a daemon started by autoStart is covered).
   */
  get apiKey() {
    return this._apiKey ?? (process.env.LOCALIMAGE_API_KEY || readKeyFile());
  }

  /** Start the daemon if it is not running (see the exported ensureDaemon). */
  ensureDaemon(opts = {}) {
    return ensureDaemon({ addr: this.addr, apiKey: this.apiKey, env: this.daemonEnv, ...opts });
  }

  async _call(method, path, body, opts) {
    try {
      return await call(this.addr, this.apiKey, method, path, body, opts);
    } catch (e) {
      if (!(e instanceof LocalImageError) || e.code !== 'daemon_unreachable' || !this.autoStart) throw e;
      this._daemon ??= this.ensureDaemon(); // concurrent calls share one start-up
      try {
        await this._daemon;
      } catch (startErr) {
        this._daemon = null;
        throw startErr;
      }
      if (opts?.signal?.aborted) throw abortReason(opts.signal);
      return call(this.addr, this.apiKey, method, path, body, opts);
    }
  }

  /**
   * Stop the daemon. Running sessions are not removed; they expire by TTL once a daemon runs again.
   * Idempotent: resolves true if a daemon was stopped, false if none was running.
   */
  async shutdown(opts) {
    try {
      await call(this.addr, this.apiKey, 'POST', '/v1/shutdown', undefined, pick(opts));
      return true;
    } catch (e) {
      if (e instanceof LocalImageError && e.code === 'daemon_unreachable') return false;
      throw e;
    }
  }

  /** Generate and activate a new daemon API key. The previous key stops working immediately. */
  async rotateKey(opts) {
    const result = await this._call('POST', '/v1/auth/rotate-key', undefined, pick(opts));
    if (this._apiKey !== undefined) this._apiKey = result.api_key;
    return result.api_key;
  }

  /**
   * The daemon's version, limits and load: { version, maxSessions, maxTtlSeconds, running,
   * available, images }, where `images` says which browser images are built, e.g.
   * { chromium: true, firefox: false }. A missing image is built on first use; see prepare().
   */
  async info(opts) {
    const i = await this._call('GET', '/v1/info', undefined, pick(opts));
    return {
      version: i.version,
      maxSessions: i.max_sessions,
      maxTtlSeconds: i.max_ttl_seconds,
      running: i.running,
      available: i.available,
      images: i.images ?? {},
      apiVersion: i.api_version ?? 0,
      components: i.components ?? {},
      staleImages: i.stale_images ?? 0,
    };
  }

  /**
   * Check that the daemon is one this SDK can talk to, and optionally that its browsers are the
   * versions you expect. Rejects with `incompatible_daemon`. Resolves to info().
   *
   *   await li.assertCompatible({ chromium: /^154\\./ });
   */
  async assertCompatible({ chromium, firefox, ...opts } = {}) {
    const info = await this.info(opts);
    const bad = [];
    if (info.apiVersion !== SUPPORTED_API_VERSION) {
      bad.push(`its API version is ${info.apiVersion || 'unknown (an old daemon)'}, this SDK needs ${SUPPORTED_API_VERSION}`);
    }
    for (const [name, want] of Object.entries({ chromium, firefox })) {
      if (want === undefined) continue;
      const have = info.components[name]?.version ?? '';
      if (!(want instanceof RegExp ? want.test(have) : have === String(want))) bad.push(`${name} is ${have || 'unknown'}, expected ${want}`);
    }
    if (bad.length) {
      throw new LocalImageError(`the daemon at ${this.addr} (version ${info.version}) is not compatible: ${bad.join('; ')}`, {
        code: 'incompatible_daemon',
        details: { apiVersion: info.apiVersion },
      });
    }
    return info;
  }

  /**
   * Fail early, with a clear error, when `n` sessions could never run at once. Call it before
   * starting a pool of workers. It checks the daemon's limit (LOCALIMAGE_MAX_SESSIONS), not the
   * sessions free right now: others may finish, and create({ waitSeconds }) queues for a slot.
   * Resolves to info(); rejects with `limit_exceeded` and details { max, requested }.
   */
  async assertCapacity(n, opts) {
    const info = await this.info(opts);
    if (n > info.maxSessions) {
      throw new LocalImageError(
        `${n} concurrent sessions were requested but the daemon at ${this.addr} allows ${info.maxSessions}; ` +
          `restart it with LOCALIMAGE_MAX_SESSIONS=${n} (or higher), or use fewer workers`,
        { code: 'limit_exceeded', details: { max: info.maxSessions, requested: n } },
      );
    }
    return info;
  }

  /**
   * Build the browser images that are missing, so the first session does not have to. The first
   * Chromium launch builds an image (about a minute, longer for Firefox), which is more than a
   * typical attempt budget: call this in a prepare phase, then create sessions with
   * `noBuild: true` so they fail fast with `image_not_ready` instead of building mid-attempt.
   *
   * Resolves to { chromium: 'ready' | 'built', ... }. `onProgress({ browser, message })` is
   * called with build steps. Aborting `signal` cancels the build; Docker keeps the layers it
   * finished, so the next call continues from there. There is no timeout unless you pass
   * `timeoutMs`.
   */
  async prepare({ browsers = ['chromium'], onProgress, signal, timeoutMs } = {}) {
    let last = null;
    await this._call('POST', '/v1/images/prepare', { browsers }, {
      signal,
      timeoutMs,
      onLine: (ev) => {
        last = ev;
        if (ev.event === 'progress') onProgress?.({ browser: ev.browser, message: ev.message });
      },
    });
    if (!last || last.event === 'ping') {
      throw new LocalImageError('the daemon closed the connection before the build finished', { code: 'daemon_unreachable' });
    }
    if (last.event === 'error') throw new LocalImageError(last.error, { code: last.code ?? 'internal' });
    return last.images;
  }

  /**
   * Start a browser session.
   * Returns { id, browser, protocol, connectUrl, liveUrl, viewUrl, profile, tags, expiresAt,
   *            connect(), screenshot(), startRecording(), stopRecording(), downloadRecording(), close() }.
   * `protocol` is 'cdp' (chromium) or 'playwright' (firefox).
   * `liveUrl` lets a viewer control the browser; `viewUrl` is watch-only.
   * `profile` names persistent browser state (chromium only).
   * `tags` is { key: value } metadata stored on the session, returned by list() and usable as a
   * list filter (up to 20 tags; keys are letters, digits, '.', '_' and '-').
   * `waitSeconds` queues for a free session slot instead of failing at once with `limit_exceeded`.
   * `noBuild: true` fails with `image_not_ready` instead of building a missing browser image.
   * `webgl: true` enables WebGL (software rendering; Chromium only). `timezone` (an IANA name such as
   * `Europe/Berlin`) and `locale` (such as `de-DE`; Chromium only) set the browser's timezone and
   * language. For Firefox, set them on the Playwright context.
   * `signal` cancels the call (an AbortError); the daemon removes whatever it had started, so an
   * abandoned create leaves no container or slot behind. `timeoutMs` gives up with `request_timeout`.
   * `downloadsDir` is where the browser saves downloads; read files with downloadFile().
   */
  async create({ browser = 'chromium', screen, ttlSeconds, profile, memoryMb, cpus, kiosk, extensions, tags, waitSeconds, noBuild, webgl, timezone, locale, signal, timeoutMs } = {}) {
    const s = await this._call('POST', '/v1/sessions', {
      browser,
      screen,
      ttl_seconds: ttlSeconds,
      profile,
      memory_mb: memoryMb,
      cpus,
      kiosk,
      extensions,
      tags,
      wait_seconds: waitSeconds,
      no_build: noBuild,
      webgl,
      timezone,
      locale,
    }, { signal, timeoutMs });
    return this._session(s);
  }

  /** Build the Session object for a session the daemon described. Used by create() and attach(). */
  _session(s) {
    const url = (p) => `/v1/sessions/${s.id}${p}`;
    const get = (p, opts) => this._call('GET', url(p), undefined, pick(opts));
    const post = (p, body, opts) => this._call('POST', url(p), body, pick(opts));
    const computer = (action, body = {}) => post(`/computer/${action}`, body);
    const session = {
      id: s.id,
      browser: s.browser,
      protocol: s.protocol,
      connectUrl: s.connect_url,
      liveUrl: s.live_url,
      viewUrl: s.view_url,
      profile: s.profile,
      extensions: s.extensions ?? [],
      tags: s.tags ?? {},
      downloadsDir: s.downloads_dir,
      expiresAt: new Date(s.expires_at),
      /** Connect with playwright-core (a peer dependency), or the module you pass in. */
      connect: async (pw) => {
        pw ??= await import('playwright-core');
        return s.protocol === 'cdp'
          ? pw.chromium.connectOverCDP(s.connect_url)
          : pw[s.browser].connect(s.connect_url);
      },
      /** PNG of the display; pass { x, y, width, height } to crop. */
      screenshot: (region, opts) => get(`/screenshot${region ? `?${new URLSearchParams(region)}` : ''}`, { ...opts, raw: true }),
      // Computer controls act on the whole display (screen coordinates), not a page.
      click: (x, y, { button, numClicks } = {}) => computer('click', { x, y, button, num_clicks: numClicks }),
      moveMouse: (x, y) => computer('move', { x, y }),
      type: (text, { delayMs } = {}) => computer('type', { text, delay_ms: delayMs }),
      pressKey: (...keys) => computer('key', { keys }),
      scroll: (x, y, { deltaX = 0, deltaY = 0 } = {}) => computer('scroll', { x, y, delta_x: deltaX, delta_y: deltaY }),
      drag: (path, { button } = {}) => computer('drag', { path, button }),
      /**
       * Begin a recording (video/mp4, H.264). Resolves once frames are being written, to
       * { replay_id, status, started_at, started_at_ms, content_type, ... }; `started_at` is when
       * capture began, the video's time zero. One recording can run at a time. It stops by itself
       * after `maxSeconds` (default 3600), so a forgotten recording cannot fill the disk.
       */
      startRecording: ({ maxSeconds, ...opts } = {}) => post('/recording/start', maxSeconds ? { max_seconds: maxSeconds } : undefined, opts),
      /**
       * Stop the running recording (or `replayId`). The file is complete when this resolves, and the
       * result adds stopped_at, duration_ms, size, width, height, fps and codec.
       */
      stopRecording: ({ replayId, ...opts } = {}) => post('/recording/stop', replayId ? { replay_id: replayId } : undefined, opts),
      recordings: (opts) => get('/recordings', opts),
      /** MP4 bytes of a recording; the latest if `replayId` is omitted. */
      downloadRecording: (replayId, opts) => get(replayId ? `/recordings/${replayId}` : '/recording', { ...opts, raw: true }),
      /** Run a command in the session container as the browser user (no shell). */
      exec: (command, { cwd, timeoutSeconds, ...opts } = {}) =>
        post('/process/exec', { command, cwd, timeout_seconds: timeoutSeconds }, opts),
      uploadFile: (path, data, opts) =>
        this._call('PUT', url(`/files?${new URLSearchParams({ path })}`), Buffer.from(data), pick(opts)),
      /**
       * The end of one of the session's logs, as text. `source` is `container` (default), `chromium`,
       * `firefox`, `neko`, `xorg`, `openbox`, `pulseaudio` or `cdp-forward`; `tail` is a number of
       * lines (default 200). Works for a stopped session too, to see why it died.
       */
      logs: async ({ source, tail, ...opts } = {}) => {
        const q = new URLSearchParams();
        if (source) q.set('source', source);
        if (tail) q.set('tail', String(tail));
        return (await get(`/logs${q.size ? `?${q}` : ''}`, { ...opts, raw: true })).toString('utf8');
      },
      /** Read a file from the session, e.g. a download (see downloadsDir). */
      downloadFile: (path, opts) => get(`/files?${new URLSearchParams({ path })}`, { ...opts, raw: true }),
      listFiles: (path, opts) => get(`/files/list?${new URLSearchParams({ path })}`, opts),
      /**
       * Whether the browser is up and whether it has restarted: { state, health, starts, restarts,
       * last_started_at }. After a crash the browser comes back by itself in about a second on the same
       * URL, but as a new, empty browser: `restarts` going up is how a client finds out.
       */
      health: (opts) => get('/health', opts),
      /**
       * Change the session's tags after creation: `set` adds or replaces, `remove` deletes. Resolves
       * to the resulting tags and updates `tags` on this object.
       */
      updateTags: async ({ set, remove, ...opts } = {}) => {
        const r = await this._call('PATCH', url('/tags'), { set, remove }, pick(opts));
        session.tags = r.tags;
        return r.tags;
      },
      /**
       * Write the session's logs (every source) to `dir/<id>/<source>.log` and resolve to the file
       * paths. A session's logs die with its container, so call this when a test fails, before close().
       */
      saveLogs: async (dir, opts) => {
        const { mkdir, writeFile } = await import('node:fs/promises');
        const { join } = await import('node:path');
        const target = join(dir, s.id);
        await mkdir(target, { recursive: true });
        const written = [];
        for (const source of ['container', 'chromium', 'firefox', 'neko', 'xorg', 'openbox', 'pulseaudio', 'cdp-forward']) {
          const text = (await get(`/logs?source=${source}&tail=5000`, { ...opts, raw: true })).toString('utf8');
          if (!text.trim()) continue;
          const file = join(target, `${source}.log`);
          await writeFile(file, text);
          written.push(file);
        }
        return written;
      },
      /** Delete the session. Safe to call more than once; resolves true if this call removed it. */
      close: (opts) => this.delete(s.id, opts),
    };
    return session;
  }

  /**
   * Get a Session for one that already exists, by id: from another process, after a restart, or
   * from a stored lease. Rejects with `session_gone` if it is not there. A stopped session can be
   * attached too, to read its logs.
   */
  async attach(id, opts) {
    return this._session(await this._call('GET', `/v1/sessions/${id}`, undefined, pick(opts)));
  }

  /**
   * List sessions in every state (a crashed worker can leave stopped containers), as the daemon
   * returns them: { id, browser, protocol, state, tags, created_at, expires_at, ... }.
   * `tags` keeps only sessions that carry all of those tags.
   */
  list({ tags, ...opts } = {}) {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(tags ?? {})) q.append('tag', `${k}=${v}`);
    return this._call('GET', `/v1/sessions${q.size ? `?${q}` : ''}`, undefined, pick(opts));
  }

  /**
   * Delete a session. Idempotent: resolves true if this call removed it and false if it was
   * already gone (deleted, expired, or never existed), so releasing a lease twice, or after a
   * failed acquire, never throws. Other failures, such as `daemon_unreachable`, still reject.
   */
  async delete(id, opts) {
    try {
      await this._call('DELETE', `/v1/sessions/${id}`, undefined, pick(opts));
      return true;
    } catch (e) {
      if (e instanceof LocalImageError && e.code === 'session_gone') return false;
      throw e;
    }
  }

  /** Upload an unpacked Chromium extension as a zip (manifest.json at the root or in one folder). */
  /**
   * Remove browser images left behind by older versions of localimage. Resolves to the tags it
   * removed. info().staleImages says how many there are.
   */
  async prune(opts) {
    return (await this._call('POST', '/v1/images/prune', undefined, pick(opts))).removed;
  }

  uploadExtension(name, zip) {
    return this._call('POST', `/v1/extensions?${new URLSearchParams({ name })}`, Buffer.from(zip));
  }

  extensions() {
    return this._call('GET', '/v1/extensions');
  }

  deleteExtension(name) {
    return this._call('DELETE', `/v1/extensions/${name}`);
  }

  profiles() {
    return this._call('GET', '/v1/profiles');
  }

  deleteProfile(name) {
    return this._call('DELETE', `/v1/profiles/${name}`);
  }
}
