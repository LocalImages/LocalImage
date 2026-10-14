import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Dict, Optional

from ._binary import find_binary, make_executable
from ._apikey import read_key_file

SUPPORTED_API_VERSION = 1  # the daemon API revision this SDK speaks
DEFAULT_ADDR = os.environ.get("LOCALIMAGE_ADDR", "127.0.0.1:7777")
_DEFAULT = object()  # "use the client's default timeout"


class LocalImageError(RuntimeError):
    """Every failure is a LocalImageError with a stable ``code``, so callers can branch without
    parsing messages. Codes from the daemon:

    - ``session_gone``: the session does not exist (already deleted, expired, never created)
    - ``limit_exceeded``: too many sessions, or a ttl over the limit; ``details`` has max/running
    - ``invalid_request``: malformed request, or it names something that does not exist
    - ``conflict``: clashes with current state (for example a profile already in use)
    - ``image_not_ready``: ``no_build`` was set and the browser image is not built (see prepare())
    - ``unauthorized``: missing or wrong API key
    - ``forbidden``: the request's Host or Origin is not allowed (LOCALIMAGE_ALLOWED_HOSTS)
    - ``docker_unavailable``: the daemon cannot reach Docker
    - ``not_found``, ``internal``

    Codes raised by this SDK:

    - ``request_timeout``: the call exceeded its ``timeout``
    - ``incompatible_daemon``: assert_compatible() found a daemon this SDK cannot talk to
    - ``daemon_unreachable``: nothing is listening at the daemon address
    - ``daemon_start_failed``: ensure_daemon could not start the daemon (the message has its log)
    - ``binary_not_found``: there is no localimage binary
    """

    def __init__(self, message: str, code: str = "internal", status: Optional[int] = None,
                 details: Optional[dict] = None):
        super().__init__(message)
        self.code = code
        self.status = status
        self.details = details


def _healthy(addr: str):
    """{"pid": int | None} when a daemon answers at addr (pid is None for an old daemon), else None."""
    try:
        with urllib.request.urlopen(f"http://{addr}/healthz", timeout=1.5) as r:
            if r.status != 200:
                return None
            try:
                return {"pid": json.loads(r.read() or b"{}").get("pid")}
            except ValueError:
                return {"pid": None}
    except (OSError, ValueError):
        return None


def ensure_daemon(addr: str = DEFAULT_ADDR, api_key: Optional[str] = None, env: Optional[dict] = None,
                  timeout: float = 30.0, log_file: Optional[str] = None) -> dict:
    """Make sure a daemon is running at ``addr``, starting one if needed, and wait until it answers.

    An already-running daemon is reused. A daemon started here is detached and keeps running after
    this process exits, so parallel workers and later runs share it.

    Returns ``{"started": bool, "addr": str, "pid": int, "log_file": str}`` (the last two only when
    started). Raises ``LocalImageError`` with ``daemon_start_failed``, ``docker_unavailable`` (the
    daemon could not reach Docker) or ``binary_not_found``. ``env`` is passed to a daemon started
    here, for example ``{"LOCALIMAGE_MAX_SESSIONS": "20"}``; it does not affect a running one.
    """
    if _healthy(addr):
        return {"started": False, "addr": addr}

    binary = find_binary()
    if not binary:
        raise LocalImageError("no localimage binary found; install a platform wheel or set LOCALIMAGE_BIN",
                              code="binary_not_found")
    make_executable(binary)

    log = log_file or os.path.join(
        tempfile.gettempdir(), "localimage-daemon-" + "".join(c if c.isalnum() or c in ".-" else "_" for c in addr) + ".log")
    child_env = {**os.environ, **(env or {})}
    if api_key:
        child_env["LOCALIMAGE_API_KEY"] = api_key
    kwargs: dict = {}
    if sys.platform == "win32":
        kwargs["creationflags"] = 0x00000008 | 0x00000200  # DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
    else:
        kwargs["start_new_session"] = True
    with open(log, "ab") as out:
        try:
            proc = subprocess.Popen([binary, "start", "-addr", addr], stdin=subprocess.DEVNULL,
                                    stdout=out, stderr=out, env=child_env, **kwargs)
        except OSError as e:
            raise LocalImageError(f"could not run {binary}: {e}", code="daemon_start_failed") from None

    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        h = _healthy(addr)
        if h:
            # Several processes may start a daemon at once; only one binds the port and the others
            # exit. `started` is true only for the process whose daemon is the one answering.
            if h["pid"] is None or h["pid"] == proc.pid:
                # The daemon is detached on purpose: stop Popen from warning when it is collected.
                proc.returncode = 0
                return {"started": True, "addr": addr, "pid": proc.pid, "log_file": log}
            return {"started": False, "addr": addr}
        if proc.poll() is not None:
            # Our process exited. If another process started a daemon at the same moment, that
            # one answers; otherwise this really failed.
            time.sleep(0.3)
            if _healthy(addr):
                return {"started": False, "addr": addr}
            break
        time.sleep(0.1)

    if proc.poll() is None:  # never became healthy: do not leave it running
        proc.terminate()
        try:
            proc.wait(5)
        except subprocess.TimeoutExpired:
            proc.kill()
    try:
        with open(log, "r", errors="replace") as f:
            tail = "\n".join(f.read().strip().splitlines()[-6:])
    except OSError:
        tail = ""
    docker_down = "cannot reach docker" in tail.lower()
    raise LocalImageError(
        f"{'Docker is not available' if docker_down else 'the localimage daemon did not start'} "
        f"at {addr} (log {log})" + (f":\n{tail}" if tail else ""),
        code="docker_unavailable" if docker_down else "daemon_start_failed")


@dataclass
class Session:
    id: str
    browser: str
    protocol: str  # "cdp" (chromium) or "playwright" (firefox)
    connect_url: str
    live_url: str  # interactive: can control the browser
    view_url: str  # read-only: watch only
    profile: Optional[str]
    extensions: list
    tags: dict
    downloads_dir: str  # where the browser saves downloads; read files with download_file()
    expires_at: str
    _client: "LocalImage"

    def connect(self, playwright):
        """Attach a Playwright instance (from sync_playwright()) to this session."""
        if self.protocol == "cdp":
            return playwright.chromium.connect_over_cdp(self.connect_url)
        return getattr(playwright, self.browser).connect(self.connect_url)

    def screenshot(self, region: Optional[dict] = None) -> bytes:
        """PNG of the display; pass {"x", "y", "width", "height"} to crop."""
        q = ("?" + urllib.parse.urlencode(region)) if region else ""
        return self._client._raw("GET", f"/v1/sessions/{self.id}/screenshot{q}")

    # Computer controls act on the whole display (screen coordinates), not a page.
    def _computer(self, action: str, body: dict) -> None:
        self._client._call("POST", f"/v1/sessions/{self.id}/computer/{action}", body)

    def click(self, x: int, y: int, button: str = "left", num_clicks: int = 1) -> None:
        self._computer("click", {"x": x, "y": y, "button": button, "num_clicks": num_clicks})

    def move_mouse(self, x: int, y: int) -> None:
        self._computer("move", {"x": x, "y": y})

    def type(self, text: str, delay_ms: int = 0) -> None:
        self._computer("type", {"text": text, "delay_ms": delay_ms})

    def press_key(self, *keys: str) -> None:
        self._computer("key", {"keys": list(keys)})

    def scroll(self, x: int, y: int, delta_x: int = 0, delta_y: int = 0) -> None:
        self._computer("scroll", {"x": x, "y": y, "delta_x": delta_x, "delta_y": delta_y})

    def drag(self, path: list, button: str = "left") -> None:
        self._computer("drag", {"path": path, "button": button})

    def start_recording(self, max_seconds: Optional[int] = None) -> dict:
        """Begin a recording (video/mp4); returns its info, with "replay_id" and "started_at". One can
        run at a time. It stops by itself after max_seconds (default 3600)."""
        body = {"max_seconds": max_seconds} if max_seconds else None
        return self._client._call("POST", f"/v1/sessions/{self.id}/recording/start", body)

    def stop_recording(self, replay_id: Optional[str] = None) -> dict:
        body = {"replay_id": replay_id} if replay_id else None
        return self._client._call("POST", f"/v1/sessions/{self.id}/recording/stop", body)

    def recordings(self) -> list:
        return self._client._call("GET", f"/v1/sessions/{self.id}/recordings")

    def download_recording(self, replay_id: Optional[str] = None) -> bytes:
        """MP4 bytes of a recording; the latest if replay_id is omitted."""
        path = f"/recordings/{replay_id}" if replay_id else "/recording"
        return self._client._raw("GET", f"/v1/sessions/{self.id}{path}")

    def exec(self, command: list, cwd: Optional[str] = None, timeout_seconds: int = 0) -> dict:
        """Run a command in the session container as the browser user (no shell)."""
        return self._client._call("POST", f"/v1/sessions/{self.id}/process/exec",
                                  {"command": command, "cwd": cwd or "", "timeout_seconds": timeout_seconds})

    def upload_file(self, path: str, data: bytes) -> dict:
        q = urllib.parse.urlencode({"path": path})
        return json.loads(self._client._open("PUT", f"/v1/sessions/{self.id}/files?{q}", data=data))

    def download_file(self, path: str) -> bytes:
        q = urllib.parse.urlencode({"path": path})
        return self._client._raw("GET", f"/v1/sessions/{self.id}/files?{q}")

    def list_files(self, path: str) -> list:
        q = urllib.parse.urlencode({"path": path})
        return self._client._call("GET", f"/v1/sessions/{self.id}/files/list?{q}")

    def logs(self, source: Optional[str] = None, tail: Optional[int] = None) -> str:
        """The end of one of the session's logs: source is "container" (default), "chromium",
        "firefox", "neko", "xorg", "openbox", "pulseaudio" or "cdp-forward"; tail is a number of lines
        (default 200). Works for a stopped session too, to see why it died."""
        q = urllib.parse.urlencode({k: v for k, v in (("source", source), ("tail", tail)) if v})
        return self._client._raw("GET", f"/v1/sessions/{self.id}/logs" + (f"?{q}" if q else "")).decode("utf-8", "replace")

    def health(self) -> dict:
        """Whether the browser is up and whether it has restarted: state, health, starts, restarts,
        last_started_at. After a crash the browser comes back by itself on the same URL, but as a
        new, empty browser: "restarts" going up is how a client finds out."""
        return self._client._call("GET", f"/v1/sessions/{self.id}/health")

    def update_tags(self, set: Optional[dict] = None, remove: Optional[list] = None) -> dict:
        """Change tags after creation. Returns the resulting tags and updates ``tags`` on this object."""
        r = self._client._call("PATCH", f"/v1/sessions/{self.id}/tags", {"set": set or {}, "remove": remove or []})
        self.tags = r["tags"]
        return r["tags"]

    def save_logs(self, directory: str) -> list:
        """Write every log source to <directory>/<id>/<source>.log and return the paths. A session's
        logs die with its container, so call this when a test fails, before close()."""
        target = os.path.join(directory, self.id)
        os.makedirs(target, exist_ok=True)
        written = []
        for source in ("container", "chromium", "firefox", "neko", "xorg", "openbox", "pulseaudio", "cdp-forward"):
            text = self.logs(source=source, tail=5000)
            if text.strip():
                path = os.path.join(target, f"{source}.log")
                with open(path, "w", encoding="utf-8") as f:
                    f.write(text)
                written.append(path)
        return written

    def close(self) -> bool:
        """Delete the session. Safe to call more than once; True if this call removed it."""
        return self._client.delete(self.id)

    def __enter__(self) -> "Session":
        return self

    def __exit__(self, *exc) -> None:
        self.close()


class LocalImage:
    """Client for the localimage daemon.

    ``auto_start`` starts the daemon on the first call if none is running (default: on when
    ``LOCALIMAGE_AUTOSTART=1``); ``daemon_env`` is the environment for a daemon started that way.
    """

    def __init__(self, addr: str = DEFAULT_ADDR, api_key: Optional[str] = None,
                 auto_start: Optional[bool] = None, daemon_env: Optional[dict] = None,
                 timeout: Optional[float] = 300.0):
        # `timeout` is the default for ordinary calls; it limits how long the connection may stay
        # silent. create() and prepare() default to no limit, since a first-run image build is one
        # long request. Pass timeout= to any of them to override.
        self.timeout = timeout
        self.addr = addr
        self.api_key = api_key
        self.auto_start = (os.environ.get("LOCALIMAGE_AUTOSTART") == "1") if auto_start is None else auto_start
        self.daemon_env = daemon_env or {}
        self._start_lock = threading.Lock()

    def _key(self) -> Optional[str]:
        """The key sent to the daemon: the api_key argument, else LOCALIMAGE_API_KEY, else the key
        file the daemon creates (read when a call is made, so a daemon started by auto_start works)."""
        return self.api_key or os.environ.get("LOCALIMAGE_API_KEY") or read_key_file()

    def ensure_daemon(self, **kwargs) -> dict:
        """Start the daemon if it is not running (see the module-level ensure_daemon)."""
        kwargs.setdefault("env", self.daemon_env)
        return ensure_daemon(addr=self.addr, api_key=self.api_key, **kwargs)

    def _open_once(self, method: str, path: str, body: Optional[dict] = None,
                   data: Optional[bytes] = None, timeout=_DEFAULT) -> bytes:
        timeout = self.timeout if timeout is _DEFAULT else timeout
        if data is None and body is not None:
            data = json.dumps(body).encode()
        req = urllib.request.Request(
            f"http://{self.addr}{path}", data=data, method=method,
            headers={"Content-Type": "application/octet-stream" if body is None and data is not None
                     else "application/json",
                     **({"Authorization": f"Bearer {self._key()}"} if self._key() else {})},
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.read()
        except urllib.error.HTTPError as e:
            try:
                b = json.loads(e.read() or b"{}")
            except ValueError:
                b = {}
            finally:
                e.close()
            raise LocalImageError(b.get("error", f"HTTP {e.code}"), code=b.get("code", f"http_{e.code}"),
                                  status=e.code, details=b.get("details")) from None
        except urllib.error.URLError as e:
            if isinstance(e.reason, TimeoutError):
                raise LocalImageError(f"{method} {path} did not answer within {timeout} s",
                                      code="request_timeout") from None
            raise LocalImageError(
                f"localimage daemon not reachable at {self.addr}; start it with `localimage start`, "
                f"or use ensure_daemon() / auto_start ({e.reason})", code="daemon_unreachable") from None
        except TimeoutError:
            raise LocalImageError(f"{method} {path} did not answer within {timeout} s",
                                  code="request_timeout") from None

    def _open(self, method: str, path: str, body: Optional[dict] = None,
              data: Optional[bytes] = None, timeout=_DEFAULT) -> bytes:
        try:
            return self._open_once(method, path, body, data, timeout)
        except LocalImageError as e:
            if e.code != "daemon_unreachable" or not self.auto_start:
                raise
            with self._start_lock:  # concurrent threads share one start-up
                self.ensure_daemon()
            return self._open_once(method, path, body, data, timeout)

    def _call(self, method: str, path: str, body: Optional[dict] = None, timeout=_DEFAULT):
        raw = self._open(method, path, body, timeout=timeout)
        return json.loads(raw) if raw else None

    def _raw(self, method: str, path: str) -> bytes:
        return self._open(method, path)

    def prepare(self, browsers: Optional[list] = None, on_progress=None, timeout: Optional[float] = None) -> dict:
        """Build the browser images that are missing, so the first session does not have to.

        The first Chromium launch builds an image (about a minute, longer for Firefox), which is
        more than a typical attempt budget: call this in a prepare phase, then create sessions with
        ``no_build=True`` so they fail fast with ``image_not_ready`` instead of building mid-attempt.

        Returns {"chromium": "ready" | "built", ...}. ``on_progress({"browser", "message"})`` is
        called with build steps. There is no timeout unless you pass ``timeout`` (seconds of silence;
        the daemon sends a keep-alive every 10 s).
        """
        body = json.dumps({"browsers": browsers or ["chromium"]}).encode()
        req = urllib.request.Request(
            f"http://{self.addr}/v1/images/prepare", data=body, method="POST",
            headers={"Content-Type": "application/json",
                     **({"Authorization": f"Bearer {self._key()}"} if self._key() else {})})
        last = None
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                for raw in r:
                    line = raw.decode("utf-8", "replace").strip()
                    if not line:
                        continue
                    try:
                        last = json.loads(line)
                    except ValueError:
                        continue
                    if last.get("event") == "progress" and on_progress:
                        on_progress({"browser": last.get("browser"), "message": last.get("message")})
        except urllib.error.HTTPError as e:
            try:
                b = json.loads(e.read() or b"{}")
            finally:
                e.close()
            raise LocalImageError(b.get("error", f"HTTP {e.code}"), code=b.get("code", f"http_{e.code}"),
                                  status=e.code) from None
        except (urllib.error.URLError, TimeoutError) as e:
            reason = getattr(e, "reason", e)
            if isinstance(reason, TimeoutError):
                raise LocalImageError(f"prepare did not answer within {timeout} s", code="request_timeout") from None
            raise LocalImageError(f"localimage daemon not reachable at {self.addr}; start it with "
                                  f"`localimage start`, or use ensure_daemon() ({reason})",
                                  code="daemon_unreachable") from None
        if not last or last.get("event") == "ping":
            raise LocalImageError("the daemon closed the connection before the build finished",
                                  code="daemon_unreachable")
        if last.get("event") == "error":
            raise LocalImageError(last.get("error", "prepare failed"), code=last.get("code", "internal"))
        return last.get("images", {})

    def shutdown(self) -> bool:
        """Stop the daemon. Running sessions are not removed; they expire by TTL once a daemon runs
        again. Idempotent: True if a daemon was stopped, False if none was running."""
        try:
            self._open_once("POST", "/v1/shutdown")
            return True
        except LocalImageError as e:
            if e.code == "daemon_unreachable":
                return False
            raise

    def info(self) -> dict:
        """The daemon's version, limits and load:
        {"version", "max_sessions", "max_ttl_seconds", "running", "available"}."""
        return self._call("GET", "/v1/info")

    def assert_compatible(self, chromium=None, firefox=None) -> dict:
        """Raise ``incompatible_daemon`` unless the daemon speaks this SDK's API and, if given, its
        browser versions match (a string for an exact version, or a compiled regex). Returns info()."""
        info = self.info()
        bad = []
        if info.get("api_version") != SUPPORTED_API_VERSION:
            bad.append(f"its API version is {info.get('api_version') or 'unknown (an old daemon)'}, "
                       f"this SDK needs {SUPPORTED_API_VERSION}")
        for name, want in (("chromium", chromium), ("firefox", firefox)):
            if want is None:
                continue
            have = (info.get("components") or {}).get(name, {}).get("version", "")
            ok = want.search(have) if hasattr(want, "search") else have == str(want)
            if not ok:
                bad.append(f"{name} is {have or 'unknown'}, expected {getattr(want, 'pattern', want)}")
        if bad:
            raise LocalImageError(f"the daemon at {self.addr} (version {info.get('version')}) is not "
                                  f"compatible: {'; '.join(bad)}", code="incompatible_daemon",
                                  details={"apiVersion": info.get("api_version")})
        return info

    def assert_capacity(self, n: int) -> dict:
        """Fail early, with a clear error, when ``n`` sessions could never run at once.

        Call it before starting a pool of workers. It checks the daemon's limit
        (LOCALIMAGE_MAX_SESSIONS), not the sessions free right now: others may finish, and
        ``create(wait_seconds=...)`` queues for a slot. Returns ``info()``; raises
        ``LocalImageError`` with ``limit_exceeded`` and details {"max", "requested"}.
        """
        info = self.info()
        if n > info["max_sessions"]:
            raise LocalImageError(
                f"{n} concurrent sessions were requested but the daemon at {self.addr} allows "
                f"{info['max_sessions']}; restart it with LOCALIMAGE_MAX_SESSIONS={n} (or higher), "
                "or use fewer workers", code="limit_exceeded",
                details={"max": info["max_sessions"], "requested": n})
        return info

    def create(self, browser: str = "chromium", screen: Optional[str] = None,
               ttl_seconds: Optional[int] = None, profile: Optional[str] = None,
               memory_mb: Optional[int] = None, cpus: Optional[float] = None,
               kiosk: bool = False, extensions: Optional[list] = None,
               tags: Optional[Dict[str, str]] = None, wait_seconds: Optional[int] = None,
               no_build: bool = False, timeout: Optional[float] = None,
               webgl: bool = False, timezone: Optional[str] = None, locale: Optional[str] = None) -> Session:
        """Start a browser session.

        ``tags`` is {key: value} metadata stored on the session, returned by ``list()`` and usable
        as a list filter (up to 20 tags; keys are letters, digits, '.', '_' and '-').
        ``wait_seconds`` queues for a free session slot instead of failing at once with
        ``limit_exceeded``. ``webgl=True`` enables WebGL (software rendering, Chromium only);
        ``timezone`` (an IANA name such as ``Europe/Berlin``) and ``locale`` (such as ``de-DE``,
        Chromium only) set the browser's timezone and language. ``no_build=True`` fails with ``image_not_ready`` instead of building a
        missing browser image (see ``prepare()``). ``timeout`` limits how long the call may stay
        silent (default: no limit, because a first-run image build is one long request).
        """
        s = self._call("POST", "/v1/sessions", {
            "browser": browser, "screen": screen or "", "ttl_seconds": ttl_seconds or 0,
            "profile": profile or "", "memory_mb": memory_mb or 0, "cpus": cpus or 0,
            "kiosk": kiosk, "extensions": extensions or [], "tags": tags or {},
            "wait_seconds": wait_seconds or 0, "no_build": no_build,
            "webgl": webgl, "timezone": timezone or "", "locale": locale or ""}, timeout=timeout)
        return self._session(s)

    def _session(self, s: dict) -> Session:
        return Session(s["id"], s["browser"], s["protocol"], s["connect_url"], s["live_url"],
                       s["view_url"], s.get("profile"), s.get("extensions") or [], s.get("tags") or {},
                       s.get("downloads_dir", "/home/neko/Downloads"), s["expires_at"], self)

    def attach(self, session_id: str) -> Session:
        """A Session for one that already exists, by id: from another process, after a restart, or
        from a stored lease. Raises ``session_gone`` if it is not there. A stopped session can be
        attached too, to read its logs."""
        return self._session(self._call("GET", f"/v1/sessions/{session_id}"))

    def list(self, tags: Optional[Dict[str, str]] = None) -> list:
        """List sessions in every state (a crashed worker can leave stopped containers), as the
        daemon returns them: id, browser, protocol, state, tags, created_at, expires_at, ...
        ``tags`` keeps only sessions that carry all of those tags."""
        q = urllib.parse.urlencode([("tag", f"{k}={v}") for k, v in (tags or {}).items()])
        return self._call("GET", "/v1/sessions" + (f"?{q}" if q else ""))

    def delete(self, session_id: str) -> bool:
        """Delete a session. Idempotent: True if this call removed it, False if it was already
        gone (deleted, expired, or never existed), so releasing a lease twice, or after a failed
        acquire, never raises. Other failures, such as ``daemon_unreachable``, still raise."""
        try:
            self._call("DELETE", f"/v1/sessions/{session_id}")
            return True
        except LocalImageError as e:
            if e.code == "session_gone":
                return False
            raise

    def prune(self) -> list:
        """Remove browser images left behind by older versions. Returns the tags removed
        (info()["stale_images"] says how many there are)."""
        return self._call("POST", "/v1/images/prune")["removed"]

    def upload_extension(self, name: str, zip_bytes: bytes) -> dict:
        """Upload an unpacked Chromium extension as a zip (manifest.json at the root or in one folder)."""
        q = urllib.parse.urlencode({"name": name})
        return json.loads(self._open("POST", f"/v1/extensions?{q}", data=zip_bytes))

    def extensions(self) -> list:
        return self._call("GET", "/v1/extensions")

    def delete_extension(self, name: str) -> None:
        self._call("DELETE", f"/v1/extensions/{name}")

    def profiles(self) -> list:
        return self._call("GET", "/v1/profiles")

    def delete_profile(self, name: str) -> None:
        self._call("DELETE", f"/v1/profiles/{name}")
