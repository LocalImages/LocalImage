// The command-line interface, driven as a user would: the real binary, every command, and its
// exit codes. Needs Docker and a running daemon.
import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { LocalImage } from '../src/index.js';

const BIN = process.env.LOCALIMAGE_BIN ?? fileURLToPath(new URL('../../../bin/localimage', import.meta.url));
const li = new LocalImage();
const daemonUp = await li.list().then(() => true, () => false);
const skip = !daemonUp ? 'daemon not running' : !existsSync(BIN) ? `no binary at ${BIN}` : false;
const t = (name, fn) => test(name, { skip, timeout: 300_000 }, fn);

const cli = (...args) => {
  const r = spawnSync(BIN, args, { encoding: 'utf8', timeout: 120_000, env: process.env });
  return { code: r.status, out: r.stdout, err: r.stderr };
};
const json = (r) => {
  assert.equal(r.code, 0, `exit ${r.code}: ${r.err}`);
  return JSON.parse(r.out);
};

t('help and version', () => {
  const help = cli('help');
  assert.equal(help.code, 0);
  for (const word of ['create', 'logs', 'download', 'upload', 'health', 'tag', 'prune', 'recordings', 'pull', 'doctor']) {
    assert.ok(help.out.includes(word), `help mentions ${word}`);
  }
  assert.equal(cli('--version').code, 0);
  assert.equal(cli('no-such-command').code, 2, 'an unknown command is a usage error');
  assert.match(cli('no-such-command').err, /usage|Usage/);
});

t('info, doctor and prune', () => {
  const info = json(cli('info'));
  assert.ok(info.max_sessions > 0 && info.api_version === 1 && info.components.chromium.version);
  const doctor = cli('doctor');
  assert.equal(doctor.code, 0, doctor.err);
  assert.match(doctor.out, /Docker/);
  const before = json(cli('info')).images; // a fresh machine (CI) may have no images built yet
  const first = json(cli('prune')).removed;
  assert.ok(Array.isArray(first));
  assert.deepEqual(json(cli('prune')).removed, [], 'pruning is idempotent: a second run finds nothing');
  assert.deepEqual(json(cli('info')).images, before, 'and the current images are untouched');
});

t('a session through its whole life, from the command line', async () => {
  const created = json(cli('create', '-ttl', '300', '-tag', 'suite=cli', '-tag', 'n=1', '-timezone', 'Europe/Berlin'));
  const id = created.id;
  try {
    assert.deepEqual(created.tags, { suite: 'cli', n: '1' });
    assert.match(created.connect_url, /\/cdp$/);

    // list, filter, get, health
    assert.ok(json(cli('ls', '-tag', 'suite=cli')).some((s) => s.id === id));
    assert.deepEqual(json(cli('ls', '-tag', 'suite=nope')), []);
    const got = json(cli('get', id));
    assert.deepEqual([got.id, got.state], [id, 'running']);
    assert.equal(json(cli('attach', id)).id, id, 'attach is an alias of get');
    const health = json(cli('health', id));
    assert.deepEqual([health.starts, health.restarts], [1, 0]);

    // exec and the timezone option took effect
    const ran = json(cli('exec', id, '--', 'sh', '-c', 'echo "$TZ"'));
    assert.deepEqual([ran.exit_code, ran.stdout.trim()], [0, 'Europe/Berlin']);

    // upload and download round trip
    const dir = mkdtempSync(join(tmpdir(), 'li-cli-'));
    const local = join(dir, 'in.bin');
    const payload = Buffer.from(Array.from({ length: 3000 }, (_, i) => (i * 11) % 256));
    writeFileSync(local, payload);
    const up = cli('upload', id, local, '/home/neko/Downloads/up.bin');
    assert.equal(up.code, 0, up.err);
    const back = join(dir, 'out.bin');
    assert.equal(cli('download', id, '/home/neko/Downloads/up.bin', '-o', back).code, 0);
    assert.deepEqual(readFileSync(back), payload, 'bytes survive the round trip');
    assert.equal(cli('download', id, '/home/neko/Downloads/missing', '-o', join(dir, 'x')).code, 1, 'a missing file is an error');

    // logs
    assert.ok(cli('logs', id).out.length > 0);
    assert.ok(cli('logs', id, '-source', 'chromium', '-tail', '5').out.length > 0);
    assert.equal(cli('logs', id, '-source', 'nonsense').code, 1);
    assert.match(cli('logs', id, '-source', 'nonsense').err, /invalid_request/);

    // screenshot
    const png = join(dir, 's.png');
    assert.equal(cli('screenshot', id, '-o', png).code, 0);
    assert.ok(statSync(png).size > 1000 && readFileSync(png).subarray(1, 4).toString() === 'PNG');

    // recording with a limit, listing, download
    const rec = json(cli('record', 'start', id, '-max', '2'));
    assert.equal(rec.status, 'recording');
    await new Promise((r) => setTimeout(r, 4500));
    const listed = json(cli('recordings', id));
    assert.deepEqual([listed.length, listed[0].running], [1, false], 'it stopped at its limit');
    const mp4 = join(dir, 'r.mp4');
    assert.equal(cli('record', 'get', id, '-o', mp4).code, 0);
    assert.equal(readFileSync(mp4).subarray(4, 8).toString(), 'ftyp');

    // tags
    const tagged = json(cli('tag', id, '-set', 'stage=failed', '-remove', 'n'));
    assert.deepEqual(tagged.tags, { suite: 'cli', stage: 'failed' });
    assert.ok(json(cli('ls', '-tag', 'stage=failed')).some((s) => s.id === id));
    assert.equal(cli('tag', id, '-set', 'novalue').code, 2);
  } finally {
    assert.equal(cli('rm', id).code, 0);
  }
  assert.equal(cli('rm', id).code, 1, 'removing it again is an error with a clear code');
  assert.match(cli('rm', id).err, /session_gone/);
  assert.equal(cli('get', id).code, 1);
});

t('errors from the daemon reach the user with their stable code', () => {
  assert.match(cli('create', '-browser', 'netscape').err, /invalid_request/);
  assert.match(cli('create', '-ttl', '99999999').err, /limit_exceeded/);
  assert.equal(cli('create', '-tag', 'bad').code, 2, 'a malformed -tag is a usage error');
  const nodaemon = spawnSync(BIN, ['info'], { encoding: 'utf8', env: { ...process.env, LOCALIMAGE_ADDR: '127.0.0.1:1' } });
  assert.equal(nodaemon.status, 1);
  assert.match(nodaemon.stderr, /not reachable/);
});

t('without the key the CLI is refused, with it (the key file) it works', () => {
  const wrong = spawnSync(BIN, ['info'], { encoding: 'utf8', env: { ...process.env, LOCALIMAGE_API_KEY: 'wrong-key' } });
  assert.equal(wrong.status, 1);
  assert.match(wrong.stderr, /unauthorized/);
});
