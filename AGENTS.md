# AGENTS.md

elixir-mcp-collector: the operator-run fetch worker for
[Elixir MCP](https://elixir.poapkings.com). Split from the main repo
2026-09-04 so operators clone something small. `CLAUDE.md` is a symlink
to this file — do not fork them. `README.md` is the operator-facing
guide and must stay accurate; this file is the working guide for agents.

## What it is (one paragraph)

A collector is a pure API client of Elixir MCP: it leases fetch jobs
over three HTTPS endpoints (`/api/collector/config|lease|submit`),
calls the Clash Royale API with the operator's IP-bound key, and posts
gzipped results back. No AWS, no database, no cloud access. Two
interchangeable implementations — Go (`cmd/collector`, `internal/`) and
Python (`python/collector.py`) — kept deliberately diverse so a bad
release of one cannot silence a fleet. The old Node worker and the SQS
transport were retired 2026-09-06 (zero-trust door + Postgres job
ledger); do not resurrect them.

## Rules

1. **This repo is PUBLIC; secrets never enter it — or agent context.**
   Config is gitignored `.env*` files (`CR_API_TOKEN`,
   `ELIXIR_API_TOKEN`), mode 0600, handled by file/name reference only.
   Verify with `git ls-files`, never trust `.gitignore` alone. (A
   `.env.v2-go` once slipped past a too-narrow ignore and leaked to the
   public remote — `.gitignore` now ignores all `.env.*` except
   `.env.example`. Rotate on any exposure.)
2. **The server owns the contract AND the behavior.** The clients speak
   config/lease/submit; the server computes each CR path, assigns the
   channel (bulk for operators, live for owner machines), and hands out
   pacing/breaker/backoff at launch. Collection changes never require a
   client change. The queue/API contract is canonical in
   `jthingelstad/elixir-mcp` (`packages/contracts`); this repo's tests
   pin the shapes it produces so drift fails here first, and a contract
   change lands server-side first.
3. **One global rate budget.** More collectors = resilience, never quota
   multiplication (ToS posture). Pacing (~1.5 s floor) and the 5×403
   circuit breaker are load-bearing and server-configured; never remove
   them. 429s honor `Retry-After`.
4. **Self-update obeys the UPDATE AUTHORITY.** A release build installs
   only the exact version + SHA-256 the server's config endpoint names
   (key `go-<GOOS>-<GOARCH>`); a compromised release page alone cannot
   push code to operators. Dev builds and the Python client never
   self-update. (The `collector_release` rows the config endpoint serves
   are populated server-side; until they are, released binaries simply
   don't auto-update — that's safe.) Cross-platform: release.yml builds
   macOS (arm64/amd64), Windows (amd64/arm64), and Linux
   (amd64/arm64/armv7). Windows self-update renames the running .exe
   aside (can't overwrite a locked binary) and cleans the `.old` at
   next startup.
5. **Durability: exit rather than wedge.** Both clients run a progress
   watchdog — 5 minutes with no successful server round-trip and the
   process exits(1) so the supervisor restarts it clean. Any door
   response (even an error) counts as progress. This exists because a
   door redeploy once wedged the dev collectors (alive but not
   progressing); launchd KeepAlive only restarts a process that EXITS.
6. **Observability: the log shows work.** Both clients emit a JSON
   activity summary every 5 minutes (jobs done, fetch errors, channel).
   Don't log per-fetch (too noisy at ~40/min).
7. Work lands on `main`; CI (`go test` + Python `unittest`, both in
   `.github/workflows/validate.yml`) is the pre-push gate. `main` must
   stay releasable — `release.yml` builds the four platform binaries
   from green main. Operator-facing behavior changes update `README.md`
   in the same commit.

## Layout

- `cmd/collector/main.go` — entrypoint; runs the v2 client when
  `ELIXIR_API_TOKEN` is set.
- `internal/v2/` — the zero-trust client (config/lease/submit, watchdog,
  activity log, update authority).
- `internal/crapi`, `internal/breaker` — CR API paths + the 403 breaker
  (shared helpers).
- `python/collector.py` — the stdlib-only twin; `python/test_collector.py`
  its tests.
- `scripts/install.sh` — one-command install for macOS/Linux (download
  binary + supervise via launchd/systemd). `scripts/install.ps1` — the
  Windows equivalent (download .exe + register a Scheduled Task).
  `scripts/elixir-collector.service` (systemd unit),
  `scripts/run-forever.sh` (generic POSIX KeepAlive loop for DSM and
  other hosts without a supervisor; finds the binary in its own dir,
  its parent, or `$PWD`, falls back to the Python twin, and exits with
  a message rather than restart-looping when it finds neither),
  `scripts/test-run-forever.sh` (its tests; CI runs them under both
  `sh` and `dash`).
- `docs/GO-PORT.md` — design history (parts superseded by the zero-trust
  transition; see its postscript).

## Local services on this host

Jamie's machine runs the v2 pair: `com.poapkings.elixir-mcp-gw-go` (Go,
env `.env.local-go`) and `com.poapkings.elixir-mcp-gw2-py`
(`python/collector.py`, env `python/.env`) — card identities Ram Rider
and Tesla, both on the `live` channel. Dev builds, so they don't
self-update: after any door change or new build, reload with
`launchctl unload/load` (a plist env-path change needs a full reload,
not just `kickstart`). Logs:
`~/Library/Logs/elixir-mcp-gw-go.log` and `…-gw2-py.log`.

---

_This material is unofficial and is not endorsed by Supercell. For more
information see Supercell's Fan Content Policy:
www.supercell.com/fan-content-policy._
