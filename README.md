# localimage

Run real, headful browsers in Docker on your own machine — with a live view you can watch and
control, recordings, and a Playwright-friendly connection URL. Built for end-to-end testing and
browser agents that need a visible browser, without a hosted service.

- **Chromium and Firefox**, each in its own container with a virtual display
- **Live view over WebRTC** (smooth video), interactive for you, **read-only** for everyone else
- **Playwright / Puppeteer** connect over CDP (Chromium) or the Playwright protocol (Firefox)
- **Recordings** (MP4), **screenshots**, **computer controls** (click, type, scroll, drag),
  **command exec** and **file upload/download** inside the session
- **Persistent profiles** and **Chromium extensions**
- One small daemon, a CLI, and SDKs for **Node** and **Python**

> Built on [Neko](https://github.com/m1k1o/neko) for the virtual display and WebRTC streaming.

## Requirements

- **Docker**, running, and your user allowed to use it (`docker ps` works)
- Internet access on first use: the browser images are built locally the first time you ask for
  that browser (about 1 minute for Chromium, a few minutes for Firefox), then cached
- Node 18+ **or** Python 3.9+ for the SDKs (tested on Node 24 and Python 3.14). The `localimage`
  binary itself needs neither.

Check your setup any time with `localimage doctor`.

## Install

```bash
npm install -g localimage        # CLI + Node SDK
pip install localimage           # CLI + Python SDK
```

Both ship the same prebuilt `localimage` binary for Linux (x64, arm64), macOS (x64, arm64) and
Windows (x64). Tested on Linux x64; see [Status](#status).

## Quick start

```bash
localimage doctor                 # check Docker and the environment
localimage start                  # run the daemon (leave it running)
```

In another terminal:

```bash
localimage create                 # prints the session id and URLs
```

```jsonc
{
  "id": "4a7be405",
  "browser": "chromium",
  "protocol": "cdp",
  "connect_url": "http://127.0.0.1:7777/t/<token>/cdp",   // for Playwright / Puppeteer
  "live_url":    "http://127.0.0.1:7777/t/<token>/live/?...",  // open in a browser: control it
  "view_url":    "http://127.0.0.1:7777/v/<token>/?..."        // open in a browser: watch only
}
```

Open `live_url`: you see the browser and can use it straight away (it takes control on your first
click or keypress). Open `view_url` in another tab: same picture, but nothing you do there reaches
the browser.

### Node

```js
import { LocalImage } from 'localimage';
import * as pw from 'playwright-core';

const li = new LocalImage();
const session = await li.create({ browser: 'chromium' });
console.log('watch:', session.liveUrl);

const browser = await session.connect(pw);          // connectOverCDP / firefox.connect for you
const page = browser.contexts()[0].pages()[0] ?? await browser.contexts()[0].newPage();
await page.goto('https://example.com');

await session.startRecording();
// ... drive the page ...
await session.stopRecording();
const mp4 = await session.downloadRecording();      // Buffer

await browser.close();
await session.close();                              // removes the container
```

### Python

```python
from localimage import LocalImage
from playwright.sync_api import sync_playwright

with sync_playwright() as pw, LocalImage().create(browser="firefox") as session:
    print("watch:", session.live_url)
    browser = session.connect(pw)
    page = browser.new_page()
    page.goto("https://example.com")
    open("shot.png", "wb").write(session.screenshot())
```

### Firefox needs a matching Playwright

Firefox is driven through Playwright's own server inside the container, so your client's
`playwright` / `playwright-core` version must match the one in the image, currently **1.63.0**.
Chromium uses plain CDP and works with any Playwright version.

## Sessions and options

`create` / `LocalImage.create(...)` accept:

| Option | Meaning |
|---|---|
| `browser` | `chromium` (default) or `firefox` |
| `ttl` / `ttlSeconds` | lifetime in seconds; the session is removed after it (default 30 min) |
| `profile` | named persistent profile (cookies, storage, …). Chromium only. One live session per profile |
| `extensions` | names of uploaded extensions to load. Chromium only |
| `kiosk` | Chromium without tabs or address bar |
| `screen` | display size, e.g. `1440x900@30` (default) |
| `memoryMb`, `cpus` | resource limits for the container |
| `tags` | `{key: value}` metadata stored on the session, returned by `list()` and usable as a filter (see [Test runners](#using-it-from-a-test-runner)) |
| `waitSeconds` / `wait_seconds` | queue for a free session slot instead of failing at once |
| `noBuild` / `no_build` | fail with `image_not_ready` instead of building a missing browser image |
| `signal`, `timeoutMs` (Node) / `timeout` (Python) | cancel the call or give up after a time; see [Cancelling and timeouts](#cancelling-and-timeouts) |

Sessions are Docker containers named `localimage-<id>`. They are removed on `close()`, on `rm`, or
when their TTL expires while the daemon runs.

TTL cleanup is performed by the daemon's background reaper, which checks for expired sessions every
15 seconds. There is no independent watchdog process. If the daemon is stopped, crashes, or cannot
reach Docker, existing containers are not removed at their TTL; they remain until the daemon is
running and can reach Docker again. Starting the daemon triggers the normal cleanup path.

## Using it from a test runner

Everything a test runner or CI provider needs to manage many sessions safely.

### Start the daemon on demand

Nobody has to run `localimage start` in a second terminal. Either call `ensureDaemon()` once, or
let the client do it on first use:

```js
import { LocalImage, ensureDaemon } from 'localimage';

await ensureDaemon({ env: { LOCALIMAGE_MAX_SESSIONS: '20' } });   // reuse a running daemon, else start one
const li = new LocalImage({ autoStart: true });                    // or: start it lazily on the first call
```

```python
from localimage import LocalImage, ensure_daemon
ensure_daemon(env={"LOCALIMAGE_MAX_SESSIONS": "20"})
li = LocalImage(auto_start=True)                                   # or LOCALIMAGE_AUTOSTART=1
```

A running daemon is always reused; racing callers, even in separate processes, end up sharing one, and
only the process that really started it gets `started: true`. A daemon started this way is
detached, so it outlives the process and is shared by parallel workers. `env` only affects a daemon
that is started here. Stop it with `localimage stop` (or `li.shutdown()`); sessions keep running
until their TTL passes. If it cannot start you get a clear error (see codes below), not a hang.

### GitHub Actions

For a Linux runner with Docker available, a job can prepare the image once, run tests, collect
diagnostics, and clean up even when a test fails:

```yaml
name: browser tests
on: [push, pull_request]
jobs:
  e2e:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 20
      - run: npm install -g localimage
      - run: npm ci
      - name: Prepare image and start daemon
        env:
          LOCALIMAGE_API_KEY: ${{ secrets.LOCALIMAGE_API_KEY }}
          LOCALIMAGE_MAX_SESSIONS: 4
        run: |
          set -euo pipefail
          localimage doctor
          localimage pull chromium
          localimage start
      - name: Run tests
        env:
          LOCALIMAGE_API_KEY: ${{ secrets.LOCALIMAGE_API_KEY }}
        run: npm test
      - name: Collect diagnostics
        if: always()
        run: |
          mkdir -p artifacts/localimage
          localimage info > artifacts/localimage/info.txt || true
          localimage ls > artifacts/localimage/sessions.txt || true
          localimage doctor > artifacts/localimage/doctor.txt 2>&1 || true
      - name: Upload artifacts
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: localimage-artifacts
          path: artifacts/localimage
      - name: Stop daemon and remove sessions
        if: always()
        run: |
          localimage stop || true
          docker ps -aq --filter name=localimage- | xargs -r docker rm -f || true
```

Prefer a short TTL and explicit `close()`/`rm` calls in tests. Do not print the API key or session
URLs in workflow logs. Use a repository secret only when the daemon is intentionally exposed beyond
the runner; a generated local key is sufficient for a single-user runner.

### TypeScript

The package ships its own declarations (`types` and `exports` in `package.json`), so strict
projects need nothing extra: every option, result and error code is typed, optional properties accept
an explicit `undefined` (it works with `exactOptionalPropertyTypes`), and `LocalImageErrorCode` is a
union you can switch over exhaustively. `connect()` returns playwright-core's `Browser`, so install
`playwright-core` (it is an optional peer dependency).

```ts
import { LocalImage, LocalImageError, ERROR_CODES, type LocalImageErrorCode } from 'localimage';

try {
  await li.create({ tags: { e2e_run: runId } });
} catch (e) {
  if (e instanceof LocalImageError && e.code === 'limit_exceeded') {
    console.error(`only ${e.details?.['max']} sessions can run at once`);
  }
}
```

`ERROR_CODES` lists every code at runtime. The declarations are checked in CI by compiling a strict
TypeScript file against them, and a test fails if they drift from the real objects.

### Stable error codes

Every failure is a `LocalImageError` (Python: a `RuntimeError` subclass) with a `code`, so a
provider can map errors without parsing messages:

| Code | Meaning |
|---|---|
| `daemon_unreachable` | nothing is listening at the daemon address |
| `daemon_start_failed` | `ensureDaemon` could not start the daemon (the message includes its log) |
| `docker_unavailable` | the daemon cannot reach Docker |
| `image_not_ready` | `noBuild` was set and the browser image is not built (see `prepare`) |
| `request_timeout` | the call exceeded its `timeoutMs` / `timeout` |
| `incompatible_daemon` | `assertCompatible` found a daemon this SDK cannot talk to |
| `binary_not_found` | there is no localimage binary for this platform |
| `limit_exceeded` | too many sessions, or a ttl over the limit. `details` has `{max, running}` |
| `session_gone` | the session does not exist (deleted, expired, never created) |
| `invalid_request` | malformed request, or it names something that does not exist |
| `conflict` | clashes with current state (for example a profile already in use) |
| `unauthorized` | missing or wrong API key |
| `forbidden` | the request's `Host` or `Origin` is not allowed (see Security model) |
| `not_found` | some other resource does not exist |
| `internal` | anything else the daemon reports |
| `http_<status>` | a response that was not the daemon's own JSON error (for example from a proxy) |

### Tags and sweeping up after a dead worker

```js
const s = await li.create({ tags: { e2e_run: runId, e2e_target: 'checkout' } });

// Later, from a sweep process: find every session of that run, whatever state it is in.
for (const x of await li.list({ tags: { e2e_run: runId } })) await li.delete(x.id);
```

Tags are stored as Docker labels, so they survive a daemon restart. A filter keeps sessions that
carry **all** the given tags. `list()` returns sessions in every state, including stopped ones that
a crashed worker left behind (`state` is `running`, `exited`, ...). Up to 20 tags per session;
keys use letters, digits, `.`, `_` and `-`; values are up to 256 characters.

### Releasing is idempotent

`delete(id)` / `session.close()` resolves `true` if it removed the session and `false` if it was
already gone, and never throws for that, so it is safe to call once per lease, twice, concurrently,
or after another slot's acquire failed. Real failures (such as `daemon_unreachable`) still throw.

### Concurrency

The daemon allows `LOCALIMAGE_MAX_SESSIONS` sessions at once (default 10), counted across everything
labelled as localimage in Docker, so two daemons on one machine share the limit.

```js
await li.assertCapacity(workers);   // throws limit_exceeded, with the fix, if `workers` could never fit
const { maxSessions, running, available } = await li.info();
const s = await li.create({ waitSeconds: 60 });   // queue for a slot instead of failing at once
```

A create over the limit is **refused immediately** (HTTP 429 with `Retry-After`, code
`limit_exceeded`) rather than timing out, and the limit holds under a burst of simultaneous creates.
With `waitSeconds` the request queues until a slot frees up or the time runs out. Browser images are
built once even when many creates arrive together on a fresh machine.

### Readiness, first-run builds and `prepare`

`create` returns only when the browser is usable: CDP answers `/json/version` (or the Playwright
server answers, for Firefox) and the live view is serving, so a client never has to poll. The one
exception to "fast" is the first launch of each browser, which **builds its image**: about a minute
for Chromium, a few for Firefox. That is longer than most attempt budgets, so warm the images in a
prepare phase and tell `create` not to build:

```js
await li.prepare({ browsers: ['chromium', 'firefox'], onProgress: ({ browser, message }) => log(browser, message) });
// resolves to { chromium: 'ready' | 'built', ... }; instant when the images already exist

const s = await li.create({ noBuild: true });   // fails fast with `image_not_ready` instead of building
```

From a shell: `localimage pull chromium firefox`. `info().images` says which images are built. The
build streams progress and keep-alives, so no client times out, and cancelling it keeps Docker's
finished layers.

### Cancelling and timeouts

Node: every network call takes `{ signal, timeoutMs }`, including `create`, `delete`/`close`, the
downloads (`downloadFile`, `downloadRecording`, `screenshot`), `prepare` and `ensureDaemon`. An
aborted call rejects with the signal's reason (normally an `AbortError`, not a `LocalImageError`);
`timeoutMs` rejects with `request_timeout`. **Cancelling a `create` leaves nothing behind**: the daemon
removes the container it had started and frees the slot, whenever in the call the abort lands.

```js
const s = await li.create({ signal: request.signal });
await s.close({ signal: AbortSignal.timeout(5000) });
```

The SDK does not use `fetch`, whose built-in five-minute response timeout would cut off a long first
build and report it as an unreachable daemon. There is no ceiling unless you set `timeoutMs`.
Python has no cancellation; pass `timeout=` (seconds of silence) to any call. `create` and `prepare`
default to no limit.

### Reconnecting to a session (leases)

A session is described by plain data: `{ id, connectUrl }`. `connectUrl` is
`http://127.0.0.1:7777/t/<token>/cdp` (or `ws://.../pw` for Firefox) and nothing about it lives in
the process that created the session, so a lease can be passed through an environment variable to
another process. Verified for **Chromium** with real separate processes:

- After the connection drops, politely or abruptly, the session keeps running and another process
  can attach with only the lease. It finds **the same page, with its JavaScript state**.
- It also works after the daemon itself is restarted: the token is stored on the container, not in
  the daemon's memory.

**Firefox differs.** The session survives and a new process can attach, but Playwright's server
closes the pages a connection opened when that connection ends, so the new connection starts without
them. For Firefox, keep one connection for the whole attempt.

### Acting on a session by id

`create` is not the only way to get a `Session`. From another process, after a restart, or from a stored
lease, `attach` builds the same object from just the id:

```js
const s = await new LocalImage().attach(lease.id);   // rejects with `session_gone` if it is not there
await s.downloadFile(`${s.downloadsDir}/report.pdf`);
const { replay_id } = await s.startRecording();      // and so on: everything create() returns works
```

A stopped session can be attached too, which is how you find out why it died: `await s.logs()`.

### Logs

`session.logs({ source, tail })` returns the end of a log as text. `source` is `container` (Docker's
capture of the container's output, the default), `chromium`, `firefox` (the Playwright server), `neko`,
`xorg`, `openbox`, `pulseaudio` or `cdp-forward`; `tail` is a number of lines (default 200, at most
5000). It works for stopped containers, so a crashed session leaves something to debug with.

### Reaching an app on your machine

The browser runs in a container, so `localhost` there is the container. Every session maps
`host.docker.internal` to the host, so an app on your machine is reachable as
`http://host.docker.internal:<port>/` **if it listens on a non-loopback address** (`0.0.0.0`, or your
machine's address). Most dev servers listen on `127.0.0.1` by default, and **a service bound only to
the host's loopback cannot be reached from any container, whatever name you use** (verified). Start the
server with its host option set to `0.0.0.0` (for example `--host 0.0.0.0`), and treat that as exposing
it to your network while it runs. Docker Desktop (macOS, Windows) provides `host.docker.internal` itself
and reaches loopback services; that is untested here.

### Versions and compatibility

The browser images are built from **base images pinned by digest**, so every machine gets the same
browser. `info()` reports what is inside, and `assertCompatible` checks it:

```js
await li.assertCompatible({ chromium: /^154\./ });   // rejects with `incompatible_daemon` if not
const { version, apiVersion, components } = await li.info();   // components.chromium.version, ...
```

| localimage | daemon API | Chromium | Firefox | Playwright (Firefox only) |
|---|---|---|---|---|
| 0.4.x | 1 | 154.0.8037.57 | 155.0 | 1.63.0 |
| 0.3.x | 1 | 154.0.8037.57 | 155.0 | 1.63.0 |

The SDK and the daemon speak API revision `SUPPORTED_API_VERSION`; `assertCompatible` rejects a daemon
that does not. Chromium is driven over CDP, so a different Playwright client version also works with it;
Firefox needs the exact Playwright version in the table.

### Browser crashes and health

If the browser inside a session crashes it comes back by itself within a second, on the **same URL**,
but as a **new, empty browser**: your connection drops and every page is gone. Nothing else changes,
so a client needs a way to notice:

```js
const { starts, restarts, health, last_started_at } = await session.health();
// restarts > 0: the browser crashed and was restarted. health: starting | healthy | unhealthy
```

`list()` entries carry `health` too. The Docker health check covers the browser as well as the live view.

### Changing tags later

Tags given at creation are Docker labels and cannot change, so later changes are kept alongside them
and merged on every read: `await session.updateTags({ set: { stage: 'failed' }, remove: ['tmp'] })`.
`list({ tags })` and `attach` see the current tags, so an engine can mark attempts as it goes.

### Logs outlive a crash, not the session

A session's logs vanish with its container. `await session.saveLogs('./artifacts')` writes every source
to `./artifacts/<id>/<source>.log`; call it when a test fails, before `close()`.

### WebGL, timezone and language

`create({ webgl: true })` turns on WebGL through software rendering (Chromium only; it costs CPU and is
off by default). `timezone: 'Europe/Berlin'` sets the browser's timezone (the page may report a legacy
alias of the same zone, such as `Asia/Calcutta`); `locale: 'de-DE'` sets its language (Chromium only).
For Firefox, set `timezoneId` and `locale` on the Playwright context.

### Recording limits

A recording stops by itself after `maxSeconds` (default 3600, at most 4 hours; the default is
`LOCALIMAGE_MAX_RECORDING_SECONDS`), so a forgotten one cannot fill the container's disk.

### Disk, memory and capacity

Each browser image is about 1.6 GB. Old revisions left by earlier versions pile up; `localimage prune`
(or `await li.prune()`) removes them, and `info().staleImages` and `localimage doctor` report them.
An idle session uses about 300 MiB of memory and 1–2% of a CPU, more with pages open, so the default
limit of 10 sessions needs roughly 3 GB. Ten sessions start in parallel in about 9 seconds.

To reproduce the same `apt` packages when building images, set `LOCALIMAGE_APT_SNAPSHOT` (a Debian
snapshot timestamp such as `20261001T000000Z`) for the daemon. It is off by default because
snapshot.debian.org can be slow.

Closing or expiring a session removes its container, including recordings, downloads, browser cache,
and other files stored in that container. Named persistent profiles are Docker volumes and survive
session deletion; remove them explicitly with `localimage profile rm <name>` after checking usage with
`localimage profile ls`. Docker storage exhaustion causes image builds, uploads, recordings, or session
creation to fail; localimage does not enforce a per-session disk quota or automatically reclaim files
from a running session. `localimage prune` only targets stale localimage image tags, keeps current
images, and leaves an image in use by an active container.

### The daemon's log

The daemon logs session creation and removal, failures and refused requests to stderr (a file when
`ensureDaemon` started it: its path is in the result). `LOCALIMAGE_LOG_LEVEL=debug` adds every API call.
Session tokens are redacted.

### Downloads

Downloads go to `session.downloadsDir` (`/home/neko/Downloads`), which exists, belongs to the browser
user and is writable. Read files with `downloadFile(path)`:

- **While Playwright is attached** (Chromium, CDP): Playwright keeps the download inside the container.
  `download.path()` is a container path, `download.saveAs()` fails (it looks on your machine), but
  `session.downloadFile(await download.path())` reads it, even after you detach.
- **A person downloading in the live view**: the file lands in `downloadsDir`; use `listFiles` and
  `downloadFile`.

### Screen versus viewport

The display (`screen`, default `1440x900@30`) and the page viewport are separate. Measured in
Chromium:

| Page | Viewport | Window |
|---|---|---|
| the existing page / default context | 1440×813 (browser chrome is the rest) | fills the screen |
| `browser.newContext({ viewport: null })` | 1440×813 | fills the screen |
| `browser.newContext()` (Playwright's default) | **1280×720 emulated** | window fills the screen, page does not |
| kiosk session, the existing page | **1440×900** | no browser chrome |

**Recommended:** use `viewport: null` for every context, so the page fills the display and the live
view shows no empty margin; use `kiosk: true` if you want exactly screen-sized pages with no browser
chrome. (A new context's window in a kiosk session is not kiosk.) Firefox windows are maximized the
same way, and `viewport: null` fills the screen there too. To make the page size match a test's viewport,
set `screen` to it, e.g. `1280x720@30`.

### Recordings

`downloadRecording` returns an **MP4** (`video/mp4`): H.264 Constrained Baseline, `yuv420p`, at the
screen size and 15 fps, as a fragmented MP4 that plays in browsers and common players. Timestamps come
from the host clock, so they line up with your run's own:

| | |
|---|---|
| `startRecording()` | resolves in well under a second, once frames are being written: `{ replay_id, started_at, started_at_ms, content_type }`. `started_at` is the video's time zero |
| `stopRecording()` | the file is complete when it resolves: adds `stopped_at` (capture ended), `finalized_at` (file complete, a moment later), `duration_ms` (measured), `size`, `width`, `height`, `fps`, `codec` |

`stopped_at - started_at` matches `duration_ms` to within a couple of frames.

## Features

- **Recordings:** `startRecording` / `stopRecording`, several per session, one running at a time;
  list and download each as MP4.
- **Screenshots:** whole display or a cropped region.
- **Computer controls:** `click`, `moveMouse`, `type`, `pressKey`, `scroll`, `drag`. These act on
  the whole display in screen coordinates (not page coordinates) and work for both browsers.
- **Exec:** run a command inside the session as the browser user (no shell unless you pass
  `["sh", "-c", ...]`), with a timeout and capped output.
- **Files:** upload, download and list files inside the session (e.g. the browser's Downloads).
- **Profiles:** `localimage create --profile work` keeps cookies and storage between sessions.
- **Extensions:** `localimage extension upload my-ext ./my-ext-dir`, then
  `localimage create --extension my-ext`.

## CLI

```
localimage start [-addr host:port]     run the daemon (default 127.0.0.1:7777)
localimage stop                        stop the daemon (sessions keep running until their TTL)
localimage doctor                      check Docker, images and the daemon
localimage pull [chromium|firefox ...] build the browser images now (default: chromium)
localimage prune                       remove browser images left by older versions
localimage get|health|recordings <id>  show a session, its browser health, its recordings
localimage logs <id> [-source s] [-tail n]
localimage download <id> <path> [-o file] | upload <id> <file> <path>
localimage tag <id> -set k=v -remove k
localimage info                        the daemon's version, limits and current load
localimage create [flags]              start a session (-tag key=value, -wait seconds, ...)
localimage ls [-tag key=value ...]     list sessions (all states), optionally filtered by tag
localimage rm <id>                     delete a session
localimage exec <id> -- <cmd...>       run a command in a session
localimage screenshot <id> [-o file]
localimage record start|stop|get <id> [-o file]
localimage profile ls | rm <name>
localimage extension upload <name> <dir-or-zip> | ls | rm <name>
```

Run `localimage help` for every flag.

## HTTP API

The daemon exposes a JSON API (default `http://127.0.0.1:7777`):

| | |
|---|---|
| `POST/GET /v1/sessions`, `GET/DELETE /v1/sessions/{id}` | create (`tags`, `wait_seconds`), list (`?tag=key=value`, repeatable), inspect, delete |
| `GET /v1/info` | version, `api_version`, `components` (browser versions), `images`, `max_sessions`, `max_ttl_seconds`, `running`, `available` |
| `GET /v1/sessions/{id}/health` | container state, Docker health, browser starts and restarts |
| `PATCH /v1/sessions/{id}/tags` | `{set, remove}`: change tags after creation |
| `POST /v1/images/prune` | remove browser image tags left by older versions |
| `POST /v1/auth/rotate-key` | rotate the generated API key; returns `{"api_key":"..."}` once, and the previous key stops working immediately |
| `GET /v1/sessions/{id}/logs?source=&tail=` | the end of a session's log, as text; works for stopped sessions |
| `POST /v1/shutdown` | stop the daemon |
| `POST /v1/images/prepare` | build missing browser images; streams newline-delimited JSON progress |
| `POST /v1/sessions/{id}/recording/start`, `.../stop`; `GET .../recording`, `.../recordings[/{rid}]` | recordings |
| `GET /v1/sessions/{id}/screenshot[?x=&y=&width=&height=]` | PNG |
| `POST /v1/sessions/{id}/computer/{click,move,type,key,scroll,drag}` | input |
| `POST /v1/sessions/{id}/process/exec` | run a command |
| `PUT/GET /v1/sessions/{id}/files?path=`, `GET .../files/list?path=` | file I/O |
| `POST/GET /v1/extensions`, `DELETE /v1/extensions/{name}` | extensions (zip upload) |
| `GET /v1/profiles`, `DELETE /v1/profiles/{name}` | profiles |
| `GET /healthz` | liveness: `{"ok": true, "pid": N}` |

Errors are JSON, `{"error": "<message>", "code": "<code>", "details": {...}}`, with the codes in the
table above.

## Configuration

| Variable | Default | |
|---|---|---|
| `LOCALIMAGE_BIN` | the installed binary | path of a `localimage` binary to use instead of the one in the installed package (development) |
| `LOCALIMAGE_ADDR` | `127.0.0.1:7777` | daemon address (CLI and SDKs) |
| `LOCALIMAGE_API_KEY` | generated file | the control API key; by default a private key file is created and read by the CLI and SDKs |
| `LOCALIMAGE_CONFIG_DIR` | platform config directory | shared parent directory for the generated key file (`<dir>/localimage/api-key`) |
| `LOCALIMAGE_TLS_CERT` | unset | TLS certificate PEM file; enables HTTPS for the daemon and all proxy URLs |
| `LOCALIMAGE_TLS_KEY` | unset | TLS private key PEM file; must be set with `LOCALIMAGE_TLS_CERT` |
| `LOCALIMAGE_NO_AUTH` | unset | `1` turns authentication off |
| `LOCALIMAGE_ALLOWED_HOSTS`, `LOCALIMAGE_ALLOWED_ORIGINS` | unset | extra Host names / browser origins the daemon accepts |
| `LOCALIMAGE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `LOCALIMAGE_MAX_RECORDING_SECONDS` | `3600` | default recording limit |
| `LOCALIMAGE_APT_SNAPSHOT` | unset | build images from a dated Debian snapshot |
| `LOCALIMAGE_AUTOSTART` | unset | `1` makes the SDK clients start the daemon on first use |
| `LOCALIMAGE_PUBLIC_IP` | `127.0.0.1` | address clients use for WebRTC media (see below) |
| `LOCALIMAGE_MAX_SESSIONS` | `10` | concurrent sessions allowed |
| `LOCALIMAGE_MAX_TTL_SECONDS` | `86400` | longest session lifetime allowed |

## Security model

- **The control API is authenticated by default.** On first start the daemon generates a random key
  and keeps it in a file only your user can read (`~/.config/localimage/api-key`, or the platform's
  equivalent). The CLI and both SDKs read the same file, so you configure nothing; other users on the
  machine cannot call the daemon. The generated key file is owner-only on Unix and uses a
  protected owner-only Windows ACL. Set `LOCALIMAGE_API_KEY` to choose your own key. For direct HTTP
  requests, send an `Authorization: Bearer` header containing that key, or set `LOCALIMAGE_NO_AUTH=1` to
  turn authentication off (not recommended). `/healthz` and the live-view token URLs do not use the key. Use
  `POST /v1/auth/rotate-key` (or `rotateKey()` / `rotate_key()`) to immediately replace the
  generated key. The new key is returned once; clients using the old key must reload it. Keys
  configured through `LOCALIMAGE_API_KEY` are rotated by changing that variable and restarting.
  Set `LOCALIMAGE_CONFIG_DIR` to an explicit shared directory when the daemon and SDK run with
  different home/config environments; all clients must use the same value.
  Set `LOCALIMAGE_TLS_CERT` and `LOCALIMAGE_TLS_KEY` together to serve the control API, live view,
  CDP, and Playwright proxy over HTTPS. SDK clients may use `https://host:port` in `addr`; returned
  Chromium and Playwright URLs use `wss://` where appropriate. Certificate verification is enabled
  by default; use a certificate trusted by the client rather than disabling verification.
- Session lifetimes are enforced by a background reaper in the running daemon. The reaper checks
  every 15 seconds. If the daemon is stopped or crashes, or Docker is unavailable, a session keeps
  running past its lifetime because there is no independent expiry process. When the daemon starts
  again and reconnects to Docker, it removes expired sessions during a subsequent reaper pass.
  `localimage doctor` warns when sessions are past their lifetime and no daemon is running.
  Lifetimes cannot be extended.
- **Web pages cannot drive the daemon.** Any page you visit could otherwise send requests to
  `http://127.0.0.1:7777`. The daemon refuses a request whose `Host` is not an IP address or
  `localhost` (the DNS-rebinding case), any request carrying a foreign `Origin`, and writes with a
  content type a browser can send without a preflight. All are `403 forbidden` (or 415) and are logged.
  `LOCALIMAGE_ALLOWED_HOSTS` and `LOCALIMAGE_ALLOWED_ORIGINS` (comma separated) add exceptions.
- The daemon listens on **loopback only** by default and **refuses to listen on any other address
  unless `LOCALIMAGE_API_KEY` is set**. Requests have size limits (1 MiB of JSON) and the server has
  timeouts.
- Each session has a random secret in its URLs. `connect_url` and `live_url` give full control of
  the browser; `view_url` uses a different secret and a viewer login that cannot control anything
  and cannot reach CDP. **Treat these URLs like passwords.** The daemon log never contains them.
- Anyone with the API key can run commands and read files in your session containers. Sessions are
  containers, not a hardened sandbox for hostile code.
- Do not run untrusted websites, extensions, test code, or browser content in a daemon that can
  access sensitive host resources or credentials. `exec`, file upload/download, persistent profiles,
  extensions, and browser control can expose or modify session data.
- WebRTC video cannot go through the HTTP proxy: each session also opens one media port on the
  host (the same number inside and outside the container).
- TLS is opt-in. Set `LOCALIMAGE_TLS_CERT` and `LOCALIMAGE_TLS_KEY` together to serve the control
  API, live view, CDP, and Playwright proxy over HTTPS/WSS. Without TLS, do not expose the daemon
  or bearer session URLs to untrusted networks.
- The release workflow pins every action to a commit (updated by hand), publishes npm
  packages with provenance, and attests the release binaries, on public repositories.

### Upgrading from 0.2.x

Authentication is now on by default. The CLI and SDKs need no change. Anything else that called the
API directly (`curl`, another language) must send the key, for example:

```bash
KEY="$(cat ~/.config/localimage/api-key)"
curl \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  http://127.0.0.1:7777/v1/info
```

## Troubleshooting

- **`localimage doctor`** explains most setup problems.
- **Black or frozen live view:** a VPN, firewall or browser extension that blocks WebRTC. Try a
  private window with extensions off, and open the page at `127.0.0.1:7777`.
- **Remote use:** set `LOCALIMAGE_PUBLIC_IP` to an address the viewer can reach, bind the daemon
  to a non-loopback address and set `LOCALIMAGE_API_KEY`. Remote use is experimental, not a
  supported production deployment. Open the daemon's configured TCP port and, for every active
  session, the WebRTC media port over both UDP and TCP. `LOCALIMAGE_PUBLIC_IP` must be a reachable
  address; it is not sufficient when NAT, firewalls, or multiple interfaces require port forwarding
  or interface-specific routing. Reverse proxies are not supported for live view or CDP/Playwright
  proxy URLs. TLS and API-key authentication are required for any remote configuration; do not use
  `LOCALIMAGE_NO_AUTH` remotely.
- **"profile is already in use":** a Chromium profile can only be open in one session at a time.
- **Firefox connect fails with a version error:** match Playwright to the image's version.

## Status

Early (0.x). Tested end to end on **Linux x64** with Docker: both browsers, live view video and control
in Chrome, read-only enforcement, recordings, profiles, extensions, exec and files, tags, reconnecting
across processes and daemon restarts, cancellation, browser-crash recovery, the authentication and
cross-site protections, the CLI, and the TypeScript declarations. File uploads to forms work from the
client machine (a local path or a buffer; a path inside the container does not).

Known limits:

- **macOS and Windows** builds compile but have not been run, and Docker Desktop's networking differs.
- **An app listening only on `127.0.0.1`** cannot be reached from the browser (see
  "Reaching an app on your machine").
- **Firefox** loses its pages when a connection ends; it has no WebGL option, locale option or kiosk mode.
- No warm browser pools, standby, stealth or proxy options, WebKit, or multi-host scaling. TLS is
  available as an opt-in configuration but has not been validated for remote deployment.

## Development

```bash
go vet ./... && go test ./...                 # unit tests (Go version: see go.mod)
go build -o bin/localimage ./cmd/localimage

localimage start &                            # then, in packages/npm:
npm install && npm test                       # end-to-end tests (needs Docker + network;
                                              # the live-view test also needs Chrome/Chromium)

python3 scripts/package.py                    # build binaries, npm packages and wheels into dist/
python3 scripts/bump-version.py 0.2.0         # set the version everywhere
```

See [RELEASING.md](RELEASING.md) for the release process.

## Acknowledgements

Streaming and the virtual display come from [Neko](https://github.com/m1k1o/neko) (Apache-2.0).
Firefox automation uses [Playwright](https://playwright.dev) (Apache-2.0). The browser images are
built locally on your machine from upstream Debian packages and the Neko base images.

## License

[Apache-2.0](LICENSE)
