// Daemon lifecycle, tags, idempotent release and concurrency. These tests start their own daemon
// on a separate port (through ensureDaemon, which is itself under test) with a session limit of 3,
// so they do not touch the daemon the other test files use. Needs Docker. Run: npm test
import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { LocalImage, LocalImageError, ensureDaemon } from '../src/index.js';

// In a checkout there is no @localimage platform package; use the binary built at the repo root.
const built = fileURLToPath(new URL('../../../bin/localimage', import.meta.url));
if (!process.env.LOCALIMAGE_BIN && existsSync(built)) process.env.LOCALIMAGE_BIN = built;

const ADDR = '127.0.0.1:7791';
const MAX = 3;
const env = { LOCALIMAGE_MAX_SESSIONS: String(MAX) };
const li = new LocalImage({ addr: ADDR });

const dockerOk = (() => {
  try {
    execFileSync('docker', ['version', '--format', '{{.Server.Version}}'], { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
})();
const skip = dockerOk ? false : 'Docker is not available';
const t = (name, fn) => test(name, { skip, timeout: 400_000 }, fn);

const up = () => fetch(`http://${ADDR}/healthz`, { signal: AbortSignal.timeout(1500) }).then((r) => r.ok, () => false);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const errorOf = (p) => p.then(() => null, (e) => e);
// Every test that needs the daemon makes sure it is up, so tests do not depend on running in order.
const daemon = () => ensureDaemon({ addr: ADDR, env });

// Stop the daemon these tests started, and every session left behind.
after(async () => {
  if (!(await up())) return;
  for (const s of await li.list()) await li.delete(s.id).catch(() => {});
  await li.shutdown().catch(() => {});
});

t('errors have stable codes: an unreachable daemon is daemon_unreachable', async () => {
  const dead = new LocalImage({ addr: '127.0.0.1:1' });
  const e = await errorOf(dead.list());
  assert.ok(e instanceof LocalImageError, 'a LocalImageError');
  assert.equal(e.code, 'daemon_unreachable');
  assert.match(e.message, /ensureDaemon|autoStart|localimage start/);
  // release against a dead daemon is a real failure, not "gone"
  assert.equal((await errorOf(dead.delete('abcd1234'))).code, 'daemon_unreachable');
});

t('ensureDaemon starts a daemon, waits for it, and reuses a running one', async () => {
  assert.equal(await up(), false, `port ${ADDR} is already in use: stop the daemon there before running this test`);

  const a = await ensureDaemon({ addr: ADDR, env });
  assert.equal(a.started, true);
  assert.ok(Number.isInteger(a.pid) && a.pid > 0, 'pid');
  assert.ok(a.logFile, 'log file');
  assert.equal(await up(), true, 'answers /healthz as soon as it resolves');

  const b = await ensureDaemon({ addr: ADDR, env });
  assert.deepEqual([b.started, b.addr], [false, ADDR], 'a running daemon is reused');

  // Racing callers must end up sharing one daemon.
  const many = await Promise.all([1, 2, 3].map(() => ensureDaemon({ addr: ADDR, env })));
  assert.ok(many.every((r) => r.started === false));

  const info = await li.info();
  assert.equal(info.maxSessions, MAX, 'the env reached the daemon');
  assert.equal(info.running, 0, 'no sessions running: another test left some behind?');
  assert.equal(info.available, MAX);
});

t('autoStart brings a stopped daemon back transparently, and shutdown is idempotent', async () => {
  assert.equal(await li.shutdown(), true, 'stops the running daemon');
  for (let i = 0; i < 50 && (await up()); i++) await sleep(100);
  assert.equal(await up(), false, 'daemon is down');
  assert.equal(await li.shutdown(), false, 'stopping a stopped daemon is a no-op');

  // Three independent clients race to start the daemon: they must end up sharing one.
  const clients = [1, 2, 3].map(() => new LocalImage({ addr: ADDR, autoStart: true, daemonEnv: env }));
  const infos = await Promise.all(clients.map((c) => c.info()));
  assert.ok(infos.every((i) => i.maxSessions === MAX));
  assert.equal(await up(), true);
  // Concurrent calls on one client share its single start-up.
  await li.shutdown();
  for (let i = 0; i < 50 && (await up()); i++) await sleep(100);
  const one = new LocalImage({ addr: ADDR, autoStart: true, daemonEnv: env });
  assert.ok((await Promise.all([one.info(), one.info(), one.info()])).every((i) => i.maxSessions === MAX));

  // Without autoStart the same situation is a clear, stable error.
  await li.shutdown();
  for (let i = 0; i < 50 && (await up()); i++) await sleep(100);
  assert.equal((await errorOf(li.info())).code, 'daemon_unreachable');
  await ensureDaemon({ addr: ADDR, env });
});

t('tags: stored on the session, returned by list, usable as filters; sweep finds leftovers', async () => {
  await daemon();
  const mk = (run, target) => li.create({ ttlSeconds: 600, tags: { e2e_run: run, e2e_target: target } });
  const [a, b, c] = await Promise.all([mk('runA', 't1'), mk('runA', 't2'), mk('runB', 't1')]);
  try {
    assert.deepEqual(a.tags, { e2e_run: 'runA', e2e_target: 't1' }, 'returned by create');

    const ids = async (tags) => (await li.list({ tags })).map((s) => s.id).sort();
    assert.deepEqual(await ids({ e2e_run: 'runA' }), [a.id, b.id].sort());
    assert.deepEqual(await ids({ e2e_run: 'runA', e2e_target: 't1' }), [a.id], 'tags are ANDed');
    assert.deepEqual(await ids({ e2e_target: 't1' }), [a.id, c.id].sort());
    assert.deepEqual(await ids({ e2e_run: 'nope' }), []);
    assert.equal((await li.list()).length, 3, 'no filter lists everything');

    const [listed] = await li.list({ tags: { e2e_run: 'runB' } });
    assert.equal(listed.id, c.id);
    assert.deepEqual(listed.tags, { e2e_run: 'runB', e2e_target: 't1' }, 'tags come back from list');
    assert.equal(listed.state, 'running');
    assert.ok(Date.parse(listed.created_at) > 0 && Date.parse(listed.expires_at) > Date.now());

    // A worker dies and its browser container is left stopped. The sweep must still find it.
    execFileSync('docker', ['stop', '-t', '1', `localimage-${a.id}`], { stdio: 'ignore' });
    const swept = await li.list({ tags: { e2e_run: 'runA' } });
    assert.deepEqual(swept.map((s) => s.id).sort(), [a.id, b.id].sort(), 'stopped sessions are listed');
    assert.equal(swept.find((s) => s.id === a.id).state, 'exited');

    const results = await Promise.all(swept.map((s) => li.delete(s.id)));
    assert.deepEqual(results, [true, true]);
    assert.deepEqual(await ids({ e2e_run: 'runA' }), [], 'sweep removed every runA session');
    assert.deepEqual(await ids({ e2e_run: 'runB' }), [c.id], 'and left the other run alone');
  } finally {
    await Promise.all([a, b, c].map((s) => s.close()));
  }
});

t('tags are validated', async () => {
  await daemon();
  for (const bad of [{ 'bad key': 'x' }, { '': 'x' }, { 'a/b': 'x' }, { k: 'x'.repeat(257) }, { k: 'line\nbreak' }]) {
    const e = await errorOf(li.create({ tags: bad }));
    assert.equal(e?.code, 'invalid_request', `tags ${JSON.stringify(bad).slice(0, 40)}: ${e?.message}`);
  }
  const tooMany = Object.fromEntries(Array.from({ length: 21 }, (_, i) => [`k${i}`, 'v']));
  assert.equal((await errorOf(li.create({ tags: tooMany })))?.code, 'invalid_request');
  assert.equal((await errorOf(li.list({ tags: { 'a b': 'x' } })))?.code ?? 'ok', 'ok', 'list filters never error on odd keys');
  assert.deepEqual((await li.list()).length, 0, 'a rejected create leaves nothing behind');
});

t('delete is idempotent: releasing twice, concurrently, or after a failed acquire never throws', async () => {
  await daemon();
  const s = await li.create({ ttlSeconds: 300 });
  assert.equal(await s.close(), true, 'first close removes it');
  assert.equal(await s.close(), false, 'second close: already gone');
  assert.equal(await li.delete(s.id), false);
  assert.equal(await li.delete('ffffffff'), false, 'never existed');

  const s2 = await li.create({ ttlSeconds: 300 });
  const both = await Promise.all([s2.close(), s2.close()]);
  assert.deepEqual([...both].sort(), [false, true], 'racing deletes: exactly one removes it, neither throws');

  // The raw API says the same thing with a stable code.
  const r = await fetch(`http://${ADDR}/v1/sessions/${s2.id}`, { method: 'DELETE', headers: { Authorization: `Bearer ${li.apiKey}` } });
  assert.equal(r.status, 404);
  assert.equal((await r.json()).code, 'session_gone');
});

t('concurrency: the limit is readable, enforced under a burst, and can queue', async () => {
  await daemon();
  // Read the limit and fail early.
  const info = await li.info();
  assert.deepEqual([info.maxSessions, info.available], [MAX, MAX]);
  assert.equal((await li.assertCapacity(MAX)).maxSessions, MAX);
  const tooBig = await errorOf(li.assertCapacity(MAX + 1));
  assert.equal(tooBig.code, 'limit_exceeded');
  assert.deepEqual(tooBig.details, { max: MAX, requested: MAX + 1 });
  assert.match(tooBig.message, /LOCALIMAGE_MAX_SESSIONS=4/);

  const held = [];
  try {
    // A burst larger than the limit: exactly MAX succeed and the rest are refused promptly.
    const t0 = Date.now();
    const burst = await Promise.allSettled(
      Array.from({ length: MAX + 3 }, async () => {
        try {
          return await li.create({ ttlSeconds: 600 });
        } catch (e) {
          e.at = Date.now() - t0;
          throw e;
        }
      }),
    );
    const ok = burst.filter((r) => r.status === 'fulfilled').map((r) => r.value);
    held.push(...ok);
    const refused = burst.filter((r) => r.status === 'rejected').map((r) => r.reason);
    assert.equal(ok.length, MAX, `exactly ${MAX} sessions were granted`);
    assert.equal(refused.length, 3);
    for (const e of refused) {
      assert.equal(e.code, 'limit_exceeded');
      assert.equal(e.status, 429);
      assert.equal(e.details.max, MAX);
      assert.ok(e.at < 20_000, `refusal came back in ${e.at} ms, not a timeout`);
    }
    assert.equal((await li.info()).available, 0);

    // wait: queue for a slot, but give up cleanly when none frees up.
    const t1 = Date.now();
    const timedOut = await errorOf(li.create({ waitSeconds: 2 }));
    assert.equal(timedOut.code, 'limit_exceeded');
    assert.ok(Date.now() - t1 >= 1800, 'it did wait');

    // A queued create gets the slot as soon as one is released.
    const queued = li.create({ ttlSeconds: 600, waitSeconds: 120 });
    let settled = false;
    queued.then(() => (settled = true), () => (settled = true));
    await sleep(2500);
    assert.equal(settled, false, 'still queued while every slot is taken');
    await held.shift().close();
    const got = await queued;
    held.push(got);
    assert.ok(got.id, 'the queued create succeeded once a slot freed');
    assert.equal((await li.info()).available, 0);

    // The ttl cap is a limit_exceeded too.
    const long = await errorOf(li.create({ ttlSeconds: 10 ** 7 }));
    assert.equal(long.code, 'limit_exceeded');
    assert.match(long.message, /ttl_seconds/);

    // Invalid options do not consume a slot and are not reported as limits.
    assert.equal((await errorOf(li.create({ browser: 'netscape' }))).code, 'invalid_request');
    assert.equal((await errorOf(li.create({ waitSeconds: 9999 }))).code, 'invalid_request');
  } finally {
    await Promise.all(held.map((s) => s.close()));
  }
  assert.equal((await li.info()).available, MAX, 'every slot is returned');
});

t('ensureDaemon reports why it could not start: Docker down, or an unrunnable binary', async () => {
  // The daemon refuses to start without Docker; the provider gets a stable code and the reason.
  const dockerDown = await errorOf(
    ensureDaemon({ addr: '127.0.0.1:7792', timeoutMs: 15_000, env: { DOCKER_HOST: 'unix:///nonexistent-docker.sock' } }),
  );
  assert.ok(dockerDown instanceof LocalImageError);
  assert.equal(dockerDown.code, 'docker_unavailable');
  assert.match(dockerDown.message, /Docker is not available/);
  assert.match(dockerDown.message, /nonexistent-docker\.sock/, 'the log tail explains the cause');
  assert.equal(await fetch('http://127.0.0.1:7792/healthz').then(() => true, () => false), false, 'nothing was left running');

  // A binary that cannot be run is daemon_start_failed.
  const saved = process.env.LOCALIMAGE_BIN;
  process.env.LOCALIMAGE_BIN = '/nonexistent/localimage';
  try {
    const e = await errorOf(ensureDaemon({ addr: '127.0.0.1:7793', timeoutMs: 5_000 }));
    assert.equal(e?.code, 'daemon_start_failed');
  } finally {
    process.env.LOCALIMAGE_BIN = saved;
  }
});

// A lease is plain JSON handed between processes (an environment variable, in a real engine), so
// it must not depend on anything in the process that created it.
const FIXTURES = fileURLToPath(new URL('./fixtures/', import.meta.url));
const fixture = (file, ...args) =>
  spawnSync('node', [FIXTURES + file, ...args], { encoding: 'utf8', timeout: 120_000, env: { ...process.env, LOCALIMAGE_ADDR: ADDR } });
const parseLast = (out) => JSON.parse(out.trim().split('\n').pop());

for (const mode of ['graceful', 'abrupt']) {
  t(`reconnect: a lease reattaches from another process after a ${mode} drop, keeping the page`, async () => {
    await daemon();
    const a = fixture('holder.mjs', mode, 'chromium');
    const lease = parseLast(a.stdout);
    assert.equal(a.status, mode === 'graceful' ? 0 : 3);
    try {
      assert.ok((await li.list()).some((s) => s.id === lease.id && s.state === 'running'), 'the session outlives its first connection');
      const b = parseLast(fixture('attach.mjs', JSON.stringify(lease)).stdout);
      assert.deepEqual([b.ok, b.marker], [true, 'A-was-here'], 'the same page, with its JavaScript state');
      assert.ok(b.after > b.before, 'and it is still running');
    } finally {
      await li.delete(lease.id);
    }
  });
}

t('reconnect: the same lease keeps working through a restarted daemon', async () => {
  await daemon();
  const lease = parseLast(fixture('holder.mjs', 'abrupt', 'chromium').stdout);
  try {
    assert.equal(parseLast(fixture('attach.mjs', JSON.stringify(lease)).stdout).ok, true);
    await li.shutdown();
    for (let i = 0; i < 50 && (await up()); i++) await sleep(100);
    assert.equal((await ensureDaemon({ addr: ADDR, env })).started, true, 'a new daemon process');
    const c = parseLast(fixture('attach.mjs', JSON.stringify(lease)).stdout);
    assert.deepEqual([c.ok, c.marker], [true, 'A-was-here'], 'the token URL needs nothing from the old daemon process');
  } finally {
    await li.delete(lease.id);
  }
});

t('reconnect: firefox sessions survive a drop, but pages opened by the old connection are closed', async () => {
  await daemon();
  const lease = parseLast(fixture('holder.mjs', 'abrupt', 'firefox').stdout);
  try {
    assert.ok((await li.list()).some((s) => s.id === lease.id && s.state === 'running'), 'the session is still running');
    const b = parseLast(fixture('attach.mjs', JSON.stringify(lease)).stdout);
    // Playwright's server tears down a connection's pages when it goes away, so unlike Chromium
    // (CDP) the new connection starts without them. Keep one connection for a Firefox attempt.
    assert.equal(b.ok, false);
    assert.deepEqual(b.pages, []);
  } finally {
    await li.delete(lease.id);
  }
});

t('ensureDaemon from several processes at once: one daemon, and only its starter says it started it', async () => {
  const addr = '127.0.0.1:7790';
  assert.equal(await fetch(`http://${addr}/healthz`).then(() => true, () => false), false, `port ${addr} is in use`);
  const { spawn } = await import('node:child_process');
  const results = await Promise.all(
    Array.from({ length: 6 }, () =>
      new Promise((resolve, reject) => {
        const p = spawn('node', [FIXTURES + 'ensure.mjs', addr], { env: process.env });
        let out = '';
        p.stdout.on('data', (d) => (out += d));
        p.on('close', (code) => (code === 0 ? resolve(JSON.parse(out.trim().split('\n').pop())) : reject(new Error(`exit ${code}: ${out}`))));
      }),
    ),
  );
  try {
    assert.equal(results.filter((r) => r.started).length, 1, `exactly one process owns the daemon: ${JSON.stringify(results)}`);
    const health = await (await fetch(`http://${addr}/healthz`)).json();
    assert.equal(results.find((r) => r.started).pid, health.pid, 'and its pid is the daemon that is answering');
  } finally {
    await new LocalImage({ addr }).shutdown();
  }
});

t('the daemon log records what happened, including refused requests, and never a session token', async () => {
  const log = await daemon();
  const { readFileSync } = await import('node:fs');
  const { tmpdir } = await import('node:os');
  const { join } = await import('node:path');
  const { request } = await import('node:http');
  const file = log.logFile ?? join(tmpdir(), 'localimage-daemon-127.0.0.1_7791.log');
  const read = () => readFileSync(file, 'utf8');

  const s = await li.create({ ttlSeconds: 300 });
  const token = s.connectUrl.split('/t/')[1].split('/')[0];
  // A cross-site request, and a request that reaches a session through its token path.
  await new Promise((resolve) => {
    const r = request({ host: '127.0.0.1', port: 7791, method: 'POST', path: `/t/${token}/cdp/json/version`, headers: { Origin: 'http://evil.example' } }, (res) => { res.resume(); res.on('end', resolve); });
    r.end();
  });
  await fetch(`${s.connectUrl}/json/version`);
  await s.close();
  await sleep(300);

  const text = read();
  assert.match(text, /msg="session created" id=/, 'creation is logged');
  assert.match(text, /msg="session deleted" id=/, 'deletion is logged');
  assert.match(text, /msg="request refused" reason="cross-origin/, 'a refused request is logged with its reason');
  assert.ok(text.includes('/t/<redacted>/cdp/json/version'), 'token paths are redacted');
  assert.ok(!text.includes(token), 'the session token never reaches the log');
});
