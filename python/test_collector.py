"""Python v2 client tests — mirrors the Go v2 suite's pins."""

import base64
import gzip
import json
import unittest

import collector


CONFIG = {
    "pacing_ms": 1,
    "breaker": {"threshold_403": 5, "cooldown_s": 1},
    "overflow_bytes": 250000,
    "poll": {"live_wait_s": 8, "bulk_wait_s": 2, "idle_backoff_s": 1},
    "min_client_version": "2.0.0",
    "gateway": {"name": "t", "channel": "bulk", "status": "active"},
    "update": {},
}


def make(api_script, cr_result):
    """api_script: list of (status, body) consumed per call; records calls."""
    calls = []

    def api_call(method, route, body=None):
        calls.append((method, route, body))
        return api_script.pop(0)

    c = collector.Collector(
        "http://door",
        "emcg_t",
        "cr_t",
        sleep=lambda s: None,
        now=lambda: 1000.0,
        api_call=api_call,
        cr_fetch=lambda path: cr_result(path),
    )
    return c, calls


class V2Tests(unittest.TestCase):
    def test_lease_fetch_submit(self):
        seen_paths = []

        def cr(path):
            seen_paths.append(path)
            return ("http", 200, '{"tag":"#20JJJ2CCRU"}', None)

        c, calls = make(
            [
                (200, CONFIG),
                (
                    200,
                    {
                        "job": {"endpoint": "player", "entity_key": "#20JJJ2CCRU", "lane": "bulk"},
                        "cr_path": "/players/%2320JJJ2CCRU",
                        "lease": "sig.ned",
                    },
                ),
                (200, {"ok": True}),
            ],
            cr,
        )
        c.load_config()
        self.assertEqual(c.poll_once(), "job")
        self.assertEqual(seen_paths, ["/players/%2320JJJ2CCRU"], "server cr_path used verbatim")
        method, route, submit = calls[-1]
        self.assertEqual((method, route), ("POST", "/submit"))
        self.assertEqual(submit["status"], "ok")
        self.assertEqual(submit["lease"], "sig.ned")
        body = gzip.decompress(base64.b64decode(submit["body_gzip_b64"])).decode()
        self.assertEqual(json.loads(body)["tag"], "#20JJJ2CCRU")

    def test_empty_and_refused(self):
        c, _ = make([(200, CONFIG), (200, {"empty": True}), (429, {"error": "lease_cap"})],
                    lambda p: ("http", 200, "{}", None))
        c.load_config()
        self.assertEqual(c.poll_once(), "empty")
        self.assertEqual(c.poll_once(), "refused")

    def test_breaker_opens_after_five_403s(self):
        script = [(200, CONFIG)]
        for _ in range(5):
            script.append((200, {"job": {"endpoint": "player", "entity_key": "#2YG98VVQ", "lane": "bulk"},
                                 "cr_path": "/p", "lease": "x.y"}))
            script.append((200, {"ok": True}))
        c, calls = make(script, lambda p: ("http", 403, "", None))
        c.load_config()
        for _ in range(5):
            self.assertEqual(c.poll_once(), "job")
        self.assertEqual(c.poll_once(), "breaker_open")
        last_submit = [b for m, r, b in calls if r == "/submit"][-1]
        self.assertEqual(last_submit["status"], "error")
        self.assertEqual(last_submit["error"]["kind"], "http")

    def test_429_holds_pace(self):
        c, _ = make(
            [
                (200, CONFIG),
                (200, {"job": {"endpoint": "player", "entity_key": "#2YG98VVQ", "lane": "bulk"},
                       "cr_path": "/p", "lease": "x.y"}),
                (200, {"ok": True}),
            ],
            lambda p: ("http", 429, "", 7),
        )
        c.load_config()
        c.poll_once()
        self.assertGreaterEqual(
            c.last_fetch_started, 1000.0 + 7 - CONFIG["pacing_ms"] / 1000,
            "the named cooldown pushes the pace anchor forward",
        )


class OverflowTests(unittest.TestCase):
    """Collector issue #1: the transport overflow is judged on the ENCODED
    size, a raw ceiling stays distinct, and an overflow is a counted error."""

    def _run(self, body_text):
        c, calls = make(
            [
                (200, CONFIG),
                (
                    200,
                    {
                        "job": {"endpoint": "player_battlelog", "entity_key": "#20JJJ2CCRU", "lane": "bulk"},
                        "cr_path": "/players/%2320JJJ2CCRU/battlelog",
                        "lease": "7",
                    },
                ),
                (200, {"ok": True}),
            ],
            lambda path: ("http", 200, body_text, None),
        )
        c.load_config()
        c.poll_once()
        submit = next(b for m, r, b in calls if r == "/submit")
        return c, submit

    def test_large_but_compressible_body_fits_after_encoding(self):
        c, submit = self._run("x" * 311_100)  # raw > 250 KB, tiny once gzipped
        self.assertEqual(submit["status"], "ok")
        self.assertLess(len(submit["body_gzip_b64"]), CONFIG["overflow_bytes"])
        self.assertEqual(c.fetch_errors, 0)

    def test_true_encoded_overflow_is_an_error_and_counted(self):
        import random

        rnd = random.Random(1)
        body = "".join(chr(rnd.randrange(33, 127)) for _ in range(400_000))
        c, submit = self._run(body)
        self.assertEqual(submit["status"], "error")
        self.assertEqual(submit["error"]["kind"], "overflow")
        self.assertNotIn("body_gzip_b64", submit)
        self.assertEqual(c.fetch_errors, 1, "an overflow is a lost fetch")

    def test_raw_ceiling_is_distinct_and_explicit(self):
        c, submit = self._run("x" * (collector.MAX_RAW_BYTES + 1))
        self.assertEqual(submit["error"]["kind"], "overflow")
        self.assertEqual(c.fetch_errors, 1)


if __name__ == "__main__":
    unittest.main()
