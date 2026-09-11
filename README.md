# Elixir MCP Collector

**What this is:** the operator-run worker for
[Elixir MCP](https://elixir.poapkings.com), a service that records
Clash Royale history and serves it to AI agents. Clash Royale's own API
only returns the present moment; Elixir MCP keeps the history — and this
small program is how the data gets fetched.

**Why it exists:** the Clash Royale API only accepts requests from
allowlisted IP addresses, so fetching has to happen on machines with
stable IPs that volunteers run. A collector leases a work item ("fetch
this player", "fetch this clan's war") from Elixir MCP over HTTPS,
calls the Clash Royale API with the operator's own key, and posts the
result back. It never chooses its own targets and never sees any user's
private data — only public game data.

**Zero trust by design.** A collector holds exactly two secrets: your
Clash Royale API key and a bearer token Elixir MCP issues you. It talks
to **three HTTPS endpoints and nothing else** — no AWS credentials, no
database, no cloud access of any kind. The Go binary has no third-party
dependencies at all: it is the Go standard library and nothing else. Elixir MCP tells the running
collector what to fetch (it even computes the exact API path), so the
service can change what it collects without you ever updating anything.
More collectors mean resilience, never a bigger rate budget: the whole
fleet shares one global ~1 request/second budget by design (that is
Supercell Terms-of-Service posture, not a limitation to work around).

Running a collector earns its operator a higher daily tool-call quota
on Elixir MCP, a Clash Royale card as its public identity, and a spot
on the collector ladder.

Two interchangeable implementations live here — a **Go** binary and a
**Python** script — deliberately, so a bad release of one can never
silence a whole fleet. Pick whichever your machine prefers.

## You need

- A machine that stays on, with a **static public IP** (this is the
  real requirement — Clash Royale keys are IP-allowlisted).
- A **Clash Royale API key** from <https://developer.clashroyale.com>,
  created with that IP allowlisted.
- Either nothing else (the Go binary is self-contained) or **Python
  3.8+** (standard library only — no `pip install`).

## 1. Raise your hand

At <https://elixir.poapkings.com> → **Account → Collector**, pick a
short machine name and raise your hand. Elixir MCP assigns your
collector a Clash Royale **card identity** (its public name). When the
maintainer approves and provisions it, that same page shows your
**collector token once** — copy it. It looks like `emcg_…`.

There is no IP to submit, no account to create on our side, no
credentials handed over out of band. Just the token you copy.

## 2. Configure

Copy `.env.example` to `.env` (mode `600`) next to where the collector
will run, and fill in the two secrets:

```sh
CR_API_TOKEN=your-clash-royale-key
ELIXIR_API_TOKEN=emcg_your-collector-token
```

That is the entire configuration. Everything else — how fast to fetch,
what to fetch, when to back off — the server hands the collector at
startup.

**Where `.env` goes:** each worker reads it from **its own directory**,
not from wherever you happen to be standing. The Go binary looks beside
the binary; `python/collector.py` looks beside `collector.py`, so the
Python twin wants `python/.env`. Set `ELIXIR_MCP_ENV_FILE` to an
absolute path if you would rather keep config somewhere else.

## 3. Run it

**Nowhere to run it yet?** [`docs/recipes/`](docs/recipes/README.md)
has a decision table and a one-paste cloud-init file: an Oracle Always
Free VM with a reserved IP costs nothing and comes up collecting from
the create-instance form. Recipes for Hetzner, Synology, macOS and
Windows are there too, and a template for adding yours.

Runs the same on **macOS, Windows, and Linux** — a prebuilt binary
exists for each (Apple Silicon and Intel Macs; Windows x64 and ARM;
Linux x64, ARM64, and ARMv7). Pick your platform below; each installer
downloads the right binary, verifies its SHA-256, and registers a
service that keeps the collector running and restarts it after a
self-update. Run the command from the directory holding your `.env`.

### macOS

```sh
curl -fsSL https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/install.sh | sh
```

Installs a launchd agent. Logs: `~/Library/Logs/elixir-mcp-collector.log`.

### Windows

In PowerShell:

```powershell
irm https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/install.ps1 | iex
```

Installs a Scheduled Task (built into Windows — nothing else to
install) that starts at logon and restarts on failure.

### Linux

```sh
curl -fsSL https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/install.sh | sh
```

Downloads the binary; then supervise it with systemd — edit
`User=`/`WorkingDirectory=` in `scripts/elixir-collector.service`, copy
it to `/etc/systemd/system/`, and `sudo systemctl enable --now
elixir-collector`.

### NAS and anything without systemd (Synology DSM, BSD, OpenWrt)

`scripts/run-forever.sh` is a plain KeepAlive loop: it runs the
collector, and restarts it whenever it exits (a crash, a self-update,
or its progress watchdog firing). Plain POSIX shell, so BusyBox `sh` on
DSM runs it as-is.

**Put it wherever the collector is.** The script searches its own
directory, its parent, and the current working directory, and runs the
first `collector` it finds — so the flat layout the installer produces
(`collector`, `.env` and `run-forever.sh` all in one folder) works
exactly as well as a git checkout with the script in `scripts/`. With
no binary in any of those places it prints what it looked for and
exits, rather than restart-looping in silence.

On Synology DSM, start over SSH by making a folder you own. A DSM share
is not writable by your user by default, so this takes `sudo`:

```sh
sudo mkdir -p /volume1/elixir-collector
sudo chown "$USER" /volume1/elixir-collector
cd /volume1/elixir-collector
```

Write your `.env` there first, because the installer looks for it in the
current directory and stops if it is missing:

```sh
printf 'CR_API_TOKEN=%s\nELIXIR_API_TOKEN=%s\n' "your-cr-key" "emcg_your-token" > .env
chmod 600 .env
```

Then pull the binary and the supervisor into the same folder:

```sh
curl -fsSL https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/install.sh | sh
curl -fsSL -o run-forever.sh https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/run-forever.sh
```

The installer notes that DSM has no user systemd and leaves the binary
in place. Check what the supervisor resolves before you wire it to boot:

```sh
sh run-forever.sh --check
```

That prints the binary it found and the log it will write, and exits
without running anything. Then add a **triggered task** in Control Panel
→ Task Scheduler → Create → Triggered Task → User-defined script, event
**Boot**, running as your own user, with this command:

```sh
cd /volume1/elixir-collector && sh run-forever.sh
```

**Always invoke it as `sh run-forever.sh`, never `./run-forever.sh`.** A
file fetched with `curl` carries no executable bit, and a boot task that
dies on permission denied tells you nothing.

The log lands next to the binary — `/volume1/elixir-collector/collector.log`
— unless you pass a path of your own as the one argument
(`sh run-forever.sh /volume1/logs/collector.log`). Start-up failures go
to both stderr and that log, so a boot task that dies at the pre-flight
check still leaves you something to read. The log rotates at 10 MB,
keeping one previous generation as `collector.log.1`; set
`MAX_LOG_BYTES` to change the threshold, or to `0` to turn rotation off.

### Prefer Python, or an unlisted platform?

The `collector.py` twin runs anywhere with **Python 3.8+** (standard
library only — no `pip install`), identical behavior to the Go binary.
It ships as a release asset with its own SHA-256, so pin and verify it
the same way you would the binary rather than curling whatever `main`
happens to be:

```sh
mkdir -p ~/elixir-collector && cd ~/elixir-collector
TAG=$(curl -fsSL https://api.github.com/repos/jthingelstad/elixir-mcp-collector/releases/latest | sed -n 's/.*"tag_name": "\([^"]*\)".*/\1/p')
base=https://github.com/jthingelstad/elixir-mcp-collector/releases/download/$TAG
curl -fsSL -o collector.py "$base/collector.py"
curl -fsSL "$base/SHA256SUMS" | grep ' collector.py$' | shasum -a 256 -c -
```

Then put your `.env` **beside `collector.py`** (that is where it looks,
not the directory you run from) and start it:

```sh
chmod 600 .env
python3 collector.py
```

Supervise it with your platform's service manager (launchd, Scheduled
Task, systemd, or `run-forever.sh` — with no binary present the loop
runs the Python twin instead).

The Python twin **never self-updates**; that is the point of it. It
exists so a bad Go release cannot silence a whole fleet, so re-run the
download above when a new release lands. A copy running straight out of
a git checkout reports its version as `py-dev`, and a released copy
reports `py-<tag>`, so you can always tell which one a machine is
running. Building the Go binary yourself is
`go build -o collector ./cmd/collector` for any target Go supports.

## 4. Confirm it's working

**Ask the doctor first.** Both implementations carry a read-only
preflight that runs five checks and prints one summary — your runtime,
the `.env` and the shape of both secrets (never their values), what
Elixir MCP thinks this collector is (identity, lifecycle state, channel,
clock skew), the public IP your box reaches out from, and one cheap
Clash Royale read from that IP with your key:

```sh
./collector doctor            # Go binary
python3 collector.py --check  # Python twin
```

The two things that fail most, in words rather than status codes:

```
✓ elixir        https://elixir.poapkings.com/api/collector
  identity     Goblin Barrel (oracle-1)
  state        pending - installed, not yet promoted - the maintainer
               moves this collector to probation; nothing to fix here
```

```
✗ clash_royale  Clash Royale API rejected this key (403 accessDenied.invalidIp)
  Invalid authorization: API key does not allow access from IP 132.145.0.9
  your egress IP: 132.145.0.9
  fix: add 132.145.0.9 to this key's allowed IPs at developer.clashroyale.com
```

Exit code `0` is healthy, `1` means something on this box or its network
is broken, `2` means everything here is right and the collector is
simply not active yet (`pending`, `draining`). `--json` gives the same
report as a document to paste into an issue. Doctor never leases work, so
it is safe to run beside a live collector.

Once it is running, the collector writes JSON log lines to standard output; the installer
routes them to a file (`~/Library/Logs/elixir-mcp-collector.log` on
macOS; wherever your supervisor captures stdout on Windows/Linux).
Within a few minutes you'll see a startup line, a `config` line showing
your channel, then an **activity summary every 5 minutes** — jobs done,
fetch errors, channel. Warnings cover rate-limit backoff and refused
leases; if the collector can't reach the service for 5 minutes it logs
an error and exits so the supervisor restarts it clean.

Your collector's public status (by card name, heartbeat, and hourly
fetch rate) also shows on <https://elixir.poapkings.com/data/status>.

## Lifecycle

`pending` → the maintainer provisions your token → `probation` (it does
real work immediately) → after a few clean days, `active`. `draining`
means no new work (planned retirement or a tripped safety breaker);
`revoked` means the token no longer works. Revoking is instant and is
the only thing needed to remove a collector — there is no cloud account
to tear down.

## Staying current

Released Go binaries self-update, always and automatically. The Python
script and locally-built binaries cannot, so their operators are
expected to update them when asked.

**Which release is live.** Every green build publishes a release, so a
release existing does not mean anyone runs it. Candidates are marked as
prereleases; the one Elixir MCP has named is promoted to **Latest**.
That is the release the installers download and the one collectors
update themselves to, so a fresh install always matches the fleet.

**When it checks.** At startup, then once an hour, as part of the same
`/config` call it already makes. There is no separate update poll and
no push: a fleet-wide rollout therefore lands within an hour of the
server naming a version, not instantly and not in days.

**What it trusts.** The `/config` response names a version, a SHA-256,
and a download URL for your exact platform. The collector installs that
build only if the file it downloads matches that SHA-256. It never asks
GitHub what the newest release is, so a compromised release page alone
cannot push code to operators — the server is the only authority, and
it names one build per platform.

**What it needs to reach.** Three hosts, all HTTPS on 443. If your NAS
or firewall allowlists egress, these are the entries:

| Host | Why |
|---|---|
| `elixir.poapkings.com` | lease, submit, config |
| `api.clashroyale.com` | the fetches themselves |
| the update URL from `/config` | the new binary, today a GitHub release asset (`github.com`, redirecting to `objects.githubusercontent.com`) |

**What it looks like in the log.** An update is not a mystery exit. You
see the collector name it, then hand off to your supervisor:

```
{"level":"info","msg":"update authority names v0.1.15; self-updating"}
{"level":"info","msg":"updated; exiting for supervisor restart"}
```

Then `run-forever: worker exited (0); restarting in 2s`, and a fresh
startup line on the new version. An exit with no `updated` line above it
is a crash or the watchdog, not an update. A failed update logs
`self-update failed:` and keeps collecting on the old binary — an
update failure never stops collection.

**What version am I running?** Every startup logs it, so the newest such
line in your log is the answer:

```
{"level":"info","msg":"gateway up (go, zero-trust v2) version=v0.1.14"}
```

The collector also sends that version to the server on every call, so
the maintainer can see your version even when you cannot.

**On Windows**, a running `.exe` cannot be overwritten, so the update
renames the old binary to `collector.exe.old` beside itself and writes
the new one in its place. That file is deleted at the next startup.
Seeing one briefly is normal.

**Can I pin or opt out? No.** There is no pin, no version flag, and no
opt-out. A released binary runs the version the server names, and that
is deliberate: the fleet shares one global rate budget and one API
contract, so a collector running last month's code is a liability to
everyone else, not a private choice. If you cannot accept automatic
updates, running a collector is not for you.

The Python twin is not a way around this. It exists so that a bad Go
release cannot silence the whole fleet, and operators who run it are
expected to update it when asked. Self-built binaries are for
developing on this repo, not for freezing a production collector.

## What a collector can and cannot do

- It fetches only the `(endpoint, entity_key)` pairs the server leases
  it; targets are chosen and prioritized by Elixir MCP.
- Its identity is stamped from its token server-side, so it cannot act
  as another collector.
- It paces itself (server-configured, ~1.5 s floor) and opens a circuit
  breaker on repeated 403s rather than hammering the API; it honors
  `Retry-After` on 429s.
- It judges the transport overflow limit on the gzip+base64 size it
  actually sends (raw battlelogs well above 250 KB routinely fit once
  compressed); a genuine overflow is submitted as a structured error and
  counted as a lost fetch in the activity summary.
- On a transport failure or server 5xx while submitting, it retries the
  same lease within the server-supplied lease budget; a 4xx remains a refusal.
- It holds no AWS credentials and can reach nothing in the Elixir MCP
  cloud beyond three HTTPS endpoints. It never sees accounts, emails,
  or sessions — only public Clash Royale data.

## Contributing / architecture

The queue-message and API contracts are canonical in the main repo
([`jthingelstad/elixir-mcp`](https://github.com/jthingelstad/elixir-mcp),
`packages/contracts`) and enforced server-side; this repo's tests pin
the shapes it produces so drift fails here first. `AGENTS.md` is the
working guide; `docs/GO-PORT.md` is the design history. `main` must stay
releasable — CI (`go test` + Python `unittest`) gates it.

## License

MIT — see [LICENSE](LICENSE).

---

_This material is unofficial and is not endorsed by Supercell. For more
information see Supercell's Fan Content Policy:
www.supercell.com/fan-content-policy._
