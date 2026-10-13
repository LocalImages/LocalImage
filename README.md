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

Sessions are Docker containers named `localimage-<id>`. They are removed on `close()`, on `rm`, or
when their TTL expires while the daemon runs.

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
localimage doctor                      check Docker, images and the daemon
localimage create [flags]              start a session
localimage ls | rm <id>                list / delete sessions
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
| `POST/GET /v1/sessions`, `GET/DELETE /v1/sessions/{id}` | create, list, inspect, delete |
| `POST /v1/sessions/{id}/recording/start`, `.../stop`; `GET .../recording`, `.../recordings[/{rid}]` | recordings |
| `GET /v1/sessions/{id}/screenshot[?x=&y=&width=&height=]` | PNG |
| `POST /v1/sessions/{id}/computer/{click,move,type,key,scroll,drag}` | input |
| `POST /v1/sessions/{id}/process/exec` | run a command |
| `PUT/GET /v1/sessions/{id}/files?path=`, `GET .../files/list?path=` | file I/O |
| `POST/GET /v1/extensions`, `DELETE /v1/extensions/{name}` | extensions (zip upload) |
| `GET /v1/profiles`, `DELETE /v1/profiles/{name}` | profiles |
| `GET /healthz` | liveness |

## Configuration

| Variable | Default | |
|---|---|---|
| `LOCALIMAGE_ADDR` | `127.0.0.1:7777` | daemon address (CLI and SDKs) |
| `LOCALIMAGE_API_KEY` | unset | if set, the control API requires `Authorization: Bearer <key>` |
| `LOCALIMAGE_PUBLIC_IP` | `127.0.0.1` | address clients use for WebRTC media (see below) |
| `LOCALIMAGE_MAX_SESSIONS` | `10` | concurrent sessions allowed |
| `LOCALIMAGE_MAX_TTL_SECONDS` | `86400` | longest session lifetime allowed |

## Security model

- The daemon listens on **loopback only** by default and **refuses to listen on any other address
  unless `LOCALIMAGE_API_KEY` is set**.
- Each session has a random secret in its URLs. `connect_url` and `live_url` give full control of
  the browser; `view_url` uses a different secret and a viewer login that cannot control anything
  and cannot reach CDP. **Treat these URLs like passwords.**
- Anyone with the API key can run commands and read files in your session containers. Sessions are
  containers, not a hardened sandbox for hostile code.
- WebRTC video cannot go through the HTTP proxy: each session also opens one media port on the
  host (the same number inside and outside the container).
- Sessions are not encrypted on the wire: there is no TLS. Do not expose the daemon to untrusted
  networks.

## Troubleshooting

- **`localimage doctor`** explains most setup problems.
- **Black or frozen live view:** a VPN, firewall or browser extension that blocks WebRTC. Try a
  private window with extensions off, and open the page at `127.0.0.1:7777`.
- **Remote use:** set `LOCALIMAGE_PUBLIC_IP` to an address the viewer can reach, bind the daemon
  to a non-loopback address and set `LOCALIMAGE_API_KEY`. This setup is not tested yet.
- **"profile is already in use":** a Chromium profile can only be open in one session at a time.
- **Firefox connect fails with a version error:** match Playwright to the image's version.

## Status

Early (0.x). Tested end to end on Linux x64 with Docker: both browsers, live view video and
control in Chrome, read-only enforcement, recordings, profiles, extensions, exec and files.
The macOS and Windows builds compile but have not been run. Not implemented: warm browser pools,
standby/resume, stealth and proxy options, WebKit, TLS, multi-host scaling.

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
