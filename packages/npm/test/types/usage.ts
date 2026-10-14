// Compile-only test: it is never run. `npm run test:types` type-checks it in strict mode, so the
// declarations must accept what a TypeScript consumer writes and reject what it should not.
import {
  ERROR_CODES,
  LocalImage,
  LocalImageError,
  ensureDaemon,
  type CreateOptions,
  type Info,
  type LocalImageErrorCode,
  type RecordingInfo,
  type Session,
  type SessionInfo,
} from 'localimage';

// An exhaustive map of every error code. Adding a code without handling it here is a compile error.
function describe(code: LocalImageErrorCode): string {
  switch (code) {
    case 'session_gone':
    case 'limit_exceeded':
    case 'invalid_request':
    case 'conflict':
    case 'image_not_ready':
    case 'unauthorized':
    case 'forbidden':
    case 'docker_unavailable':
    case 'not_found':
    case 'internal':
    case 'daemon_unreachable':
    case 'daemon_start_failed':
    case 'binary_not_found':
    case 'request_timeout':
    case 'incompatible_daemon':
      return code;
    default: {
      const rest: `http_${number}` = code;
      return rest;
    }
  }
}

async function main(signal: AbortSignal, maybeTags: Record<string, string> | undefined): Promise<void> {
  const started = await ensureDaemon({ env: { LOCALIMAGE_MAX_SESSIONS: '20' }, signal });
  const started2: boolean = started.started;
  const pid: number | undefined = started.pid;

  const li = new LocalImage({ autoStart: true, daemonEnv: { LOCALIMAGE_MAX_SESSIONS: '20' } });
  const info: Info = await li.assertCapacity(8, { signal });
  const free: number = info.maxSessions - info.running;
  const haveChromium: boolean | undefined = info.images.chromium;

  const built = await li.prepare({ browsers: ['chromium', 'firefox'], onProgress: (e) => e.message.trim(), signal });
  const state: 'ready' | 'built' | undefined = built.chromium;

  // Optional properties accept an explicit undefined (exactOptionalPropertyTypes is on).
  const opts: CreateOptions = { tags: maybeTags, ttlSeconds: undefined, noBuild: true, signal, timeoutMs: 120_000 };
  const s: Session = await li.create({ ...opts, browser: 'chromium', tags: { e2e_run: 'r1', e2e_target: 'checkout' } });
  const live: string = s.liveUrl;
  const dir: string = s.downloadsDir;
  const when: Date = s.expiresAt;

  // Sweep: every session of a run, in any state.
  const mine: SessionInfo[] = await li.list({ tags: { e2e_run: 'r1' }, signal });
  for (const x of mine) {
    const gone: boolean = await li.delete(x.id, { signal });
    const running: boolean = x.state === 'running';
    const target: string | undefined = x.tags?.['e2e_target'];
    void [gone, running, target];
  }

  const rec: RecordingInfo = await s.startRecording();
  const done = await s.stopRecording({ replayId: rec.replay_id, signal });
  const bytes: Buffer = await s.downloadRecording(done.replay_id, { signal });
  const text: Buffer = await s.downloadFile(`${dir}/x.txt`, { signal });
  const run = await s.exec(['echo', 'hi'], { cwd: '/tmp', timeoutSeconds: 5 });
  const code: number = run.exit_code;
  await s.click(10, 20, { button: 'left', numClicks: 2 });
  const h = await s.health({ signal });
  const restarts: number = h.restarts;
  const tags2: Record<string, string> = await s.updateTags({ set: { status: 'failed' }, remove: ['old'] });
  const saved: string[] = await s.saveLogs('/tmp/logs');
  const limited: RecordingInfo = await s.startRecording({ maxSeconds: 60 });
  const removed: string[] = await li.prune();
  const wg: CreateOptions = { webgl: true, timezone: 'Europe/Berlin', locale: 'de-DE' };
  void [restarts, tags2, saved, limited, removed, wg];
  const closed: boolean = await s.close({ signal });

  try {
    await li.create();
  } catch (e) {
    if (e instanceof LocalImageError) {
      const c: LocalImageErrorCode = e.code;
      if (e.code === 'limit_exceeded') {
        const max: number | undefined = e.details?.['max'];
        void max;
      }
      describe(c);
    }
  }
  void [started2, pid, free, haveChromium, state, live, when, bytes, text, code, closed, ERROR_CODES.length];
}
void main;

// The declarations must also REJECT mistakes.
async function mistakes(li: LocalImage, s: Session): Promise<void> {
  // @ts-expect-error unknown browser
  await li.create({ browser: 'netscape' });
  // @ts-expect-error tags are strings
  await li.create({ tags: { n: 1 } });
  // @ts-expect-error not an option
  await li.create({ ttl: 5 });
  // @ts-expect-error delete resolves a boolean, not void
  const v: void = await li.delete('abc');
  // @ts-expect-error unknown error code
  const c: LocalImageErrorCode = 'made_up';
  // @ts-expect-error id is a string
  await li.delete(5);
  // @ts-expect-error recordings are described by RecordingInfo, not a string
  const r: string = await s.startRecording();
  void [v, c, r];
}
void mistakes;

// The declared members must be exactly these. The same lists are checked against the real objects
// in engine.test.mjs ("the SDK objects have exactly the declared members"): keep both in sync.
const SESSION_KEYS = [
  'id', 'browser', 'protocol', 'connectUrl', 'liveUrl', 'viewUrl', 'profile', 'extensions', 'tags',
  'downloadsDir', 'expiresAt', 'connect', 'screenshot', 'click', 'moveMouse', 'type', 'pressKey', 'scroll',
  'drag', 'startRecording', 'stopRecording', 'recordings', 'downloadRecording', 'exec', 'uploadFile',
  'downloadFile', 'listFiles', 'close', 'logs', 'health', 'updateTags', 'saveLogs',
] as const;
const CLIENT_KEYS = [
  'addr', 'apiKey', 'autoStart', 'daemonEnv', 'ensureDaemon', 'shutdown', 'info', 'assertCapacity', 'prepare',
  'create', 'list', 'delete', 'uploadExtension', 'extensions', 'deleteExtension', 'profiles', 'deleteProfile',
  'attach', 'assertCompatible', 'prune',
] as const;
type Undeclared<T, K extends PropertyKey> = Exclude<keyof T, K>; // declared but not in the list
type Unimplemented<T, K extends PropertyKey> = Exclude<K, keyof T>; // in the list but not declared
const sessionExact: [Undeclared<Session, (typeof SESSION_KEYS)[number]>, Unimplemented<Session, (typeof SESSION_KEYS)[number]>] extends [never, never] ? true : never = true;
const clientExact: [Undeclared<LocalImage, (typeof CLIENT_KEYS)[number]>, Unimplemented<LocalImage, (typeof CLIENT_KEYS)[number]>] extends [never, never] ? true : never = true;
void [sessionExact, clientExact];
