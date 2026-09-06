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
database, no cloud access of any kind. Elixir MCP tells the running
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

## 3. Run it

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
elixir-collector`. On a NAS or anything without systemd,
`scripts/run-forever.sh` is a plain KeepAlive loop for a boot-up task.

### Prefer Python, or an unlisted platform?

The `python/collector.py` twin runs anywhere with **Python 3.8+**
(standard library only — no `pip install`), identical behavior to the
Go binary:

```sh
python3 python/collector.py     # reads ./.env
```

Supervise it with your platform's service manager (launchd, Scheduled
Task, systemd, or `run-forever.sh`). Building the Go binary yourself is
`go build -o collector ./cmd/collector` for any target Go supports.

## 4. Confirm it's working

The collector writes JSON log lines to standard output; the installer
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

Released Go binaries self-update: the collector installs only the exact
version and SHA-256 the **server** names, so a compromised release page
alone cannot push code to operators. An update failure never stops
collection. The Python script and locally-built binaries do not
self-update — update them yourself.

## What a collector can and cannot do

- It fetches only the `(endpoint, entity_key)` pairs the server leases
  it; targets are chosen and prioritized by Elixir MCP.
- Its identity is stamped from its token server-side, so it cannot act
  as another collector.
- It paces itself (server-configured, ~1.5 s floor) and opens a circuit
  breaker on repeated 403s rather than hammering the API; it honors
  `Retry-After` on 429s.
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
