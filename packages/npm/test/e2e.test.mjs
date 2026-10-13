// End-to-end tests. They need Docker, a running `localimage start` daemon and
// network access (the profile test uses example.com). Run: npm test
import test from 'node:test';
import assert from 'node:assert/strict';
import * as pw from 'playwright-core';
import { LocalImage } from '../src/index.js';

const li = new LocalImage();
const daemonUp = await li.list().then(() => true, () => false);
const t = (name, fn) => test(name, { skip: !daemonUp && 'daemon not running', timeout: 300_000 }, fn);

const page = async (b) => {
  const ctx = b.contexts?.()[0];
  return ctx ? (ctx.pages()[0] ?? (await ctx.newPage())) : b.newPage();
};

for (const browser of ['chromium', 'firefox']) {
  t(`${browser}: create, drive, screenshot, record, close`, async () => {
    const s = await li.create({ browser, ttlSeconds: 600 });
    try {
      const b = await s.connect(pw);
      const p = await page(b);
      await p.goto('data:text/html,<title>hello-localimage</title><h1>hi</h1>');
      assert.equal(await p.title(), 'hello-localimage');

      const png = await s.screenshot();
      assert.deepEqual([...png.subarray(0, 4)], [0x89, 0x50, 0x4e, 0x47], 'screenshot is a PNG');

      await s.startRecording();
      await p.waitForTimeout(2000);
      await s.stopRecording();
      const mp4 = await s.downloadRecording();
      assert.equal(mp4.subarray(4, 8).toString(), 'ftyp', 'recording is an MP4');
      await b.close();
    } finally {
      await s.close();
    }
    assert.equal((await li.list()).some((x) => x.id === s.id), false, 'session removed');
  });
}

t('read-only view cannot control the browser or reach CDP', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    const v = new URL(s.viewUrl);
    const c = new URL(s.liveUrl);
    const login = async (base, usr, pwd) =>
      (await fetch(base + 'api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username: usr, password: pwd }),
      })).json();

    const viewer = await login(v.origin + v.pathname, 'viewer', v.searchParams.get('pwd'));
    const ctl = await login(c.origin + c.pathname, 'controller', c.searchParams.get('pwd'));
    assert.equal(viewer.profile.can_host, false);
    assert.equal(ctl.profile.can_host, true);

    const viewToken = v.pathname.split('/')[2];
    assert.equal((await fetch(`${v.origin}/v/${viewToken}/cdp/json/version`)).status, 404);
    assert.equal((await fetch(`${v.origin}/t/${viewToken}/cdp/json/version`)).status, 404);
  } finally {
    await s.close();
  }
});

t('chromium profile persists cookies and localStorage across sessions', async () => {
  const name = `e2e-${Date.now().toString(36)}`;
  const connectPage = async (s) => {
    const b = await s.connect(pw);
    const p = await page(b);
    await p.goto('https://example.com');
    return { b, p };
  };
  try {
    let s = await li.create({ profile: name, ttlSeconds: 600 });
    {
      const { b, p } = await connectPage(s);
      await p.context().addCookies([
        { name: 'li', value: 'kept', domain: 'example.com', path: '/', expires: Math.floor(Date.now() / 1000) + 86400 },
      ]);
      await p.evaluate(() => localStorage.setItem('li', 'kept'));
      await b.close();
    }
    await s.close();

    s = await li.create({ profile: name, ttlSeconds: 600 });
    try {
      const { b, p } = await connectPage(s);
      const cookie = (await p.context().cookies('https://example.com')).find((c) => c.name === 'li');
      assert.equal(cookie?.value, 'kept');
      assert.equal(await p.evaluate(() => localStorage.getItem('li')), 'kept');
      await assert.rejects(li.create({ profile: name }), /already in use/);
      await b.close();
    } finally {
      await s.close();
    }
  } finally {
    await li.deleteProfile(name).catch(() => {});
  }
});

t('firefox rejects profiles', async () => {
  await assert.rejects(li.create({ browser: 'firefox', profile: 'x' }), /only supported for chromium/);
});

for (const browser of ['chromium', 'firefox']) {
  t(`${browser}: computer controls click, type, key, scroll, drag`, async () => {
    const s = await li.create({ browser, ttlSeconds: 600 });
    try {
      const b = await s.connect(pw);
      const p = await page(b);
      const html = `
        <title>cc</title>
        <body style="margin:0;height:3000px">
        <button id=b style="position:absolute;left:100px;top:100px;width:160px;height:60px"
                onclick="this.dataset.n=(+this.dataset.n||0)+1">go</button>
        <input id=i style="position:absolute;left:100px;top:200px;width:300px;height:40px">
        <div id=d style="position:absolute;left:100px;top:300px;width:400px;height:200px;background:#ddd"></div>
        <script>
          window.ev = [];
          d.onmousedown = e => ev.push(['down', e.offsetX, e.offsetY]);
          d.onmouseup = e => ev.push(['up', e.offsetX, e.offsetY]);
        </script></body>`;
      await p.goto('data:text/html,' + encodeURIComponent(html));
      await p.bringToFront();
      await p.waitForTimeout(500);

      // Page coordinates -> screen coordinates (window position plus browser chrome).
      const toScreen = async (sel, fx = 0.5, fy = 0.5) =>
        p.evaluate(([sel, fx, fy]) => {
          const r = document.querySelector(sel).getBoundingClientRect();
          const chromeTop = window.outerHeight - window.innerHeight;
          const chromeLeft = (window.outerWidth - window.innerWidth) / 2;
          return [
            Math.round(window.screenX + chromeLeft + r.left + r.width * fx),
            Math.round(window.screenY + chromeTop + r.top + r.height * fy),
          ];
        }, [sel, fx, fy]);

      const [bx, by] = await toScreen('#b');
      await s.click(bx, by);
      assert.equal(await p.getAttribute('#b', 'data-n'), '1', 'button clicked');
      await s.click(bx, by, { numClicks: 2 });
      assert.equal(await p.getAttribute('#b', 'data-n'), '3', 'double click adds two');

      const [ix, iy] = await toScreen('#i');
      await s.click(ix, iy);
      await s.type('hello world');
      assert.equal(await p.inputValue('#i'), 'hello world', 'typed text');
      await s.pressKey('ctrl+a');
      await s.type('X');
      assert.equal(await p.inputValue('#i'), 'X', 'select-all then replace');

      const [dx, dy] = await toScreen('#d', 0.2, 0.2);
      await s.drag([[dx, dy], [dx + 80, dy + 40], [dx + 160, dy + 60]]);
      const ev = await p.evaluate(() => window.ev);
      assert.deepEqual(ev.map((e) => e[0]), ['down', 'up'], 'drag fires down then up');
      assert.ok(ev[1][1] - ev[0][1] > 100, 'mouse moved right while dragging');

      await s.scroll(dx, dy, { deltaY: 10 });
      assert.ok((await p.evaluate(() => window.scrollY)) > 0, 'page scrolled');

      const crop = await s.screenshot({ x: 0, y: 0, width: 200, height: 100 });
      assert.equal(crop.readUInt32BE(16), 200, 'cropped width');
      assert.equal(crop.readUInt32BE(20), 100, 'cropped height');
      await b.close();
    } finally {
      await s.close();
    }
  });
}

t('invalid computer-control input is rejected', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    const r = (action, body) =>
      fetch(`http://${li.addr}/v1/sessions/${s.id}/computer/${action}`, {
        method: 'POST',
        body: JSON.stringify(body),
      });
    assert.equal((await r('click', { x: -5, y: 0 })).status, 400);
    assert.equal((await r('key', { keys: ['ctrl+a; rm -rf /'] })).status, 400);
    assert.equal((await r('nope', {})).status, 404);
  } finally {
    await s.close();
  }
});

t('kiosk mode hides browser chrome (chromium only)', async () => {
  await assert.rejects(li.create({ browser: 'firefox', kiosk: true }), /only supported for chromium/);
  const s = await li.create({ kiosk: true, ttlSeconds: 300 });
  try {
    const b = await s.connect(pw);
    const p = await page(b);
    await p.goto('data:text/html,<title>k</title>');
    const chrome = await p.evaluate(() => window.outerHeight - window.innerHeight);
    assert.ok(chrome < 5, `no tab strip or address bar in kiosk mode (chrome height ${chrome})`);
    await b.close();
  } finally {
    await s.close();
  }
});

t('multiple recordings per session, listed and downloadable by id', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    const a = await s.startRecording();
    assert.match(a.replay_id, /^[0-9a-f]{8}$/);
    await assert.rejects(s.startRecording(), /already running/);
    await new Promise((r) => setTimeout(r, 1500));
    assert.equal((await s.stopRecording()).replay_id, a.replay_id);
    const b = await s.startRecording();
    await new Promise((r) => setTimeout(r, 1500));
    await s.stopRecording({ replayId: b.replay_id });

    const list = await s.recordings();
    assert.deepEqual(list.map((r) => r.replay_id), [a.replay_id, b.replay_id]);
    assert.ok(list.every((r) => !r.running && r.size > 0));
    for (const id of [a.replay_id, b.replay_id]) {
      assert.equal((await s.downloadRecording(id)).subarray(4, 8).toString(), 'ftyp');
    }
    assert.deepEqual(await s.downloadRecording(), await s.downloadRecording(b.replay_id), 'default is the latest');
    await assert.rejects(s.stopRecording(), /no running recording/);
  } finally {
    await s.close();
  }
});

t('exec runs as the browser user, reports exit codes, and enforces timeouts', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    const hi = await s.exec(['echo', 'hi']);
    assert.deepEqual([hi.exit_code, hi.stdout], [0, 'hi\n']);
    assert.equal((await s.exec(['id', '-un'])).stdout.trim(), 'neko');
    assert.equal((await s.exec(['sh', '-c', 'exit 3'])).exit_code, 3);
    const err = await s.exec(['sh', '-c', 'echo oops >&2; exit 1']);
    assert.deepEqual([err.stderr.trim(), err.exit_code], ['oops', 1]);
    assert.equal((await s.exec(['pwd'], { cwd: '/tmp' })).stdout.trim(), '/tmp');
    const slow = await s.exec(['sleep', '20'], { timeoutSeconds: 1 });
    assert.equal(slow.timed_out, true);
    await assert.rejects(s.exec([]), /must not be empty/);
    await assert.rejects(s.exec(['true'], { timeoutSeconds: 9999 }), /timeout_seconds/);
  } finally {
    await s.close();
  }
});

t('file upload, download and listing round-trip binary data', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    const bytes = Buffer.from(Array.from({ length: 5000 }, (_, i) => (i * 7) % 256));
    const dir = '/home/neko/Downloads/e2e/nested';
    assert.equal((await s.uploadFile(`${dir}/data.bin`, bytes)).size, 5000);
    assert.deepEqual(await s.downloadFile(`${dir}/data.bin`), bytes, 'binary round trip');

    const [f] = await s.listFiles(dir);
    assert.deepEqual([f.name, f.type, f.size], ['data.bin', 'file', 5000]);
    assert.equal((await s.exec(['stat', '-c', '%U', `${dir}/data.bin`])).stdout.trim(), 'neko', 'owned by browser user');

    await assert.rejects(s.uploadFile('relative.txt', 'x'), /absolute/);
    await assert.rejects(s.downloadFile(`${dir}/missing`), /no such file/);
    await assert.rejects(s.downloadFile(dir), /not a regular file/);
  } finally {
    await s.close();
  }
});

// Minimal zip writer (stored, no compression) so the tests need no dependency.
import { crc32 } from 'node:zlib';
function makeZip(files) {
  const parts = [], central = [];
  let offset = 0;
  for (const [name, content] of Object.entries(files)) {
    const nameBuf = Buffer.from(name), data = Buffer.from(content), crc = crc32(data);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0); local.writeUInt16LE(20, 4);
    local.writeUInt32LE(crc, 14); local.writeUInt32LE(data.length, 18); local.writeUInt32LE(data.length, 22);
    local.writeUInt16LE(nameBuf.length, 26);
    const cen = Buffer.alloc(46);
    cen.writeUInt32LE(0x02014b50, 0); cen.writeUInt16LE(20, 4); cen.writeUInt16LE(20, 6);
    cen.writeUInt32LE(crc, 16); cen.writeUInt32LE(data.length, 20); cen.writeUInt32LE(data.length, 24);
    cen.writeUInt16LE(nameBuf.length, 28); cen.writeUInt32LE(offset, 42);
    parts.push(local, nameBuf, data);
    central.push(cen, nameBuf);
    offset += 30 + nameBuf.length + data.length;
  }
  const cd = Buffer.concat(central);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(central.length / 2, 8); end.writeUInt16LE(central.length / 2, 10);
  end.writeUInt32LE(cd.length, 12); end.writeUInt32LE(offset, 16);
  return Buffer.concat([...parts, cd, end]);
}

const demoExtension = (title = 'Demo') =>
  makeZip({
    'manifest.json': JSON.stringify({
      manifest_version: 3, name: title, version: '1.0',
      background: { service_worker: 'sw.js' },
      content_scripts: [{ matches: ['https://example.com/*'], js: ['cs.js'] }],
    }),
    'sw.js': 'console.log("sw")',
    'cs.js': 'document.documentElement.dataset.liExt = "loaded"',
  });

t('extensions: upload, load into a session, replace and delete', async () => {
  const name = `e2e-ext-${Date.now().toString(36)}`;
  try {
    const up = await li.uploadExtension(name, demoExtension('First'));
    assert.deepEqual([up.name, up.title, up.version], [name, 'First', '1.0']);
    assert.ok((await li.extensions()).some((e) => e.name === name));

    const s = await li.create({ extensions: [name], ttlSeconds: 300 });
    try {
      assert.deepEqual(s.extensions, [name]);
      // The extension's service worker shows up as a CDP target...
      const targets = await (await fetch(`${s.connectUrl}/json/list`)).json();
      assert.ok(targets.some((x) => x.type === 'service_worker' && x.url.startsWith('chrome-extension://')), 'service worker running');
      // ...and its content script runs on matching pages.
      const b = await s.connect(pw);
      const p = await page(b);
      await p.goto('https://example.com');
      await p.waitForFunction(() => document.documentElement.dataset.liExt === 'loaded', null, { timeout: 15000 });
      await b.close();

      await assert.rejects(li.deleteExtension(name), /in use|conflict|volume/i, 'cannot delete while in use');
    } finally {
      await s.close();
    }
    assert.equal((await li.uploadExtension(name, demoExtension('Second'))).title, 'Second', 'replaced');
  } finally {
    await li.deleteExtension(name).catch(() => {});
  }
  assert.ok(!(await li.extensions()).some((e) => e.name === name), 'deleted');
});

t('extensions: bad uploads and bad requests are rejected', async () => {
  await assert.rejects(li.uploadExtension('bad', Buffer.from('not a zip')), /valid zip/);
  await assert.rejects(li.uploadExtension('bad', makeZip({ 'a.js': 'x' })), /manifest\.json not found/);
  await assert.rejects(li.uploadExtension('Bad Name!', demoExtension()), /invalid extension name/);
  await assert.rejects(li.create({ extensions: ['does-not-exist'] }), /not found/);
  await assert.rejects(li.create({ browser: 'firefox', extensions: ['x'] }), /only supported for chromium/);
});
