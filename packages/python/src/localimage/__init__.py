import json
import os
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Optional

DEFAULT_ADDR = os.environ.get("LOCALIMAGE_ADDR", "127.0.0.1:7777")


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

    def start_recording(self) -> dict:
        """Begin a recording; returns {"replay_id", "status"}. One can run at a time."""
        return self._client._call("POST", f"/v1/sessions/{self.id}/recording/start")

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

    def close(self) -> None:
        self._client.delete(self.id)

    def __enter__(self) -> "Session":
        return self

    def __exit__(self, *exc) -> None:
        self.close()


class LocalImage:
    def __init__(self, addr: str = DEFAULT_ADDR, api_key: Optional[str] = None):
        self.addr = addr
        self.api_key = api_key or os.environ.get("LOCALIMAGE_API_KEY")

    def _open(self, method: str, path: str, body: Optional[dict] = None,
              data: Optional[bytes] = None) -> bytes:
        if data is None and body is not None:
            data = json.dumps(body).encode()
        req = urllib.request.Request(
            f"http://{self.addr}{path}", data=data, method=method,
            headers={"Content-Type": "application/octet-stream" if body is None and data is not None
                     else "application/json",
                     **({"Authorization": f"Bearer {self.api_key}"} if self.api_key else {})},
        )
        try:
            with urllib.request.urlopen(req, timeout=300) as r:
                return r.read()
        except urllib.error.HTTPError as e:
            msg = json.loads(e.read() or b"{}").get("error", f"HTTP {e.code}")
            raise RuntimeError(msg) from None
        except urllib.error.URLError as e:
            raise RuntimeError(
                f"localimage daemon not reachable at {self.addr}; run `localimage start` ({e.reason})"
            ) from None

    def _call(self, method: str, path: str, body: Optional[dict] = None):
        raw = self._open(method, path, body)
        return json.loads(raw) if raw else None

    def _raw(self, method: str, path: str) -> bytes:
        return self._open(method, path)

    def create(self, browser: str = "chromium", screen: Optional[str] = None,
               ttl_seconds: Optional[int] = None, profile: Optional[str] = None,
               memory_mb: Optional[int] = None, cpus: Optional[float] = None,
               kiosk: bool = False, extensions: Optional[list] = None) -> Session:
        s = self._call("POST", "/v1/sessions", {
            "browser": browser, "screen": screen or "", "ttl_seconds": ttl_seconds or 0,
            "profile": profile or "", "memory_mb": memory_mb or 0, "cpus": cpus or 0,
            "kiosk": kiosk, "extensions": extensions or []})
        return Session(s["id"], s["browser"], s["protocol"], s["connect_url"], s["live_url"],
                       s["view_url"], s.get("profile"), s.get("extensions") or [], s["expires_at"], self)

    def list(self) -> list:
        return self._call("GET", "/v1/sessions")

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

    def delete(self, session_id: str) -> None:
        self._call("DELETE", f"/v1/sessions/{session_id}")
