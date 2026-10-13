// Live view checks in a real browser: WebRTC video plays, the controller can
// control the browser, the read-only viewer cannot. Needs a daemon, Docker and
// a Chrome/Chromium binary (set CHROME_PATH, or it tries common locations).
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import * as pw from 'playwright-core';
import { LocalImage } from '../src/index.js';

const li = new LocalImage();
const daemonUp = await li.list().then(() => true, () => false);
const chrome = [process.env.CHROME_PATH, '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser']
  .find((p) => p && fs.existsSync(p));
const skip = !daemonUp ? 'daemon not running' : !chrome ? 'no Chrome/Chromium found (set CHROME_PATH)' : false;

const REMOTE_HTML = `<title>remote</title>
<body style="margin:0;background:#103060;color:#fff;font:bold 56px monospace">
<div id=t style="font-size:150px">0</div>
<div>clicks: <span id=c>0</span></div>
<button id=b style="position:absolute;left:520px;top:420px;width:380px;height:180px;font-size:56px"
        onclick="c.textContent=+c.textContent+1">CLICK ME</button>
<script>let n=0;setInterval(()=>{t.textContent=String(++n).padStart(5,'0')},100)</script></body>`;

// Open a live-view URL and wait until the video is decoding frames.
async function openView(browser, url) {
  const page = await (await browser.newContext({ viewport: { width: 1440, height: 900 } })).newPage();
  await page.goto(url);
  await page.waitForSelector('video', { timeout: 30_000 });
  const sample = () =>
    page.evaluate(() => {
      const v = document.querySelector('video');
      return { w: v.videoWidth, frames: v.getVideoPlaybackQuality().totalVideoFrames, t: v.currentTime, top: v.getBoundingClientRect().top };
    });
  let a = await sample();
  for (let i = 0; i < 20 && !(a.w > 0 && a.frames > 0); i++) {
    await page.waitForTimeout(500);
    a = await sample();
  }
  await page.waitForTimeout(3000);
  return { page, a, b: await sample() };
}

// Click where the remote button is, mapping remote screen coordinates onto the video.
async function clickRemote(page, point) {
  const g = await page.evaluate(() => {
    const v = document.querySelector('video');
    const r = v.getBoundingClientRect();
    return { x: r.x, y: r.y, w: r.width, h: r.height, vw: v.videoWidth, vh: v.videoHeight };
  });
  await page.mouse.move(g.x + point.x * (g.w / g.vw), g.y + point.y * (g.h / g.vh));
  await page.mouse.down();
  await page.mouse.up();
  await page.waitForTimeout(1200);
}

test('live view: video plays, controller controls, viewer is read-only', { skip, timeout: 300_000 }, async () => {
  const s = await li.create({ ttlSeconds: 600 });
  let viewer;
  try {
    const rb = await s.connect(pw);
    const rp = rb.contexts()[0].pages()[0] ?? (await rb.contexts()[0].newPage());
    await rp.goto('data:text/html,' + encodeURIComponent(REMOTE_HTML));
    await rp.bringToFront();
    await rp.waitForTimeout(800);
    const button = await rp.evaluate(() => {
      const r = document.getElementById('b').getBoundingClientRect();
      return {
        x: window.screenX + (window.outerWidth - window.innerWidth) / 2 + r.left + r.width / 2,
        y: window.screenY + (window.outerHeight - window.innerHeight) + r.top + r.height / 2,
      };
    });
    const clicks = () => rp.evaluate(() => +document.getElementById('c').textContent);

    viewer = await pw.chromium.launch({
      executablePath: chrome,
      headless: true,
      args: ['--autoplay-policy=no-user-gesture-required', '--no-sandbox'],
    });

    // Controller: video plays, nothing above it, and a click takes control and lands.
    const ctl = await openView(viewer, s.liveUrl);
    assert.ok(ctl.b.w > 0, 'video has dimensions');
    assert.ok(ctl.b.frames > ctl.a.frames + 10, `frames advance (${ctl.a.frames} -> ${ctl.b.frames})`);
    assert.ok(ctl.b.t > ctl.a.t + 1, 'playback time advances');
    assert.ok(ctl.b.top < 5, `no banner above the video (top = ${ctl.b.top}px)`);
    const shot1 = await ctl.page.locator('video').screenshot();
    await ctl.page.waitForTimeout(1200);
    assert.ok(!shot1.equals(await ctl.page.locator('video').screenshot()), 'picture changes over time');

    const before = await clicks();
    for (let i = 0; i < 3 && (await clicks()) === before; i++) await clickRemote(ctl.page, button);
    assert.ok((await clicks()) > before, 'controller click reaches the remote page');

    // Viewer: video plays, but clicks do nothing.
    const ro = await openView(viewer, s.viewUrl);
    assert.ok(ro.b.frames > ro.a.frames + 10, 'viewer video plays');
    const mid = await clicks();
    for (let i = 0; i < 3; i++) await clickRemote(ro.page, button);
    assert.equal(await clicks(), mid, 'viewer clicks are ignored');
    await rb.close();
  } finally {
    await viewer?.close();
    await s.close();
  }
});
