"""Daemon lifecycle, tags, idempotent release and concurrency for the Python SDK.

Needs Docker. Starts its own daemon on a separate port (through ensure_daemon, which is itself
under test) with a session limit of 2. Run from packages/python:

    PYTHONPATH=src python3 -m unittest discover -s tests -v
"""
import os
import subprocess
import sys
import threading
import time
import unittest
import urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from localimage import LocalImage, LocalImageError, ensure_daemon  # noqa: E402

ADDR = "127.0.0.1:7794"
MAX = 2
ENV = {"LOCALIMAGE_MAX_SESSIONS": str(MAX)}

# In a checkout there is no bundled binary; use the one built at the repo root.
_built = os.path.join(os.path.dirname(__file__), "..", "..", "..", "bin", "localimage")
if not os.environ.get("LOCALIMAGE_BIN") and os.path.exists(_built):
    os.environ["LOCALIMAGE_BIN"] = os.path.abspath(_built)


def _docker_ok() -> bool:
    try:
        subprocess.run(["docker", "version", "--format", "{{.Server.Version}}"], check=True,
                       capture_output=True, timeout=20)
        return True
    except (OSError, subprocess.SubprocessError):
        return False


def _up() -> bool:
    try:
        with urllib.request.urlopen(f"http://{ADDR}/healthz", timeout=1.5):
            return True
    except OSError:
        return False


def _wait_down() -> None:
    for _ in range(50):
        if not _up():
            return
        time.sleep(0.1)


@unittest.skipUnless(_docker_ok(), "Docker is not available")
class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.li = LocalImage(addr=ADDR)
        ensure_daemon(addr=ADDR, env=ENV)  # idempotent, so tests do not depend on order

    @classmethod
    def tearDownClass(cls):
        li = LocalImage(addr=ADDR)
        if _up():
            for s in li.list():
                li.delete(s["id"])
            li.shutdown()

    def test_unreachable_daemon_has_a_stable_code(self):
        dead = LocalImage(addr="127.0.0.1:1")
        with self.assertRaises(LocalImageError) as cm:
            dead.list()
        self.assertEqual(cm.exception.code, "daemon_unreachable")
        self.assertIsInstance(cm.exception, RuntimeError, "still catchable as RuntimeError")
        with self.assertRaises(LocalImageError) as cm:
            dead.delete("abcd1234")  # a dead daemon is a real failure, not "gone"
        self.assertEqual(cm.exception.code, "daemon_unreachable")

    def test_ensure_daemon_starts_reuses_and_shutdown_is_idempotent(self):
        self.assertTrue(self.li.shutdown() in (True, False))
        _wait_down()
        self.assertFalse(_up())
        self.assertFalse(self.li.shutdown(), "stopping a stopped daemon is a no-op")

        started = ensure_daemon(addr=ADDR, env=ENV)
        self.assertTrue(started["started"])
        self.assertGreater(started["pid"], 0)
        self.assertTrue(_up())
        self.assertFalse(ensure_daemon(addr=ADDR, env=ENV)["started"], "a running daemon is reused")

        info = self.li.info()
        self.assertEqual(info["max_sessions"], MAX, "the env reached the daemon")
        self.assertEqual(info["running"], 0, "no sessions running: another test left some behind?")

    def test_auto_start_restarts_a_stopped_daemon(self):
        self.li.shutdown()
        _wait_down()
        clients = [LocalImage(addr=ADDR, auto_start=True, daemon_env=ENV) for _ in range(3)]
        results, errors = [], []

        def go(c):
            try:
                results.append(c.info()["max_sessions"])
            except Exception as e:  # noqa: BLE001
                errors.append(e)

        threads = [threading.Thread(target=go, args=(c,)) for c in clients]
        [t.start() for t in threads]
        [t.join() for t in threads]
        self.assertEqual(errors, [])
        self.assertEqual(results, [MAX] * 3, "racing clients end up sharing one daemon")

    def test_docker_unavailable_is_reported_with_a_code(self):
        with self.assertRaises(LocalImageError) as cm:
            ensure_daemon(addr="127.0.0.1:7795", timeout=15,
                          env={"DOCKER_HOST": "unix:///nonexistent-docker.sock"})
        self.assertEqual(cm.exception.code, "docker_unavailable")
        self.assertIn("nonexistent-docker.sock", str(cm.exception))

    def test_tags_filter_and_sweep(self):
        a = self.li.create(ttl_seconds=600, tags={"e2e_run": "runA", "e2e_target": "t1"})
        b = self.li.create(ttl_seconds=600, tags={"e2e_run": "runB", "e2e_target": "t1"})
        try:
            self.assertEqual(a.tags, {"e2e_run": "runA", "e2e_target": "t1"})

            def ids(**tags):
                return sorted(s["id"] for s in self.li.list(tags=tags))

            self.assertEqual(ids(e2e_run="runA"), [a.id])
            self.assertEqual(ids(e2e_target="t1"), sorted([a.id, b.id]))
            self.assertEqual(ids(e2e_run="runA", e2e_target="t2"), [], "tags are ANDed")
            [listed] = self.li.list(tags={"e2e_run": "runB"})
            self.assertEqual((listed["tags"], listed["state"]), ({"e2e_run": "runB", "e2e_target": "t1"}, "running"))

            # A dead worker's browser container is left stopped; the sweep must still find it.
            subprocess.run(["docker", "stop", "-t", "1", f"localimage-{a.id}"], check=True, capture_output=True)
            [stopped] = self.li.list(tags={"e2e_run": "runA"})
            self.assertEqual(stopped["state"], "exited")
            self.assertTrue(self.li.delete(stopped["id"]))
            self.assertEqual(ids(e2e_run="runA"), [])
            self.assertEqual(ids(e2e_run="runB"), [b.id], "the other run is untouched")

            for bad in ({"bad key": "x"}, {"k": "x" * 257}, {"k": "a\nb"}):
                with self.assertRaises(LocalImageError) as cm:
                    self.li.create(tags=bad)
                self.assertEqual(cm.exception.code, "invalid_request")
        finally:
            a.close()
            b.close()

    def test_delete_is_idempotent(self):
        s = self.li.create(ttl_seconds=300)
        self.assertIs(s.close(), True)
        self.assertIs(s.close(), False)
        self.assertIs(self.li.delete("ffffffff"), False)

        s2 = self.li.create(ttl_seconds=300)
        out = []
        threads = [threading.Thread(target=lambda: out.append(s2.close())) for _ in range(2)]
        [t.start() for t in threads]
        [t.join() for t in threads]
        self.assertEqual(sorted(out), [False, True], "racing deletes: one removes it, neither raises")

        with self.assertRaises(LocalImageError) as cm:
            self.li._call("DELETE", f"/v1/sessions/{s2.id}")
        self.assertEqual((cm.exception.code, cm.exception.status), ("session_gone", 404))

    def test_limit_is_readable_enforced_and_can_queue(self):
        info = self.li.info()
        self.assertEqual((info["max_sessions"], info["available"]), (MAX, MAX))
        self.assertEqual(self.li.assert_capacity(MAX)["max_sessions"], MAX)
        with self.assertRaises(LocalImageError) as cm:
            self.li.assert_capacity(MAX + 1)
        self.assertEqual(cm.exception.code, "limit_exceeded")
        self.assertEqual(cm.exception.details, {"max": MAX, "requested": MAX + 1})
        self.assertIn(f"LOCALIMAGE_MAX_SESSIONS={MAX + 1}", str(cm.exception))

        held = []
        try:
            outcome = []

            def grab():
                try:
                    outcome.append(self.li.create(ttl_seconds=600))
                except LocalImageError as e:
                    outcome.append(e)

            threads = [threading.Thread(target=grab) for _ in range(MAX + 2)]  # a burst over the limit
            [t.start() for t in threads]
            [t.join() for t in threads]
            held = [o for o in outcome if not isinstance(o, Exception)]
            refused = [o for o in outcome if isinstance(o, Exception)]
            self.assertEqual((len(held), len(refused)), (MAX, 2), "exactly MAX granted under a burst")
            for e in refused:
                self.assertEqual((e.code, e.status, e.details["max"]), ("limit_exceeded", 429, MAX))

            start = time.monotonic()
            with self.assertRaises(LocalImageError) as cm:
                self.li.create(wait_seconds=2)
            self.assertEqual(cm.exception.code, "limit_exceeded")
            self.assertGreaterEqual(time.monotonic() - start, 1.8, "it did wait")

            queued = []
            t = threading.Thread(target=lambda: queued.append(self.li.create(ttl_seconds=600, wait_seconds=120)))
            t.start()
            time.sleep(2.5)
            self.assertEqual(queued, [], "still queued while every slot is taken")
            held.pop().close()
            t.join(120)
            self.assertEqual(len(queued), 1, "the queued create got the freed slot")
            held.append(queued[0])
        finally:
            for s in held:
                s.close()
        self.assertEqual(self.li.info()["available"], MAX, "every slot is returned")

    def test_prepare_no_build_images_and_downloads_dir(self):
        self.assertTrue(self.li.info()["images"]["chromium"], "the chromium image is built")
        seen = []
        result = self.li.prepare(["chromium"], on_progress=seen.append)
        self.assertEqual(result["chromium"], "ready")
        self.assertTrue(any(e["browser"] == "chromium" for e in seen), "progress is reported")
        with self.assertRaises(LocalImageError) as cm:
            self.li.prepare(["netscape"])
        self.assertEqual(cm.exception.code, "invalid_request")

        with self.li.create(ttl_seconds=300, no_build=True) as s:
            self.assertEqual(s.downloads_dir, "/home/neko/Downloads")
            self.assertEqual(s.exec(["stat", "-c", "%U", s.downloads_dir])["stdout"].strip(), "neko")
            s.upload_file(s.downloads_dir + "/x.txt", b"hello")
            self.assertEqual(s.download_file(s.downloads_dir + "/x.txt"), b"hello")

    def test_recording_has_timestamps_and_media_details(self):
        with self.li.create(ttl_seconds=300) as s:
            started = s.start_recording()
            self.assertEqual(started["content_type"], "video/mp4")
            time.sleep(3)
            stopped = s.stop_recording()
            self.assertEqual((stopped["codec"], stopped["pix_fmt"], stopped["width"], stopped["height"]),
                             ("h264", "yuv420p", 1440, 900))
            wall = stopped["stopped_at_ms"] - stopped["started_at_ms"]
            self.assertLess(abs(stopped["duration_ms"] - wall), 600)
            self.assertEqual(len(s.download_recording(started["replay_id"])), stopped["size"])

    def test_timeout_is_request_timeout_not_unreachable(self):
        import socket
        srv = socket.socket()
        srv.bind(("127.0.0.1", 0))
        srv.listen(1)  # accepts connections but never answers
        try:
            slow = LocalImage(addr="127.0.0.1:%d" % srv.getsockname()[1], timeout=0.5)
            with self.assertRaises(LocalImageError) as cm:
                slow.info()
            self.assertEqual(cm.exception.code, "request_timeout")
        finally:
            srv.close()


if __name__ == "__main__":
    unittest.main()
