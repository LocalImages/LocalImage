// The daemon must not be usable by web pages in the developer's own browser, by other users, or
// with oversized input. Needs a running daemon with authentication on (the default).
import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawnSync } from 'node:child_process';
import { existsSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { homedir, platform } from 'node:os';
import { join } from 'node:path';
import { LocalImage } from '../src/index.js';

const li = new LocalImage();
const authOn = await fetch(`http://${li.addr}/v1/info`).then((r) => r.status === 401, () => false);
const t = (name, fn) => test(name, { skip: !authOn && 'no daemon with authentication running', timeout: 120_000 }, fn);

const [host, port] = li.addr.split(':');
// fetch() refuses to set Host, so use http.request for the rebinding case.
const raw = (method, path, { headers = {}, body } = {}) =>
  new Promise((resolve, reject) => {
    const req = http.request({ host, port, method, path, headers }, (res) => {
      let text = '';
      res.on('data', (d) => (text += d));
      res.on('end', () => resolve({ status: res.statusCode, body: text ? JSON.parse(text) : null }));
    });
    req.on('error', reject);
    req.end(body);
  });
const key = { Authorization: `Bearer ${li.apiKey}` };

t('authentication: no key or a wrong key is refused; the SDK finds the key by itself', async () => {
  const none = await raw('GET', '/v1/info');
  assert.deepEqual([none.status, none.body.code], [401, 'unauthorized']);
  const wrong = await raw('GET', '/v1/info', { headers: { Authorization: 'Bearer not-the-key' } });
  assert.equal(wrong.status, 401);
  assert.equal((await li.info()).apiVersion, 1, 'the SDK reads the key file with no configuration');
  assert.equal((await fetch(`http://${li.addr}/healthz`)).status, 200, 'liveness stays open');
});

t('the key file is private to the user', async () => {
  if (platform() === 'win32') return;
  const base = process.env.XDG_CONFIG_HOME || (platform() === 'darwin' ? join(homedir(), 'Library', 'Application Support') : join(homedir(), '.config'));
  const mode = statSync(join(base, 'localimage', 'api-key')).mode & 0o777;
  assert.equal(mode, 0o600, `mode is ${mode.toString(8)}`);
});

t('a web page cannot call the daemon: cross-site origins, rebinding hosts and forged content types', async () => {
  // Even with a valid key (a page could not have it), these are refused before anything runs.
  const origin = await raw('POST', '/v1/sessions', { headers: { ...key, Origin: 'http://evil.example', 'Content-Type': 'text/plain' }, body: '{"ttl_seconds":30}' });
  assert.deepEqual([origin.status, origin.body.code], [403, 'forbidden']);

  const rebinding = await raw('GET', '/v1/info', { headers: { ...key, Host: 'attacker.test:7777' } });
  assert.deepEqual([rebinding.status, rebinding.body.code], [403, 'forbidden']);

  const shutdown = await raw('POST', '/v1/shutdown', { headers: { Origin: 'http://evil.example' } });
  assert.equal(shutdown.status, 403);
  assert.equal((await li.info()).apiVersion, 1, 'the daemon is still running');

  const forged = await raw('POST', '/v1/sessions', { headers: { ...key, 'Content-Type': 'text/plain' }, body: '{"ttl_seconds":30}' });
  assert.equal(forged.status, 415, 'a form-style content type cannot carry a JSON command');
  assert.equal((await li.list()).filter((s) => s.tags?.attacker).length, 0);

  const opaque = await raw('GET', '/v1/info', { headers: { ...key, Origin: 'null' } });
  assert.equal(opaque.status, 403, 'sandboxed pages (opaque origin) are refused too');
});

t('input limits: an oversized JSON body is refused, not buffered', async () => {
  const big = JSON.stringify({ tags: { k: 'x'.repeat(2 * 1024 * 1024) } });
  const r = await raw('POST', '/v1/sessions', { headers: { ...key, 'Content-Type': 'application/json' }, body: big });
  assert.equal(r.status, 413);
  assert.equal(r.body.code, 'invalid_request');
  assert.equal((await li.list()).length >= 0, true, 'and the daemon is fine');
});

t('malformed JSON is a clear 400, not a crash', async () => {
  const r = await raw('POST', '/v1/sessions', { headers: { ...key, 'Content-Type': 'application/json' }, body: '{not json' });
  assert.deepEqual([r.status, r.body.code], [400, 'invalid_request']);
});

const BIN = process.env.LOCALIMAGE_BIN ?? fileURLToPath(new URL('../../../bin/localimage', import.meta.url));

test('the daemon refuses to start unprotected if it cannot set up its key, and the opt-out is explicit', { skip: !existsSync(BIN) && 'no binary', timeout: 30_000 }, async () => {
  // No home or config directory (a minimal container, a service): it must stop, not run open.
  const r = spawnSync(BIN, ['start', '-addr', '127.0.0.1:7786'], { encoding: 'utf8', env: { PATH: process.env.PATH }, timeout: 15_000 });
  assert.equal(r.status, 1, `exit ${r.status}: ${r.stderr}`);
  assert.match(r.stderr, /cannot set up the API key file/);
  assert.match(r.stderr, /LOCALIMAGE_API_KEY.*LOCALIMAGE_NO_AUTH=1/s, 'and it says how to proceed');
  assert.equal(await fetch('http://127.0.0.1:7786/healthz').then(() => true, () => false), false, 'nothing is listening');
});
