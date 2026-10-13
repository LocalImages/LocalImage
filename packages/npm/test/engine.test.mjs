// What a test-runner engine relies on: readiness, prepare, abort and timeouts, recordings,
// downloads and window geometry. Needs Docker and a running daemon (see e2e.test.mjs).
import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import * as pw from 'playwright-core';
import { LocalImage, LocalImageError } from '../src/index.js';

const li = new LocalImage();
const daemonUp = await li.list().then(() => true, () => false);
const t = (name, fn) => test(name, { skip: !daemonUp && 'daemon not running', timeout: 300_000 }, fn);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const errorOf = (p) => p.then(() => null, (e) => e);
const SCREEN = { width: 1440, height: 900 };

t('create returns only when CDP answers; prepare and noBuild work', async () => {
  const info = await li.info();
  assert.equal(info.images.chromium, true, 'the chromium image is built');

  const events = [];
  const result = await li.prepare({ browsers: ['chromium'], onProgress: (e) => events.push(e) });
  assert.equal(result.chromium, 'ready', 'an existing image is reported as ready, quickly');
  assert.ok(events.some((e) => e.browser === 'chromium'), 'progress is reported');
  assert.equal((await errorOf(li.prepare({ browsers: ['netscape'] })))?.code, 'invalid_request');

  const s = await li.create({ noBuild: true, ttlSeconds: 300 });
  try {
    // No polling: the very first request after create must succeed.
    const r = await fetch(`${s.connectUrl}/json/version`);
    assert.equal(r.status, 200);
    const v = await r.json();
    assert.match(v.webSocketDebuggerUrl, /^ws:\/\/[^/]+\/t\/[0-9a-f]+\/cdp\/devtools\/browser\//, 'the ws URL points back at the proxy');
    assert.equal(s.downloadsDir, '/home/neko/Downloads');
  } finally {
    await s.close();
  }
});

t('abort: a cancelled create leaves no container and no slot behind', async () => {
  const before = await li.info();
  const pre = new AbortController();
  pre.abort();
  const e0 = await errorOf(li.create({ signal: pre.signal }));
  assert.equal(e0?.name, 'AbortError', 'an already-aborted signal rejects at once');
  assert.ok(!(e0 instanceof LocalImageError), 'a cancellation is not reported as a failure');

  const reason = new Error('release budget exhausted');
  for (const delay of [0, 5, 30, 120, 400, 1200]) {
    const ac = new AbortController();
    setTimeout(() => ac.abort(reason), delay);
    try {
      const s = await li.create({ signal: ac.signal, ttlSeconds: 300 });
      await s.close(); // it finished before the abort: fine, but release it
    } catch (e) {
      assert.equal(e, reason, `abort at ${delay} ms rejects with the signal's reason`);
    }
  }
  await sleep(6000); // the daemon removes what it had started
  const after = await li.info();
  assert.equal(after.running, before.running, 'no slot is left occupied');
  assert.equal((await li.list()).length, before.running, 'no container is left behind');
});

t('abort and timeout work on delete, downloads and any call', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    const pre = AbortSignal.abort();
    assert.equal((await errorOf(s.close({ signal: pre })))?.name, 'AbortError');
    assert.equal((await li.list()).some((x) => x.id === s.id), true, 'an aborted delete did not delete');

    await s.exec(['sh', '-c', 'head -c 20000000 /dev/zero > /tmp/big.bin']);
    const ac = new AbortController();
    const p = s.downloadFile('/tmp/big.bin', { signal: ac.signal });
    ac.abort();
    assert.equal((await errorOf(p))?.name, 'AbortError');
    assert.equal((await s.downloadFile('/tmp/big.bin')).length, 20_000_000, 'the session is fine afterwards');
  } finally {
    await s.close();
  }

  // A daemon that never answers: timeoutMs gives a clear, stable error, not "unreachable".
  const silent = http.createServer(() => {});
  await new Promise((r) => silent.listen(0, '127.0.0.1', r));
  try {
    const slow = new LocalImage({ addr: `127.0.0.1:${silent.address().port}` });
    const start = Date.now();
    const e = await errorOf(slow.info({ timeoutMs: 400 }));
    assert.equal(e?.code, 'request_timeout');
    assert.ok(Date.now() - start < 3000);
  } finally {
    silent.closeAllConnections();
    silent.close();
  }

  // And a slow answer is not cut off: there is no hidden ceiling on a request.
  const slowServer = http.createServer((req, res) => setTimeout(() => res.end('{"version":"x","images":{}}'), 2500));
  await new Promise((r) => slowServer.listen(0, '127.0.0.1', r));
  try {
    const patient = new LocalImage({ addr: `127.0.0.1:${slowServer.address().port}` });
    assert.equal((await patient.info()).version, 'x');
  } finally {
    slowServer.close();
  }
});

t('recording: mp4 with real timestamps, measured duration and codec details', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    await sleep(1000);
    const before = Date.now();
    const started = await s.startRecording();
    const returned = Date.now();
    assert.ok(returned - before < 2500, `start returned in ${returned - before} ms`);
    assert.equal(started.content_type, 'video/mp4');
    assert.ok(started.started_at_ms >= before - 50 && started.started_at_ms <= returned, 'started_at is when capture began');
    assert.equal(started.status, 'recording');

    await sleep(4000);
    const stopped = await s.stopRecording();
    assert.equal(stopped.replay_id, started.replay_id);
    assert.equal(stopped.status, 'stopped');
    assert.deepEqual([stopped.codec, stopped.pix_fmt, stopped.width, stopped.height, stopped.fps], ['h264', 'yuv420p', SCREEN.width, SCREEN.height, 15]);
    const wall = stopped.stopped_at_ms - stopped.started_at_ms;
    assert.ok(wall >= 3800 && wall <= 5500, `stopped - started = ${wall} ms`);
    assert.ok(Math.abs(stopped.duration_ms - wall) < 600, `media duration ${stopped.duration_ms} ms matches ${wall} ms`);
    assert.ok(stopped.finalized_at_ms >= stopped.stopped_at_ms, 'the file is finalized after capture stops');

    const mp4 = await s.downloadRecording(started.replay_id);
    assert.equal(mp4.length, stopped.size, 'the downloaded file is the whole recording');
    assert.equal(mp4.subarray(4, 8).toString(), 'ftyp');
    const [listed] = await s.recordings();
    assert.deepEqual([listed.replay_id, listed.status, listed.running], [started.replay_id, 'stopped', false]);
  } finally {
    await s.close();
  }
});

// A page with a link that downloads "hello world".
const DOWNLOAD_PAGE =
  '<title>dl</title><body style="margin:0"><a id=a download="hello.txt" href="data:text/plain;base64,aGVsbG8gd29ybGQ=" ' +
  'style="position:absolute;left:100px;top:100px;width:300px;height:80px;background:#9cf">download</a>';

t('downloads: the directory is ready, and both Playwright and a person downloading can be read back', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    const sh = async (cmd) => (await s.exec(['sh', '-c', cmd])).stdout.trim();
    assert.equal(await sh('stat -c "%U %a" /home/neko/Downloads'), 'neko 755', 'exists, owned by the browser user');
    assert.equal(await sh('touch /home/neko/Downloads/.w && echo ok; rm -f /home/neko/Downloads/.w'), 'ok', 'writable by the browser user');
    assert.deepEqual(await s.listFiles(s.downloadsDir), [], 'empty to start with');

    const b = await s.connect(pw);
    const page = b.contexts()[0].pages()[0] ?? (await b.contexts()[0].newPage());
    await page.goto('data:text/html,' + encodeURIComponent(DOWNLOAD_PAGE));

    // 1. While Playwright is attached: its download lives in the container; read it by path.
    const [d] = await Promise.all([page.waitForEvent('download'), page.click('#a')]);
    assert.equal(d.suggestedFilename(), 'hello.txt');
    const path = await d.path();
    assert.match(path, /^\/tmp\/playwright-artifacts-/);
    assert.equal((await s.downloadFile(path)).toString(), 'hello world', 'download.path() + downloadFile()');

    // 2. A person clicks the link in the live view (no automation attached): it lands in downloadsDir.
    const at = await page.evaluate(() => {
      const r = document.getElementById('a').getBoundingClientRect();
      return { x: window.screenX + (outerWidth - innerWidth) / 2 + r.left + r.width / 2, y: window.screenY + (outerHeight - innerHeight) + r.top + r.height / 2 };
    });
    await b.close();
    await s.click(Math.round(at.x), Math.round(at.y));
    for (let i = 0; i < 30 && !(await s.listFiles(s.downloadsDir)).length; i++) await sleep(200);
    const files = await s.listFiles(s.downloadsDir);
    assert.deepEqual(files.map((f) => [f.name, f.size]), [['hello.txt', 11]]);
    assert.equal((await s.downloadFile(`${s.downloadsDir}/hello.txt`)).toString(), 'hello world');
  } finally {
    await s.close();
  }
});

// Where the page actually ends up, in screen terms.
const measure = (page) =>
  page.evaluate(() => ({ inner: [innerWidth, innerHeight], outer: [outerWidth, outerHeight], at: [screenX, screenY] }));

t('viewport: a window fills the screen with the default page or viewport: null, not with the default newContext()', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    const b = await s.connect(pw);
    const first = b.contexts()[0].pages()[0] ?? (await b.contexts()[0].newPage());
    await first.goto('about:blank');
    let m = await measure(first);
    assert.deepEqual([m.outer, m.at], [[SCREEN.width, SCREEN.height], [0, 0]], 'the existing page fills the screen');
    assert.equal(first.viewportSize(), null, 'no viewport emulation on the default context');

    const filled = await (await b.newContext({ viewport: null })).newPage();
    await filled.goto('about:blank');
    m = await measure(filled);
    assert.deepEqual([m.outer, m.at], [[SCREEN.width, SCREEN.height], [0, 0]], 'viewport: null fills the screen');
    assert.equal(m.inner[0], SCREEN.width);

    // The pitfall: Playwright's own default viewport (1280x720) is smaller than the display.
    const small = await (await b.newContext()).newPage();
    await small.goto('about:blank');
    assert.deepEqual((await measure(small)).inner, [1280, 720], 'a plain newContext() emulates 1280x720');
    await b.close();
  } finally {
    await s.close();
  }
});

t('viewport: kiosk gives the existing page exactly the screen; firefox needs viewport: null too', async () => {
  const k = await li.create({ kiosk: true, ttlSeconds: 300 });
  try {
    const b = await k.connect(pw);
    const page = b.contexts()[0].pages()[0] ?? (await b.contexts()[0].newPage());
    await page.goto('about:blank');
    assert.deepEqual((await measure(page)).inner, [SCREEN.width, SCREEN.height], 'kiosk: no browser chrome at all');
    await b.close();
  } finally {
    await k.close();
  }

  const f = await li.create({ browser: 'firefox', ttlSeconds: 300 });
  try {
    const b = await f.connect(pw);
    const page = await (await b.newContext({ viewport: null })).newPage();
    await page.goto('about:blank');
    await page.waitForTimeout(500);
    const m = await measure(page);
    assert.deepEqual([m.outer, m.at], [[SCREEN.width, SCREEN.height], [0, 0]], 'firefox with viewport: null fills the screen');
    await b.close();
  } finally {
    await f.close();
  }
});

t('the SDK objects have exactly the declared members (src/index.d.ts and test/types/usage.ts)', async () => {
  const SESSION_KEYS = [
    'id', 'browser', 'protocol', 'connectUrl', 'liveUrl', 'viewUrl', 'profile', 'extensions', 'tags',
    'downloadsDir', 'expiresAt', 'connect', 'screenshot', 'click', 'moveMouse', 'type', 'pressKey', 'scroll',
    'drag', 'startRecording', 'stopRecording', 'recordings', 'downloadRecording', 'exec', 'uploadFile',
    'downloadFile', 'listFiles', 'close',
  ];
  const CLIENT_KEYS = [
    'addr', 'apiKey', 'autoStart', 'daemonEnv', 'ensureDaemon', 'shutdown', 'info', 'assertCapacity', 'prepare',
    'create', 'list', 'delete', 'uploadExtension', 'extensions', 'deleteExtension', 'profiles', 'deleteProfile',
  ];
  const publicKeys = (o) =>
    [...new Set([...Object.keys(o), ...Object.getOwnPropertyNames(Object.getPrototypeOf(o))])]
      .filter((k) => k !== 'constructor' && !k.startsWith('_'));
  assert.deepEqual(publicKeys(li).sort(), [...CLIENT_KEYS].sort(), 'LocalImage');
  const s = await li.create({ ttlSeconds: 120 });
  try {
    assert.deepEqual(Object.keys(s).sort(), [...SESSION_KEYS].sort(), 'Session');
  } finally {
    await s.close();
  }
});
