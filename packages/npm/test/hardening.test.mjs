// Browser crash visibility, WebGL, timezone and locale, recording limits, changeable tags, image
// pruning and saved logs. Needs Docker and a running daemon.
import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import * as pw from 'playwright-core';
import { LocalImage } from '../src/index.js';

const li = new LocalImage();
const daemonUp = await li.list().then(() => true, () => false);
const t = (name, fn) => test(name, { skip: !daemonUp && 'daemon not running', timeout: 300_000 }, fn);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const errorOf = (p) => p.then(() => null, (e) => e);
const pageOf = async (b) => b.contexts()[0].pages()[0] ?? b.contexts()[0].newPage();

t('a browser crash is visible: restarts goes up, health recovers, and the URL keeps working', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    const before = await s.health();
    assert.deepEqual([before.starts, before.restarts, before.state], [1, 0, 'running']);
    assert.ok(before.last_started_at_ms > 0);

    const b = await s.connect(pw);
    let dropped = false;
    b.on('disconnected', () => (dropped = true));
    const id = async () => (await (await fetch(`${s.connectUrl}/json/version`)).json()).webSocketDebuggerUrl.split('/').pop();
    const idBefore = await id();

    await s.exec(['sh', '-c', 'for p in $(pidof chromium); do kill -9 $p; done']); // a real crash
    let back = false;
    for (let i = 0; i < 100 && !back; i++) {
      await sleep(200);
      back = await fetch(`${s.connectUrl}/json/version`, { signal: AbortSignal.timeout(1000) }).then((r) => r.ok, () => false);
    }
    assert.ok(back, 'the same URL answers again');
    assert.notEqual(await id(), idBefore, 'but it is a new, empty browser');
    assert.equal(dropped, true, 'the client saw the connection drop');

    const after = await s.health();
    assert.deepEqual([after.starts, after.restarts], [2, 1], 'the restart is counted');
    assert.ok(after.last_started_at_ms > before.last_started_at_ms);

    // Docker's health check covers the browser, so it settles back to healthy.
    let healthy = '';
    for (let i = 0; i < 40 && healthy !== 'healthy'; i++) {
      healthy = (await li.list()).find((x) => x.id === s.id)?.health ?? '';
      if (healthy !== 'healthy') await sleep(1000);
    }
    assert.equal(healthy, 'healthy', 'list() reports health');
  } finally {
    await s.close();
  }
});

t('WebGL is off by default and on with webgl: true; timezone and locale can be set', async () => {
  const probe = async (opts) => {
    const s = await li.create({ ttlSeconds: 300, ...opts });
    try {
      const b = await s.connect(pw);
      const page = await pageOf(b);
      await page.goto('data:text/html,<canvas id=c></canvas>');
      const r = await page.evaluate(() => ({
        webgl: !!document.getElementById('c').getContext('webgl'),
        tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
        lang: navigator.language,
        offset: new Date(2026, 0, 15, 12).getTimezoneOffset(),
      }));
      await b.close();
      return r;
    } finally {
      await s.close();
    }
  };
  const plain = await probe({});
  assert.deepEqual([plain.webgl, plain.tz, plain.lang], [false, 'UTC', 'en-US']);
  const custom = await probe({ webgl: true, timezone: 'Asia/Kolkata', locale: 'de-DE' });
  assert.equal(custom.webgl, true, 'WebGL works with webgl: true');
  assert.match(custom.tz, /^Asia\/(Kolkata|Calcutta)$/, 'Chromium may report the legacy alias of the same zone');
  assert.equal(custom.offset, -330, 'UTC+5:30');
  assert.equal(custom.lang, 'de-DE');

  for (const bad of [{ timezone: 'Not a zone!' }, { locale: 'x' }, { locale: 'en_US' }]) {
    assert.equal((await errorOf(li.create(bad)))?.code, 'invalid_request', JSON.stringify(bad));
  }
  assert.equal((await errorOf(li.create({ browser: 'firefox', webgl: true })))?.code, 'invalid_request');
});

t('a recording stops by itself at its time limit, and a bad limit is refused', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  try {
    assert.equal((await errorOf(s.startRecording({ maxSeconds: 999999 })))?.code, 'invalid_request');
    const started = await s.startRecording({ maxSeconds: 2 });
    await sleep(5000);
    const [r] = await s.recordings();
    assert.equal(r.replay_id, started.replay_id);
    assert.deepEqual([r.running, r.status], [false, 'stopped'], 'it ended without a stop call');
    assert.ok(r.stopped_at_ms > 0 && r.duration_ms >= 1500 && r.duration_ms <= 3500, `duration ${r.duration_ms} ms`);
    assert.equal((await s.downloadRecording(r.replay_id)).subarray(4, 8).toString(), 'ftyp', 'and the file is valid');
    // The limit freed the slot: a new recording can start.
    await s.startRecording({ maxSeconds: 1 });
  } finally {
    await s.close();
  }
});

t('tags can be changed after creation, and filters, list and attach see the change', async () => {
  const s = await li.create({ ttlSeconds: 300, tags: { run: 'r1', stage: 'running', old: 'x' } });
  try {
    const result = await s.updateTags({ set: { stage: 'failed', note: 'timeout' }, remove: ['old'] });
    assert.deepEqual(result, { run: 'r1', stage: 'failed', note: 'timeout' });
    assert.deepEqual(s.tags, result, 'the session object is updated');

    const ids = async (tags) => (await li.list({ tags })).map((x) => x.id);
    assert.deepEqual(await ids({ stage: 'failed' }), [s.id], 'filters use the current tags');
    assert.deepEqual(await ids({ stage: 'running' }), [], 'the old value no longer matches');
    assert.deepEqual(await ids({ old: 'x' }), [], 'a removed tag no longer matches');
    assert.deepEqual((await li.attach(s.id)).tags, result, 'attach sees the change');

    await s.updateTags({ set: { stage: 'running' } }); // changes stack, and a tag can be restored
    assert.deepEqual((await li.attach(s.id)).tags, { run: 'r1', stage: 'running', note: 'timeout' });
    await s.updateTags({ set: { old: 'back' } });
    assert.equal((await li.attach(s.id)).tags.old, 'back');

    assert.equal((await errorOf(s.updateTags({ set: { 'bad key': 'v' } })))?.code, 'invalid_request');
    const many = Object.fromEntries(Array.from({ length: 21 }, (_, i) => [`k${i}`, 'v']));
    assert.equal((await errorOf(s.updateTags({ set: many })))?.code, 'invalid_request', 'the limit of 20 holds');
  } finally {
    await s.close();
  }
  assert.equal((await errorOf(li.attach('ffffffff').then((x) => x.updateTags({ set: { a: 'b' } }))))?.code, 'session_gone');
});

t('prune removes images from older versions, and only those', async () => {
  const s = await li.create({ ttlSeconds: 300 });
  const current = execFileSync('docker', ['inspect', `localimage-${s.id}`, '--format', '{{.Config.Image}}']).toString().trim();
  const stale = current.replace(/:.*$/, ':r0-old-test');
  execFileSync('docker', ['tag', current, stale]);
  try {
    assert.ok((await li.info()).staleImages >= 1, 'the old image is counted');
    const removed = await li.prune();
    assert.ok(removed.includes(stale), `removed ${removed}`);
    assert.ok(!removed.includes(current), 'the current image is untouched');
    assert.equal((await li.info()).images.chromium, true);
    assert.equal(execFileSync('docker', ['images', '-q', stale]).toString().trim(), '', 'it is gone from disk');
    assert.equal((await li.info()).staleImages, 0);
  } finally {
    await s.close();
    try { execFileSync('docker', ['rmi', stale], { stdio: 'ignore' }); } catch { /* already pruned */ }
  }
});

t('saveLogs keeps a session\'s logs after it is gone', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'li-logs-'));
  const s = await li.create({ ttlSeconds: 300 });
  let files;
  try {
    files = await s.saveLogs(dir);
  } finally {
    await s.close();
  }
  assert.ok(files.length >= 3, `wrote ${files.length} files`);
  assert.ok(files.every(existsSync));
  assert.ok(files.some((f) => f.endsWith('chromium.log')) && files.some((f) => f.endsWith('container.log')));
  assert.ok(readFileSync(files.find((f) => f.endsWith('chromium.log')), 'utf8').length > 0, 'and they survive close()');
});
