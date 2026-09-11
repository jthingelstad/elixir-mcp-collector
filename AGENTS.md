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
ledger) and their code was DELETED the same day; do not resurrect them.
The Go module now has no third-party dependencies at all, which is what
makes "no AWS access" a property of the build rather than a promise.

## Rules

1. **This repo is PUBLIC; secrets never enter it — or agent context.**
   Config is gitignored `.env*` files (`CR_API_TOKEN`,
   `ELIXIR_API_TOKEN`), mode 0600, handled by file/name reference only.
   Verify with `git ls-files`, never trust `.gitignore` alone. (A
   `.env.v2-go` once slipped past a too-narrow ignore and leaked to the
   public remote — `.gitignore` now ignores all `.env.*` except
   `.env.example`. Rotate on any exposure.)
2. **The server owns the contract AND the behavior.** The clients speak
   config/lease/submit; the server computes each CR path, says when to
   check in again (`next_check_in_s` on every lease answer; since
   2026-09-11 a collector never asks the door to wait, and every
   collector serves priority work first - there is no live channel), and
   hands out pacing/breaker/backoff at launch. Collection changes never require a
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
   push code to operators. The check rides the `/config` call at startup
   and hourly after, so naming a version reaches the fleet within the
   hour. There is NO pin and no opt-out, deliberately: the fleet shares
   one rate budget and one contract, so a stale client is everyone's
   problem. Do not add a pin flag, and do not present the Python twin as
   a way to freeze a version - it is release insurance. Dev builds and
   the Python client cannot self-update; their operators update when
   asked. `min_client_version` is parsed from `/config` and NOT enforced
   by either client, which is the obvious lever if a stale client ever
   needs refusing. (The `collector_release` rows the config endpoint
   serves are populated server-side; until they are, released binaries
   simply don't auto-update - that's safe.) Cross-platform: release.yml
   builds macOS (arm64/amd64), Windows (amd64/arm64), and Linux
   (amd64/arm64/armv7). Windows self-update renames the running .exe
   aside (can't overwrite a locked binary) and cleans the `.old` at next
   startup. README "Staying current" is the operator-facing version of
   all this and must stay true to `internal/v2/v2.go`.
5. **Exit codes are the supervisor contract.** 2 means the config will
   never work (a missing token): `run-forever.sh` stops, systemd has
   `RestartPreventExitStatus=2`, and no supervisor should spin on it.
   Anything else is restartable - crash, watchdog, or self-update.
   Both clients exit 2 for the same reason; keep them in step.
6. **Durability: exit rather than wedge.** Both clients run a progress
   watchdog — 5 minutes with no successful server round-trip and the
   process exits(1) so the supervisor restarts it clean. Any door
   response (even an error) counts as progress. This exists because a
   door redeploy once wedged the dev collectors (alive but not
   progressing); launchd KeepAlive only restarts a process that EXITS.
7. **Observability: the log shows work.** Both clients emit a JSON
   activity summary every 5 minutes (jobs done, fetch errors, channel).
   Don't log per-fetch (too noisy at ~40/min).
8. **A published release is a CANDIDATE, not a shipment.** Every green
   push publishes one as a PRERELEASE, and it reaches nobody until
   Elixir MCP names it — naming also promotes that release to Latest.
   `releases/latest` is what `install.sh`, `install.ps1` and the README
   hand a new operator, so promotion-on-naming keeps a fresh install
   matched to what the fleet actually runs. Soak a candidate on one
   machine before naming it, and never flip the prerelease flag by hand
   or the release page starts lying about what is live. Procedure,
   including rollback and the platform-key trap that fails silently:
   `docs/RELEASING-COLLECTOR.md` in the elixir-mcp repo.

9. Work lands on `main`; CI (`go test`, Python `unittest`, and the
   `run-forever.sh` shell tests, all in
   `.github/workflows/validate.yml`) is the pre-push gate. `main` must
   stay releasable — `release.yml` builds the seven platform artifacts
   plus the Python twin from green main. Operator-facing behavior
   changes update `README.md` in the same commit.

## Layout

- `cmd/collector/main.go` — entrypoint. Requires `CR_API_TOKEN` and
  `ELIXIR_API_TOKEN`; missing either is exit 2. The module has ZERO
  third-party dependencies (stdlib only) since the SQS path went.
- `internal/v2/` — the zero-trust client (config/lease/submit, watchdog,
  activity log, update authority).
- `internal/doctor/` — the operator preflight (`collector doctor
  [--json]`; Python: `collector.py --check [--json]`). Five read-only
  checks, all of which run; exit 0 healthy / 1 broken / 2 valid-but-not-
  yet-active; secrets shown as their last four characters in every mode.
  It NEVER leases (a diagnostic lease would orphan a real job for its
  TTL) - it reads `/config`, which since 2026-09-11 answers `pending`
  tokens, returns 403 `revoked`, echoes `observed_ip`, and names the one
  CR path (`doctor.cr_path`) doctor may read. Both twins print the same
  report; `doctor_test.go` and `DoctorTests` pin the same wording.
- `internal/filter/` — what a lease asks the collector to drop before
  submitting (2026-09-11): `filter.battles_after` on a battlelog lease
  is the newest battleTime the hub holds, in the API's own spelling;
  `Battlelog()` keeps the entries after it (string compare, never a
  date parse), returns observed/filtered counts, and leaves a non-array
  body untouched so the hub still sees what the API said. The submit
  carries `observed` and `filtered` beside `fetched_at`; the body stays
  the API's array. Python: `filter_battlelog()`, same semantics, pinned
  by `FilterTests`. The hub filters under its own mark regardless, so a
  collector that ignores the filter is correct, only wasteful - which is
  why the field is optional on both sides.
- `internal/crapi`, `internal/breaker` — CR API paths + the 403 breaker
  (shared helpers). `internal/worker` (SQS envelopes) and
  `internal/update` (GitHub-polling updater) were DELETED 2026-09-06
  with the transport they served; do not reintroduce either.
- `python/collector.py` — the stdlib-only twin; `python/test_collector.py`
  its tests. It ships as a release asset (`collector.py`) with its own
  SHA-256 line in SHA256SUMS, so operators pin and verify it like the
  binary. `release.yml` stamps the tag into `VERSION`; the checked-in
  value stays `py-dev` so a working-tree run is visibly a dev build in
  the admin version column. It never self-updates - that is the point
  of the twin, not a gap to close.
- `scripts/install.sh` — one-command install for macOS/Linux (download
  binary + supervise via launchd/systemd). `scripts/install.ps1` — the
  Windows equivalent (download .exe + register a Scheduled Task).
  `scripts/elixir-collector.service` (systemd unit),
  `scripts/run-forever.sh` (generic POSIX KeepAlive loop for DSM and
  other hosts without a supervisor; finds the binary in its own dir,
  its parent, or `$PWD`, falls back to the Python twin, and exits with
  a message rather than restart-looping when it finds neither).
  Start-up failures go to stderr AND are mirrored into the log, because
  DSM Task Scheduler discards stderr; the log rotates at `MAX_LOG_BYTES`
  (10 MB default, one generation) since volunteer hardware runs this for
  years. `scripts/test-run-forever.sh` (its tests; CI runs them under
  both `sh` and `dash`, dash standing in for BusyBox ash).
- `docs/recipes/` — where to run one. `cloud-init.yaml` is the single
  cloud recipe (any Linux VM with cloud-init: unprivileged user, .env
  at 600, the repo's own installer pinned to a tag, a hardened systemd
  unit with `ReadWritePaths` on the collector dir so self-update can
  swap the binary, doctor's verdict at the end of the cloud-init log);
  provider pages (Oracle Always Free, Hetzner) are the delta, never a
  second copy of the procedure. Synology/macOS/Windows are the hand
  recipes. No "last verified" headers - Jamie: ceremony. No
  DigitalOcean page - its Reserved IP is an alias with ambiguous
  egress. When the installer or the unit changes, the YAML changes in
  the same commit.
- `docs/GO-PORT.md` — design history (parts superseded by the zero-trust
  transition; see its postscript).

## Local services on this host

Jamie's machine runs the v2 pair, card identities Ram Rider (Go) and
Tesla (Python). **Neither runs out of this
checkout any more** (2026-09-06): both were moved to released artifacts
under `~/elixir-collectors/<name>/`, each with its own `.env` beside it,
so editing this repo cannot reach a live collector.

- `com.poapkings.elixir-mcp-gw-go` -> `~/elixir-collectors/ram-rider/collector`,
  a released Go binary. It SELF-UPDATES, so a door change no longer needs
  a hand bounce here.
- `com.poapkings.elixir-mcp-gw2-py` -> `~/elixir-collectors/tesla/collector.py`,
  the released Python twin (`py-<tag>`). It never self-updates by design;
  re-download it from a release when you want it current. Keep this one
  Python — one Go plus one Python is the whole point of the twin, and
  Jamie has said so explicitly.

Reload either with `launchctl unload/load` (a plist path or env change
needs a full reload, not `kickstart`). Logs:
`~/Library/Logs/elixir-mcp-gw-go.log` and `…-gw2-py.log`.

Do NOT run a staged collector by hand to check its version: if a `.env`
is already beside it you have just started a second live collector on
that identity. Read the version from the log after launchd starts it.

---

_This material is unofficial and is not endorsed by Supercell. For more
information see Supercell's Fan Content Policy:
www.supercell.com/fan-content-policy._
