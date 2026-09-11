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
# Stamped with the release tag when published as a release asset;
# a copy running straight out of a checkout stays "py-dev", which
# is what the admin version column should say about it.
VERSION = "py-dev"
# Watchdog: with no successful door contact for this long, exit so the
# supervisor restarts clean (a wedged-but-alive process is invisible to
# launchd KeepAlive; automates the manual kickstart from the 2026-09-06
# phase-1-redeploy wedge).
WATCHDOG_TIMEOUT_S = 300
# Raw-response safety ceiling, distinct from the server-configured transport
# overflow (which is judged on the gzip+base64 ENCODED size). No legitimate CR
# response is anywhere near this; it only bounds what we are willing to gzip.
MAX_RAW_BYTES = 8 * 1024 * 1024


_last_progress = [time.time()]


def _touch_progress():
    _last_progress[0] = time.time()


def log(level, msg):
    print(json.dumps({"level": level, "msg": msg, "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}), flush=True)


def load_env():
    """Read .env beside this file (or $ELIXIR_MCP_ENV_FILE) into the
    environment. Returns (path looked at, found) for doctor to report."""
    path = os.environ.get(
        "ELIXIR_MCP_ENV_FILE",
        os.path.join(os.path.dirname(os.path.abspath(__file__)), ".env"),
    )
    if not os.path.exists(path):
        return path, False
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            os.environ.setdefault(key.strip(), value.strip().strip("\"'"))
    return path, True


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


def filter_battlelog(body_text, after):
    """Apply the lease's filter (2026-09-11): keep the entries whose
    battleTime is after `after`, the newest battle the hub already holds,
    in the API's own spelling (20260911T123456.000Z) - battleTime strings
    compare lexically, so nothing here parses a date. The body stays the
    API's array, fewer entries. Returns (body, observed, filtered, applied);
    a body that is not an array is returned untouched with applied=False so
    the hub still sees exactly what the API said. An entry without a
    readable battleTime is kept: dropping what we cannot judge would be a
    silent loss. Same as the Go twin's filter.Battlelog."""
    if not after:
        return body_text, 0, 0, False
    try:
        entries = json.loads(body_text)
    except ValueError:
        return body_text, 0, 0, False
    if not isinstance(entries, list):
        return body_text, 0, 0, False
    kept = [
        e for e in entries
        if not isinstance(e, dict)
        or not isinstance(e.get("battleTime"), str)
        or e["battleTime"] == ""
        or e["battleTime"] > after
    ]
    return (
        json.dumps(kept, separators=(",", ":"), ensure_ascii=False),
        len(entries),
        len(entries) - len(kept),
        True,
    )


def fetch_cr(cr_token, path, timeout=15):
    """One CR API attempt: (kind, status, body_text, retry_after)."""
    req = urllib.request.Request(
        CR_BASE + path,
        headers={
            "authorization": f"Bearer {cr_token}",
            # The real name and the stamped version, as the Go collector
            # sends: Supercell sees this on every request.
            "user-agent": f"Elixir-MCP-Collector/{VERSION} (+https://elixir.poapkings.com/docs/operators)",
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
        self.submit_api = (
            (lambda body, timeout: api_call("POST", "/submit", body))
            if api_call
            else (lambda body, timeout: api(base, api_token, "POST", "/submit", body, timeout))
        )
        self.cr = cr_fetch or (lambda path: fetch_cr(cr_token, path))
        self.cfg = None
        self.last_fetch_started = 0.0
        self.consecutive_403 = 0
        self.breaker_open_until = 0.0
        self.next_wait = 0  # seconds until the next check-in, as the door said
        self.jobs_done = 0
        self.fetch_errors = 0

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
        # Check-ins, not polling (2026-09-11): never ask the door to wait;
        # it says when to come back (next_check_in_s), and run() sleeps
        # that long. A door older than the contract says nothing, and the
        # config's idle backoff stands.
        status, lease = self.api("POST", "/lease", {})
        idle = self.cfg["poll"]["idle_backoff_s"]
        if status in (401, 409, 429):
            log("warn", f"lease refused HTTP {status} {lease.get('error', '')}")
            self.next_wait = self._next_wait(lease, idle)
            return "refused"
        if lease.get("empty") or not lease.get("lease"):
            self.next_wait = self._next_wait(lease, idle)
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
        overflow = False
        if kind == "http" and http_status == 200:
            # What the API handed us before any filter, so the hub can say
            # what the edge saved (2026-09-11).
            submit["api_bytes"] = len(body_text.encode())
            # Drop what the hub already holds, and say how much that was.
            after = (lease.get("filter") or {}).get("battles_after")
            if after:
                body_text, observed, dropped, applied = filter_battlelog(body_text, after)
                if applied:
                    submit["observed"] = observed
                    submit["filtered"] = dropped
            raw = body_text.encode()
            if len(raw) > MAX_RAW_BYTES:
                # Explicit raw safety ceiling - distinct from the transport limit.
                overflow = True
            else:
                b64 = base64.b64encode(gzip.compress(raw)).decode()
                # The transport overflow is judged on the ENCODED size the
                # door receives (DESIGN 5.1): raw battlelogs above 250 KB
                # routinely compress 10-20x and must not be discarded.
                if len(b64) > self.cfg["overflow_bytes"]:
                    overflow = True
            if overflow:
                submit.update(status="error", error={"kind": "overflow"})
            else:
                submit.update(
                    status="ok", http_status=http_status, body_gzip_b64=b64
                )
        else:
            submit.update(
                status="error",
                error={"kind": "http" if kind == "http" else "transport"},
            )
            if http_status:
                submit["http_status"] = http_status
        s_status = self.submit_with_retry(submit)
        if s_status != 200:
            log("warn", f"submit refused HTTP {s_status}")
        self.jobs_done += 1
        # An overflow is a lost fetch and counts as one in the summary.
        if overflow or not (kind == "http" and http_status == 200):
            self.fetch_errors += 1
        # There may be more: the door said so when it granted this one.
        self.next_wait = self._next_wait(lease, 0)
        return "job"

    @staticmethod
    def _next_wait(lease, fallback_s):
        v = (lease or {}).get("next_check_in_s")
        if isinstance(v, int) and not isinstance(v, bool) and v >= 0:
            return v
        return fallback_s

    def submit_with_retry(self, submit):
        """Retry only transient submit failures without abandoning the lease."""
        retry = self.cfg.get("submit_retry", {})
        attempts = retry.get("max_attempts", 3)
        attempts = 3 if not isinstance(attempts, int) or not 1 <= attempts <= 3 else attempts
        timeout = retry.get("timeout_s", 20)
        timeout = 20 if not isinstance(timeout, int) or not 1 <= timeout <= 20 else timeout
        backoff_ms = retry.get("backoff_ms", 500)
        backoff_ms = 500 if not isinstance(backoff_ms, int) or not 1 <= backoff_ms <= 5000 else backoff_ms

        for attempt in range(1, attempts + 1):
            try:
                status, _ = self.submit_api(submit, timeout)
                transient = status >= 500
            except Exception as exc:  # transport failures are transient too
                status, transient = None, True
                detail = str(exc)
            if not transient:
                return status
            if attempt == attempts:
                return status
            if status is None:
                log("warn", f"submit transport failure; retrying same lease ({attempt}/{attempts}): {detail}")
            else:
                log("warn", f"submit refused HTTP {status}; retrying same lease ({attempt}/{attempts})")
            self.sleep(backoff_ms / 1000)
            backoff_ms *= 2

        return None

    def run(self):
        self.load_config()
        _touch_progress()
        last_config = self.now()
        last_summary = self.now()
        while True:
            # Activity summary every ~5 min so the log shows real work.
            if self.now() - last_summary >= 300:
                log("info", f"activity: {self.jobs_done} jobs done, "
                    f"{self.fetch_errors} fetch errors in the last 5m "
                    f"(channel={self.cfg['gateway']['channel']})")
                self.jobs_done = 0
                self.fetch_errors = 0
                last_summary = self.now()
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
            if self.next_wait > 0:
                self.sleep(self.next_wait)


# ---- doctor: the operator's preflight (collector.py --check [--json]) ----
#
# Five read-only checks that say, in one paste, why a box "isn't
# collecting". Never leases work (a diagnostic lease would orphan a real
# job for its TTL), never prints a secret (last four characters, in every
# mode). Same report shape and wording as the Go twin's `collector doctor`;
# the two are kept in step by hand like the rest of the client.

EXIT_HEALTHY, EXIT_BROKEN, EXIT_NOT_YET_ACTIVE = 0, 1, 2
# What doctor reads from the CR API when the door could not designate a
# probe; the door's doctor.cr_path wins.
DEFAULT_PROBE = "/locations?limit=1"
STATE_TEXT = {
    "pending": "installed, not yet promoted - the maintainer moves this collector to probation; nothing to fix here",
    "probation": "leasing work; the maintainer activates it after watching it run",
    "active": "leasing and submitting work",
    "draining": "finishing what it holds and leasing nothing new - the maintainer is retiring it, or it was quarantined for expired leases; ask them",
}


def _tail4(s):
    return "…" + s[-4:] if len(s) > 4 else "…"


def _check(name, ok=False, detail="", warn=False, lines=None, fix="", fields=None):
    c = {"name": name, "ok": ok, "detail": detail}
    if warn:
        c["warn"] = True
    if lines:
        c["lines"] = lines
    if fix:
        c["fix"] = fix
    if fields:
        c["fields"] = fields
    return c


def _host_arch():
    import platform
    return platform.machine()


def doctor_run(env_path, env_found, cr_token, api_token, base,
               config_call=None, cr_fetch=None, now=time.time, host_arch=_host_arch):
    """Every check runs; returns the report dict. config_call() -> (status,
    body, date_header) and cr_fetch(path) are injectable for tests."""
    import platform
    checks = []

    # 1. runtime
    py = platform.python_version()
    arch = host_arch()
    ok = sys.version_info >= (3, 8)
    checks.append(_check("runtime", ok=ok,
                         detail=f"python {py} {sys.platform}/{arch}",
                         fix="" if ok else "Python 3.8 or newer is required"))

    # 2. config file and the shape of the two secrets
    fields, lines, warn, fix = {}, [], False, ""
    if env_found:
        detail = env_path
        if os.name != "nt":
            mode = os.stat(env_path).st_mode & 0o777
            detail += f" (mode {mode:o})"
            if mode & 0o077:
                warn, fix = True, f"chmod 600 {env_path} - it holds two secrets"
    else:
        detail = f"no .env found at {env_path}; reading environment variables only"
    missing = []
    if not cr_token:
        missing.append("CR_API_TOKEN")
    else:
        fields["cr_api_token"] = _tail4(cr_token)
        if cr_token.count(".") != 2 or not cr_token.startswith("eyJ"):
            warn = True
            lines.append("CR_API_TOKEN does not look like a Clash Royale key (they are JWTs: three dot-separated parts starting eyJ)")
    if not api_token:
        missing.append("ELIXIR_API_TOKEN")
    else:
        fields["elixir_api_token"] = _tail4(api_token)
        if not api_token.startswith("emcg_"):
            warn = True
            lines.append("ELIXIR_API_TOKEN does not start with emcg_ - collector tokens do; a service token (svt_) or agent token is a different door")
    ok = not missing
    if missing:
        lines.append("missing: " + ", ".join(missing))
        fix = "put both keys in .env beside collector.py (README, step 2)"
    checks.append(_check("config", ok=ok, detail=detail, warn=warn, lines=lines, fix=fix, fields=fields))

    # 3. the door: identity, state, channel, clock skew
    cfg = None
    fields, lines, warn, fix, ok = {}, [], False, "", False
    detail = base
    if not api_token:
        detail += " - skipped, no ELIXIR_API_TOKEN"
    else:
        call = config_call or (lambda: _config_with_date(base, api_token))
        try:
            status, body, date_header = call()
        except Exception as e:  # noqa: BLE001 - unreachable is a finding
            status, body, date_header = None, {}, None
            detail += f" - unreachable: {e}"
            fix = "this box needs outbound HTTPS to elixir.poapkings.com"
        if date_header:
            try:
                import email.utils
                server = email.utils.parsedate_to_datetime(date_header).timestamp()
                skew = now() - server
                fields["skew_s"] = f"{skew:.1f}"
                if abs(skew) > 30:
                    warn = True
                    lines.append(f"clock is {skew:.0f}s off the server's - fix NTP")
            except Exception:  # noqa: BLE001 - an unparseable Date is not a finding
                pass
        if status == 200:
            ok, cfg = True, body
            gw = body.get("gateway", {})
            fields.update(name=gw.get("name", ""), card=gw.get("card") or "",
                          channel=gw.get("channel", ""), status=gw.get("status", ""),
                          state=STATE_TEXT.get(gw.get("status", ""), ""))
        elif status == 401:
            detail += " - the door does not recognise this token"
            lines.append("a typo, a token from another machine, or one that was never claimed on the Collectors page")
            fix = "copy the token again from Status > Collectors (a staged token expires unclaimed after 72 hours)"
        elif status == 403:
            detail += f" - {body.get('error', '')}"
            if body.get("hint"):
                lines.append(body["hint"])
        elif status == 429:
            detail += " - config budget spent for this hour (120/hour)"
            lines.append("a running collector reads config once an hour; something on this token is calling it far more often")
        elif status is not None:
            detail += f" - HTTP {status} {body.get('error', '')}"
    checks.append(_check("elixir", ok=ok, detail=detail, warn=warn, lines=lines, fix=fix, fields=fields))

    # 4. egress IP, as the door saw it
    egress = (cfg or {}).get("observed_ip") or ""
    if egress:
        checks.append(_check("egress", ok=True, fields={"ip": egress},
                             detail=f"{egress} (this box, as the door saw it - the address to allowlist on the Clash Royale key)"))
    else:
        checks.append(_check("egress", detail="unknown - the door did not answer, so nothing saw this box from outside"))

    # 5. one CR read from this IP, on the server-named path
    fields, lines, fix, ok, warn = {}, [], "", False, False
    if not cr_token:
        detail = "skipped, no CR_API_TOKEN"
    else:
        path = (cfg or {}).get("doctor", {}).get("cr_path") or DEFAULT_PROBE
        fields["path"] = path
        fetch = cr_fetch or (lambda p: fetch_cr(cr_token, p))
        kind, status, body_text, _ = fetch(path)
        shown = egress or "unknown"
        if kind != "http":
            detail = "api.clashroyale.com unreachable"
            fix = "this box needs outbound HTTPS to api.clashroyale.com"
        else:
            fields["status"] = str(status)
            if status == 200:
                ok, detail = True, f"key works from {shown} (GET {path} -> 200)"
            elif status == 403:
                try:
                    body = json.loads(body_text or "{}")
                except ValueError:
                    body = {}
                reason, message = body.get("reason", ""), body.get("message", "")
                detail = f"Clash Royale API rejected this key (403 {reason})"
                if message:
                    lines.append(message)
                if "invalidIp" in reason or "IP" in message:
                    lines.append(f"your egress IP: {shown}")
                    fix = f"add {shown} to this key's allowed IPs at developer.clashroyale.com (or create a key with it)"
                else:
                    fix = "check the key at developer.clashroyale.com - this one is not accepted at all"
            elif status == 429:
                ok, warn, detail = True, True, "key works, but the API is throttling it right now (429)"
            else:
                detail = f"GET {path} -> HTTP {status}"
    checks.append(_check("clash_royale", ok=ok, detail=detail, warn=warn, lines=lines, fix=fix, fields=fields))

    if not all(c["ok"] for c in checks):
        verdict, code = "broken", EXIT_BROKEN
    elif cfg and cfg.get("gateway", {}).get("status") in ("pending", "draining"):
        verdict, code = "not_yet_active", EXIT_NOT_YET_ACTIVE
    else:
        verdict, code = "healthy", EXIT_HEALTHY
    import platform as _pl
    return {
        "client": {"impl": "python", "version": VERSION, "os": sys.platform, "arch": _pl.machine()},
        "checks": checks,
        "verdict": verdict,
        "exit": code,
    }


def _config_with_date(base, token):
    """GET /config keeping the Date header (clock skew); a 4xx is an answer."""
    req = urllib.request.Request(
        base + "/config",
        headers={"authorization": f"Bearer {token}", "x-collector-version": VERSION},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as res:
            return res.status, json.loads(res.read().decode() or "{}"), res.headers.get("date")
    except urllib.error.HTTPError as e:
        try:
            body = json.loads(e.read().decode() or "{}")
        except Exception:  # noqa: BLE001
            body = {}
        return e.code, body, e.headers.get("date")


def doctor_text(report):
    c = report["client"]
    out = [f"Elixir MCP Collector doctor ({c['impl']} {c['version']}, {c['os']}/{c['arch']})", ""]
    for ch in report["checks"]:
        mark = "✓" if ch["ok"] and not ch.get("warn") else ("!" if ch["ok"] else "✗")
        out.append(f"{mark} {ch['name']:<13} {ch['detail']}")
        f = ch.get("fields", {})
        if ch["name"] == "config":
            for k in ("cr_api_token", "elixir_api_token"):
                if k in f:
                    out.append(f"  {k.upper():<16} {f[k]}")
        if ch["name"] == "elixir":
            if ch["ok"]:
                out.append(f"  {'identity':<12} {f['card']} ({f['name']})")
                out.append(f"  {'state':<12} {f['status']} - {f['state']}")
                out.append(f"  {'channel':<12} {f['channel']}")
            if "skew_s" in f:
                out.append(f"  {'server time':<12} skew {f['skew_s']}s")
        for line in ch.get("lines", []):
            out.append(f"  {line}")
        if ch.get("fix"):
            out.append(f"  fix: {ch['fix']}")
    out += ["", report["verdict"].replace("_", " ")]
    return "\n".join(out) + "\n"


def main():
    env_path, env_found = load_env()
    cr_token = os.environ.get("CR_API_TOKEN")
    api_token = os.environ.get("ELIXIR_API_TOKEN")
    base = os.environ.get(
        "ELIXIR_API_BASE", "https://elixir.poapkings.com/api/collector"
    )
    if "--check" in sys.argv[1:]:
        report = doctor_run(env_path, env_found, cr_token, api_token, base)
        if "--json" in sys.argv[1:]:
            print(json.dumps(report, indent=2, ensure_ascii=False))
        else:
            print(doctor_text(report), end="")
        sys.exit(report["exit"])
    if not cr_token or not api_token:
        log("error", "CR_API_TOKEN and ELIXIR_API_TOKEN are required")
        sys.exit(2)
    log("info", f"gateway up (python, zero-trust v2) version={VERSION}")
    Collector(base, api_token, cr_token).run()


if __name__ == "__main__":
    main()
