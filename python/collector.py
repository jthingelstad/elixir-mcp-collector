#!/usr/bin/env python3
"""Zero-trust Elixir MCP collector, Python edition (COLLECTOR-ZERO-TRUST.md).

A deliberate runtime-diverse twin of the Go v2 client: stdlib only, no
AWS anything. Two secrets in .env next to this file (or $ELIXIR_MCP_ENV_FILE):

    CR_API_TOKEN=...        # your Clash Royale key (IP-bound by Supercell)
    ELIXIR_API_TOKEN=emcg_... # the token Elixir MCP issued for this collector
    ELIXIR_API_BASE=https://elixir.poapkings.com/api/collector  # optional

Loop: config -> lease -> fetch the server-named CR path (paced) -> submit.
The server owns pacing/breaker constants, the channel, and the CR path -
collection changes never require touching this file. No self-update: this
runs on machines we administer directly.
"""

import base64
import gzip
import json
import os
import sys
import time
import urllib.error
import urllib.request

CR_BASE = "https://api.clashroyale.com/v1"
VERSION = "py-2.0.0"
# Watchdog: with no successful door contact for this long, exit so the
# supervisor restarts clean (a wedged-but-alive process is invisible to
# launchd KeepAlive; automates the manual kickstart from the 2026-09-06
# phase-1-redeploy wedge).
WATCHDOG_TIMEOUT_S = 300


_last_progress = [time.time()]


def _touch_progress():
    _last_progress[0] = time.time()


def log(level, msg):
    print(json.dumps({"level": level, "msg": msg, "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}), flush=True)


def load_env():
    path = os.environ.get(
        "ELIXIR_MCP_ENV_FILE",
        os.path.join(os.path.dirname(os.path.abspath(__file__)), ".env"),
    )
    if os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                key, _, value = line.partition("=")
                os.environ.setdefault(key.strip(), value.strip().strip("\"'"))


def api(base, token, method, route, body=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + route,
        data=data,
        method=method,
        headers={
            "authorization": f"Bearer {token}",
            "content-type": "application/json",
            "x-collector-version": VERSION,
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as res:
            _touch_progress()
            return res.status, json.loads(res.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        _touch_progress()  # the door responded - not wedged
        try:
            return e.code, json.loads(e.read().decode() or "{}")
        except Exception:
            return e.code, {}


def fetch_cr(cr_token, path, timeout=15):
    """One CR API attempt: (kind, status, body_text, retry_after)."""
    req = urllib.request.Request(
        CR_BASE + path,
        headers={
            "authorization": f"Bearer {cr_token}",
            "user-agent": "Elixir-MCP-Gateway/py2",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as res:
            return "http", res.status, res.read().decode(), None
    except urllib.error.HTTPError as e:
        retry = e.headers.get("retry-after")
        return "http", e.code, "", int(retry) if retry and retry.isdigit() else None
    except Exception:
        return "transport", None, "", None


class Collector:
    def __init__(self, base, api_token, cr_token, sleep=time.sleep, now=time.time,
                 api_call=None, cr_fetch=None):
        self.base = base
        self.api_token = api_token
        self.cr_token = cr_token
        self.sleep = sleep
        self.now = now
        self.api = api_call or (lambda m, r, b=None: api(base, api_token, m, r, b))
        self.cr = cr_fetch or (lambda path: fetch_cr(cr_token, path))
        self.cfg = None
        self.last_fetch_started = 0.0
        self.consecutive_403 = 0
        self.breaker_open_until = 0.0

    def load_config(self):
        status, cfg = self.api("GET", "/config")
        if status != 200:
            raise RuntimeError(f"config refused: HTTP {status}")
        self.cfg = cfg
        log("info", f"config: channel={cfg['gateway']['channel']} pacing={cfg['pacing_ms']}ms")

    def pace(self):
        wait = self.cfg["pacing_ms"] / 1000 - (self.now() - self.last_fetch_started)
        if wait > 0:
            self.sleep(wait)
        self.last_fetch_started = self.now()

    def poll_once(self):
        """Returns 'empty' | 'job' | 'breaker_open' | 'refused'."""
        if self.now() < self.breaker_open_until:
            self.sleep(self.cfg["breaker"]["cooldown_s"])
            return "breaker_open"
        channel = self.cfg["gateway"]["channel"]
        wait = self.cfg["poll"]["live_wait_s" if channel == "live" else "bulk_wait_s"]
        status, lease = self.api("POST", "/lease", {"wait_s": wait})
        if status in (401, 409, 429):
            log("warn", f"lease refused HTTP {status} {lease.get('error', '')}")
            self.sleep(self.cfg["poll"]["idle_backoff_s"])
            return "refused"
        if lease.get("empty") or not lease.get("lease"):
            return "empty"

        self.pace()
        kind, http_status, body_text, retry_after = self.cr(lease["cr_path"])
        fetched_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        if kind == "http" and http_status == 429:
            hold = retry_after or 60
            self.last_fetch_started = self.now() + hold - self.cfg["pacing_ms"] / 1000
            log("warn", f"429 from the CR API; holding fetches {hold}s")
        if kind == "http" and http_status == 403:
            self.consecutive_403 += 1
            if self.consecutive_403 >= self.cfg["breaker"]["threshold_403"]:
                self.breaker_open_until = self.now() + self.cfg["breaker"]["cooldown_s"]
                log("warn", "circuit breaker OPEN after consecutive 403s")
        elif kind == "http" and http_status == 200:
            self.consecutive_403 = 0

        submit = {"lease": lease["lease"], "fetched_at": fetched_at}
        if kind == "http" and http_status == 200:
            if len(body_text) > self.cfg["overflow_bytes"]:
                submit.update(status="error", error={"kind": "overflow"})
            else:
                gz = gzip.compress(body_text.encode())
                submit.update(
                    status="ok",
                    http_status=http_status,
                    body_gzip_b64=base64.b64encode(gz).decode(),
                )
        else:
            submit.update(
                status="error",
                error={"kind": "http" if kind == "http" else "transport"},
            )
            if http_status:
                submit["http_status"] = http_status
        s_status, _ = self.api("POST", "/submit", submit)
        if s_status != 200:
            log("warn", f"submit refused HTTP {s_status}")
        return "job"

    def run(self):
        self.load_config()
        _touch_progress()
        last_config = self.now()
        while True:
            if time.time() - _last_progress[0] > WATCHDOG_TIMEOUT_S:
                log("error", "watchdog: no successful door contact in "
                    f"{WATCHDOG_TIMEOUT_S}s; exiting for supervisor restart")
                sys.exit(1)
            if self.now() - last_config > 3600:
                try:
                    self.load_config()
                except Exception as e:  # noqa: BLE001 - config refresh is best-effort
                    log("warn", f"config refresh failed: {e}")
                last_config = self.now()
            try:
                outcome = self.poll_once()
            except Exception as e:  # noqa: BLE001 - the loop must survive
                log("warn", f"poll error: {e}")
                self.sleep(10)
                continue
            if outcome == "empty" and self.cfg["gateway"]["channel"] != "live":
                self.sleep(self.cfg["poll"]["idle_backoff_s"])


def main():
    load_env()
    cr_token = os.environ.get("CR_API_TOKEN")
    api_token = os.environ.get("ELIXIR_API_TOKEN")
    if not cr_token or not api_token:
        log("error", "CR_API_TOKEN and ELIXIR_API_TOKEN are required")
        sys.exit(2)
    base = os.environ.get(
        "ELIXIR_API_BASE", "https://elixir.poapkings.com/api/collector"
    )
    log("info", f"gateway up (python, zero-trust v2) version={VERSION}")
    Collector(base, api_token, cr_token).run()


if __name__ == "__main__":
    main()
