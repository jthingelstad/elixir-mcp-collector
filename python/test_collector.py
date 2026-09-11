"""Python v2 client tests — mirrors the Go v2 suite's pins."""

import base64
import gzip
import json
import time
import unittest

import collector


CONFIG = {
    "pacing_ms": 1,
    "breaker": {"threshold_403": 5, "cooldown_s": 1},
    "overflow_bytes": 250000,
    "poll": {"live_wait_s": 8, "bulk_wait_s": 2, "idle_backoff_s": 1},
    "submit_retry": {"max_attempts": 3, "timeout_s": 20, "backoff_ms": 1},
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
        c, calls = make([(200, CONFIG), (200, {"empty": True}), (429, {"error": "lease_cap"})],
                        lambda p: ("http", 200, "{}", None))
        c.load_config()
        self.assertEqual(c.poll_once(), "empty")
        self.assertEqual(c.next_wait, 1, "a door that says nothing: the idle backoff")
        self.assertEqual(c.poll_once(), "refused")
        self.assertEqual(c.next_wait, 1)

    def test_check_in_follows_the_door(self):
        # Check-ins, not polling (2026-09-11): no wait_s in the request, and
        # next_check_in_s is the wait - 0 after a job, the interval on empty.
        c, calls = make(
            [
                (200, CONFIG),
                (200, {"job": {"endpoint": "player", "entity_key": "#20JJJ2CCRU", "lane": "live"},
                       "cr_path": "/players/%2320JJJ2CCRU", "lease": "1", "next_check_in_s": 0}),
                (200, {"ok": True}),
                (200, {"empty": True, "next_check_in_s": 15}),
                (429, {"error": "lease_cap", "next_check_in_s": 5}),
            ],
            lambda p: ("http", 200, '{"tag":"#20JJJ2CCRU"}', None),
        )
        c.load_config()
        self.assertEqual(c.poll_once(), "job")
        self.assertEqual(c.next_wait, 0, "more may remain: straight back")
        submit = [b for m, r, b in calls if (m, r) == ("POST", "/submit")][-1]
        self.assertEqual(submit["api_bytes"], len('{"tag":"#20JJJ2CCRU"}'))
        self.assertEqual(c.poll_once(), "empty")
        self.assertEqual(c.next_wait, 15)
        self.assertEqual(c.poll_once(), "refused")
        self.assertEqual(c.next_wait, 5)
        for m, r, b in calls:
            if r == "/lease":
                self.assertNotIn("wait_s", b, "a check-in never asks the door to wait")

    def test_submit_retries_a_transient_server_failure_with_same_lease(self):
        c, calls = make(
            [
                (200, CONFIG),
                (200, {"job": {"endpoint": "player", "entity_key": "#20JJJ2CCRU", "lane": "bulk"},
                       "cr_path": "/players/%2320JJJ2CCRU", "lease": "same-lease"}),
                (500, {"error": "ingest_failed"}),
                (200, {"ok": True}),
            ],
            lambda p: ("http", 200, "{}", None),
        )
        c.load_config()
        self.assertEqual(c.poll_once(), "job")
        submits = [body for method, route, body in calls if (method, route) == ("POST", "/submit")]
        self.assertEqual(len(submits), 2)
        self.assertEqual(submits[0], submits[1])

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


class BreakerConfigParityTests(unittest.TestCase):
    """The server owns the breaker; both runtimes must apply the SAME
    config the same way. The Go client used to ignore these two fields
    (collector issue #2) while this one honoured them — one config, two
    behaviours. These pin the Python half of the contract."""

    def _client(self, threshold, cooldown_s, clock):
        cfg = json.loads(json.dumps(CONFIG))
        cfg["breaker"] = {"threshold_403": threshold, "cooldown_s": cooldown_s}

        def api_call(method, route, body=None):
            if route.endswith("/config"):
                return (200, cfg)
            if route.endswith("/lease"):
                return (
                    200,
                    {
                        "job": {
                            "endpoint": "player",
                            "entity_key": "#2YG98VVQ",
                            "lane": "bulk",
                        },
                        "cr_path": "/players/%232YG98VVQ",
                        "lease": "x.y",
                    },
                )
            return (200, {"ok": True})

        c = collector.Collector(
            "http://door",
            "emcg_t",
            "cr_t",
            sleep=lambda s: None,
            now=lambda: clock[0],
            api_call=api_call,
            cr_fetch=lambda path: ("http", 403, "denied", None),
        )
        c.load_config()
        return c

    def test_threshold_and_cooldown_come_from_the_server(self):
        clock = [1000.0]
        c = self._client(2, 30, clock)
        self.assertEqual(c.poll_once(), "job")
        self.assertEqual(c.poll_once(), "job")
        # Two strikes, because the server said two - not five.
        self.assertEqual(c.poll_once(), "breaker_open")
        # And the cooldown is the server's 30s.
        clock[0] += 29
        self.assertEqual(c.poll_once(), "breaker_open")
        clock[0] += 1
        self.assertNotEqual(c.poll_once(), "breaker_open")

    def test_config_refresh_does_not_clear_an_open_breaker(self):
        clock = [1000.0]
        c = self._client(2, 300, clock)
        c.poll_once()
        c.poll_once()
        self.assertEqual(c.poll_once(), "breaker_open")
        c.load_config()  # the hourly refresh
        self.assertEqual(
            c.poll_once(),
            "breaker_open",
            "a config refresh must not hand a stopped collector its fetches back",
        )


CR_KEY = "eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzUxMiJ9.secretsecretsecret.Zt9c"
API_TOKEN = "emcg_secretsecretsecretsecret9f2c"


def healthy_body(status="active"):
    return {
        "pacing_ms": 1500,
        "gateway": {"name": "oracle-1", "card": "Goblin Barrel", "channel": "bulk", "status": status},
        "observed_ip": "132.145.0.9",
        "doctor": {"cr_path": "/locations?limit=1"},
    }


def run_doctor(config=(200, None, None), cr=("http", 200, '{"items":[]}', None),
               cr_token=CR_KEY, api_token=API_TOKEN, now=None, probed=None):
    status, body, date = config
    if body is None and status == 200:
        body = healthy_body()
    date = date or email_date(time.time())

    def config_call():
        if status == "down":
            raise OSError("connection refused")
        return status, body or {}, date

    def cr_fetch(path):
        if probed is not None:
            probed.append(path)
        return cr

    return collector.doctor_run(
        "/x/.env", False, cr_token, api_token, "http://door",
        config_call=config_call, cr_fetch=cr_fetch, now=now or time.time,
        host_arch=lambda: "x86_64",
    )


def email_date(ts):
    import email.utils
    return email.utils.formatdate(ts, usegmt=True)


class DoctorTests(unittest.TestCase):
    """Mirrors internal/doctor/doctor_test.go: same checks, same words."""

    def test_healthy_box_reads_identity_and_probes_the_server_named_path(self):
        probed = []
        r = run_doctor(probed=probed)
        self.assertEqual((r["verdict"], r["exit"]), ("healthy", 0))
        self.assertEqual(probed, ["/locations?limit=1"])
        txt = collector.doctor_text(r)
        for want in ("Goblin Barrel (oracle-1)", "active - leasing and submitting work",
                     "132.145.0.9", "skew"):
            self.assertIn(want, txt)

    def test_secrets_never_appear_in_either_mode(self):
        r = run_doctor()
        for out in (collector.doctor_text(r), json.dumps(r, ensure_ascii=False)):
            for secret in ("secretsecret", CR_KEY, API_TOKEN):
                self.assertNotIn(secret, out)
            self.assertIn("…Zt9c", out)
            self.assertIn("…9f2c", out)

    def test_pending_is_valid_but_not_yet_active(self):
        r = run_doctor(config=(200, healthy_body("pending"), None))
        self.assertEqual((r["verdict"], r["exit"]), ("not_yet_active", 2))
        self.assertIn("not yet promoted", collector.doctor_text(r))

    def test_unknown_and_revoked_tokens_are_told_apart(self):
        r = run_doctor(config=(401, {"error": "unauthenticated"}, None))
        self.assertEqual(r["exit"], 1)
        self.assertIn("does not recognise this token", collector.doctor_text(r))
        r = run_doctor(config=(403, {"error": "revoked", "hint": "This collector token was revoked by the maintainer; it will never work again."}, None))
        self.assertEqual(r["exit"], 1)
        self.assertIn("revoked by the maintainer", collector.doctor_text(r))

    def test_ip_mismatch_names_the_egress_and_the_fix(self):
        body = '{"reason":"accessDenied.invalidIp","message":"Invalid authorization: API key does not allow access from IP 132.145.0.9"}'
        r = run_doctor(cr=("http", 403, body, None))
        txt = collector.doctor_text(r)
        self.assertEqual(r["exit"], 1)
        for want in ("rejected this key (403 accessDenied.invalidIp)",
                     "your egress IP: 132.145.0.9",
                     "fix: add 132.145.0.9 to this key's allowed IPs"):
            self.assertIn(want, txt)

    def test_door_down_still_probes_the_key_with_the_default_path(self):
        probed = []
        r = run_doctor(config=("down", None, None), probed=probed)
        self.assertEqual(probed, [collector.DEFAULT_PROBE])
        self.assertEqual(r["exit"], 1)
        self.assertIn("unreachable", collector.doctor_text(r))

    def test_every_check_runs_even_when_the_first_fails(self):
        r = run_doctor(cr_token="")
        self.assertEqual(len(r["checks"]), 5)
        self.assertEqual(r["exit"], 1)
        self.assertIn("missing: CR_API_TOKEN", collector.doctor_text(r))
        self.assertTrue(r["checks"][2]["ok"], "the Elixir check must still run and pass")

    def test_clock_skew_is_a_warning(self):
        r = run_doctor(now=lambda: time.time() + 600)
        self.assertEqual(r["exit"], 0)
        self.assertIn("fix NTP", collector.doctor_text(r))

    def test_load_env_reports_where_it_looked(self):
        import os
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, ".env")
            os.environ["ELIXIR_MCP_ENV_FILE"] = path
            try:
                self.assertEqual(collector.load_env(), (path, False))
                with open(path, "w") as f:
                    f.write("DOCTOR_TEST_KEY=1\n")
                self.assertEqual(collector.load_env(), (path, True))
                self.assertEqual(os.environ.get("DOCTOR_TEST_KEY"), "1")
            finally:
                os.environ.pop("ELIXIR_MCP_ENV_FILE", None)
                os.environ.pop("DOCTOR_TEST_KEY", None)


LOG = ('[{"battleTime":"20260911T130000.000Z","type":"PvP"},'
       '{"battleTime":"20260911T123456.000Z","type":"PvP"},'
       '{"battleTime":"20260911T120000.000Z","type":"PvP"}]')


class FilterTests(unittest.TestCase):
    """Mirrors internal/filter/filter_test.go and the v2 lease-filter test."""

    def test_keeps_only_battles_after_the_mark(self):
        body, observed, dropped, applied = collector.filter_battlelog(LOG, "20260911T123456.000Z")
        self.assertEqual((observed, dropped, applied), (3, 2, True))
        self.assertEqual(json.loads(body), [{"battleTime": "20260911T130000.000Z", "type": "PvP"}])

    def test_nothing_new_is_an_empty_array_with_the_counts(self):
        body, observed, dropped, applied = collector.filter_battlelog(LOG, "20260911T130000.000Z")
        self.assertEqual((body, observed, dropped), ("[]", 3, 3))

    def test_everything_new_drops_nothing(self):
        _, observed, dropped, _ = collector.filter_battlelog(LOG, "20260911T110000.000Z")
        self.assertEqual((observed, dropped), (3, 0))

    def test_no_mark_or_no_array_leaves_the_body_alone(self):
        self.assertEqual(collector.filter_battlelog(LOG, ""), (LOG, 0, 0, False))
        err = '{"reason":"notFound"}'
        self.assertEqual(collector.filter_battlelog(err, "20260911T110000.000Z"), (err, 0, 0, False))

    def test_an_entry_without_battle_time_is_kept(self):
        body, _, dropped, _ = collector.filter_battlelog('[{"type":"odd"}]', "20260911T110000.000Z")
        self.assertEqual((json.loads(body), dropped), ([{"type": "odd"}], 0))

    def test_lease_filter_drops_battles_the_hub_holds_and_a_live_lease_submits_verbatim(self):
        leases = [
            (200, {"job": {"endpoint": "player_battlelog", "entity_key": "#20JJJ2CCRU", "lane": "bulk"},
                   "cr_path": "/players/%2320JJJ2CCRU/battlelog", "lease": "one",
                   "filter": {"battles_after": "20260911T123456.000Z"}}),
            (200, {"ok": True}),
            (200, {"job": {"endpoint": "player_battlelog", "entity_key": "#20JJJ2CCRU", "lane": "live"},
                   "cr_path": "/players/%2320JJJ2CCRU/battlelog", "lease": "two"}),
            (200, {"ok": True}),
        ]
        c, calls = make(leases, lambda path: ("http", 200, LOG, None))
        c.cfg = dict(CONFIG)
        c.poll_once()
        c.poll_once()
        submits = [b for (m, r, b) in calls if r == "/submit"]
        self.assertEqual(len(submits), 2)
        unzip = lambda s: gzip.decompress(base64.b64decode(s["body_gzip_b64"])).decode()
        self.assertEqual((submits[0]["observed"], submits[0]["filtered"]), (3, 2))
        self.assertEqual(json.loads(unzip(submits[0])), [{"battleTime": "20260911T130000.000Z", "type": "PvP"}])
        self.assertNotIn("observed", submits[1], "no filter on the lease, no counts on the submit")
        self.assertEqual(unzip(submits[1]), LOG, "unfiltered body must be verbatim")
