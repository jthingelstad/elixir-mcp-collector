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
gzipped results back. No AWS, no database, no cloud access. One
implementation, in Go (`cmd/collector`, `internal/`). The old Node
worker and the SQS transport were retired 2026-09-06 (zero-trust door +
Postgres job ledger) and their code was DELETED the same day; do not
resurrect them. The Go module now has no third-party dependencies at
all, which is what makes "no AWS access" a property of the build rather
than a promise.

**Go only since 2026-09-26 (Jamie's decision).** A stdlib Python twin
used to ship beside the binary as insurance against a bad Go release.
It was deleted because it doubled every change and, never
self-updating, forced the hub to keep retired contract fields alive for
it (the `poll` block, issue #7). Do not bring it back or add a second
implementation: release insurance moves into the Go updater, and
candidates soak as a dev build (rule 8).

**Versions are v3.0.x since 2026-09-26 (Jamie's decision).** v2.0.x
named the zero-trust client generation that speaks config/lease/submit.
That contract is unchanged, but the client around it moved enough to
be a generation of its own: Go only, an updater that rolls back a
release that cannot start (PR #10), and releases that are signed and
verified before they run (issue #5). `release.yml` numbers each green
push with the next patch no tag has used (`scripts/next-version.sh`),
so the merge that moved the major built exactly v3.0.0 and nobody tags
by hand. Never reuse or lower a number: the hub compares
major.minor.patch numerically, and every v3 must stay above every
v2.0.x.

## Rules

1. **This repo is PUBLIC; secrets never enter it — or agent context.**
   Config is gitignored `.env*` files (`CR_API_TOKEN`,
   `ELIXIR_API_TOKEN`), mode 0600, handled by file/name reference only.
   Verify with `git ls-files`, never trust `.gitignore` alone. (A
   `.env.v2-go` once slipped past a too-narrow ignore and leaked to the
   public remote — `.gitignore` now ignores all `.env.*` except
   `.env.example`. Rotate on any exposure.)
2. **The server owns the contract AND the behavior.** The client speaks
   config/lease/submit; the server computes each CR path, says when to
   check in again (`next_check_in_s` on every lease grant, empty
   answer and lease-cap refusal; since 2026-09-11 a collector never
   asks the door to wait, and every collector serves priority work
   first - there is no live channel), and
   hands out pacing/breaker/backoff at launch. Collection changes never require a
   client change. The queue/API contract is canonical in
   `jthingelstad/elixir-mcp` (`packages/contracts`); this repo's tests
   pin the shapes it produces so drift fails here first, and a contract
   change lands server-side first. Every field the client reads must
   survive the server dropping it: an absent field decodes to 0, so
   wherever 0 would be wrong the client falls back to the hub's current
   value (`idleCheckIn`, `defaultPacingMS`, `defaultOverflowBytes`, the
   breaker's defaults) - the lesson of issue #7.
3. **One global rate budget.** More collectors = resilience, never quota
   multiplication (ToS posture). Pacing (~1.5 s floor) and the 5×403
   circuit breaker are load-bearing and server-configured; never remove
   them. 429s honor `Retry-After`.
4. **Self-update obeys the UPDATE AUTHORITY, and a release must prove
   who published it.** A release build installs only the exact version
   + SHA-256 the server's config endpoint names (key
   `go-<GOOS>-<GOARCH>`), and only when (`internal/v2/trust.go`, issue
   #5, all checked before anything executes the download): the URL is
   this repo's release asset for exactly that version and platform,
   with redirects only to GitHub's asset hosts; the release's
   `SHA256SUMS` carries an SSHSIG ed25519 signature by the key compiled
   into `releasekey.go` and lists that hash AND a `VERSION` line for
   that version (the anti-replay binding); and the version is at or
   above `installFloor`. The signature adds to the hub, never replaces
   it: neither the hub nor GitHub alone can push code. Verification is
   stdlib only (`crypto/ed25519`); do not add Sigstore/cosign or any
   dependency for it. The private key lives only in the
   `COLLECTOR_SIGNING_KEY` secret of the `release` environment (deployment
   branches: main only); never generate, print or
   handle it here. `release.yml` refuses to build while the compiled
   key is the placeholder or the secret is missing, and verifies what
   it is about to publish with the collector's own verifier. A
   verification failure is an ordinary failed update (`self-update
   REFUSED`, retried hourly), never a `.refused` file. Naming an older
   signed release still rolls the fleet back; a release from before
   signing must first be signed by the `sign-release` workflow
   (SECURITY.md). `installFloor` is the monotonicity rule: it only
   moves up. The check rides the `/config` call at startup
   and hourly after, so naming a version reaches the fleet within the
   hour. There is NO pin and no opt-out, deliberately: the fleet shares
   one rate budget and one contract, so a stale client is everyone's
   problem. Do not add a pin flag, not even one for canaries (Jamie,
   2026-09-25; rule 8 says how a candidate soaks). Dev builds cannot
   self-update; their operators update when asked. Refusing a stale
   client is the hub's job, not the client's: its `CollectorMinEnforce`
   stack parameter makes the door answer `lease` and `submit` with 426
   `client_too_old` below `min_client_version`. It never refuses
   `config` (the channel a stale client updates through) and fails open
   on a version it cannot parse (`dev`), deliberately; neither property
   is to be "fixed" (elixir-mcp `docs/DECISIONS.md`, "Releases are
   candidates until named"). The client does not enforce
   `min_client_version`. The `update` block `/config` serves is the
   hub's `collector_release` ledger, one row per platform, written when
   the maintainer names a release (elixir-mcp
   `infra/scripts/name-collector-release.mjs`; procedure in
   `docs/RELEASING-COLLECTOR.md`). Cross-platform: release.yml
   builds macOS (arm64/amd64), Windows (amd64/arm64), and Linux
   (amd64/arm64/armv7), plus the pinned installers, `VERSION`,
   `SHA256SUMS` and `SHA256SUMS.sig`. **The way back from a bad release lives in the
   updater** (`internal/v2/fallback.go`, 2026-09-26). Before the swap,
   the staged candidate is run with `collector version` (first thing
   in `main`: no `.env`, no tokens, no network) and must report the
   named version. A release older than `version` is recognised by its
   token-check exit and accepted, so naming an older release still
   rolls the fleet back. The swap keeps the replaced binary as
   `<bin>.prev` and opens `<bin>.trial`. Until the first door response
   (any response, the watchdog's rule), `v2.Guard` counts starts that
   died without a clean exit. After three it restores `.prev`, writes
   `<bin>.refused`, and exits 1. Every deliberate exit before the proof
   goes through `Trial.End`, so an outage is never a crash; keep it
   that way when adding an exit path to `main`. A trial panics by
   SIGABRT, not exit 2, or systemd and `run-forever.sh` would stop
   instead of restarting into the rollback. `.refused` holds exactly
   one version: the one that failed on this machine. It clears itself
   the moment the hub names any other. It is NOT a pin and must never
   grow into one: no flag, no env var, no operator-chosen version.
   Windows swaps with two renames (the running .exe is renamed aside),
   and `run-collector.cmd` restores `.prev` if power is lost between
   them. README "Staying current" is the operator-facing version of all
   this and must stay true to `internal/v2/v2.go` and `fallback.go`.
5. **Exit codes are the supervisor contract.** 2 means the config will
   never work (a missing token): `run-forever.sh` stops, systemd has
   `RestartPreventExitStatus=2`, and no supervisor should spin on it.
   Anything else is restartable - crash, watchdog, or self-update.
6. **Durability: exit rather than wedge.** The client runs a progress
   watchdog — 5 minutes with no successful server round-trip and the
   process exits(1) so the supervisor restarts it clean. Any door
   response (even an error) counts as progress. This exists because a
   door redeploy once wedged the dev collectors (alive but not
   progressing); launchd KeepAlive only restarts a process that EXITS.
7. **Observability: the log shows work.** The client emits a JSON
   activity summary every 5 minutes (jobs done, fetch errors, channel).
   Don't log per-fetch (too noisy at ~40/min).
8. **A published release is a CANDIDATE, not a shipment.** Every green
   push publishes one as a PRERELEASE, and it reaches nobody until
   Elixir MCP names it — naming also promotes that release to Latest.
   `releases/latest` is what `install.sh`, `install.ps1` and the README
   hand a new operator, so promotion-on-naming keeps a fresh install
   matched to what the fleet actually runs. Soak a candidate before
   naming it by running a dev build of its commit
   (`go build -o collector ./cmd/collector`) on one machine: a released
   binary cannot soak, because it downgrades itself to the named version
   within seconds, while a dev build never self-updates. A dev build
   does not exercise the update path. The updater now carries the
   insurance the twin used to: a candidate that fails `version`, or
   crashes before its first door response, is rolled back on each
   machine (rule 4). That net has holes, so still review changes to
   startup and self-update with the fleet in mind. A release whose
   requests all fail before any response (broken TLS, a bad base URL)
   looks like an outage and is not rolled back. A release that breaks
   `v2.Guard` or the updater itself breaks the net it would fall into.
   And the first release carrying the fallback was installed without
   it. Never flip the
   prerelease flag by hand or the release page starts lying about what
   is live. Procedure, including rollback and the platform-key trap
   that fails silently: `docs/RELEASING-COLLECTOR.md` in the elixir-mcp
   repo.

9. Work lands on `main`; CI (`gofmt`, `go vet`, `go test`, and the
   `run-forever.sh` and `install.sh` shell tests under `sh` and `dash`,
   the workflow hygiene check, `go test` on macOS and Windows, the
   `install.ps1` ACL, wrapper and verify tests, and the hardened unit
   under real systemd on x86_64 and arm64, all in
   `.github/workflows/validate.yml`) is the pre-push gate. Every
   Action is pinned to a full commit SHA with its tag in a comment,
   every workflow has `permissions: {}` and each job asks for its own,
   and no checkout persists the token (`scripts/test-workflows.sh`
   enforces all three). `main`
   must stay releasable — `release.yml` builds the seven platform
   artifacts from green main. Operator-facing behavior changes update
   `README.md` in the same commit.

## Layout

- `cmd/collector/main.go` — entrypoint. Requires `CR_API_TOKEN` and
  `ELIXIR_API_TOKEN`; missing either is exit 2. The module has ZERO
  third-party dependencies (stdlib only) since the SQS path went.
- `internal/v2/` — the zero-trust client (config/lease/submit, watchdog,
  activity log, update authority). `trust.go` is the release trust
  chain (URL and redirect allowlist, SSHSIG verification, the VERSION
  binding, the install floor) and `releasekey.go` the compiled-in
  public key; `trust_test.go` covers bad signature, wrong host,
  redirect escape, downgrade, replay, interrupted write and rollback,
  interop with real `ssh-keygen`, and (`RELEASE_DIR`,
  `REQUIRE_RELEASE_KEY`) the checks `release.yml` runs. Issue #6 hardening lives here too:
  `SecureBase` (http -> https except loopback; repair, never refuse),
  bounded door/update reads, and `installBinary` (exclusive random temp
  file beside the binary, fsync, same-file check, atomic rename), with
  the fallback on top of it in `fallback.go`: `installSteps.check`
  runs the candidate's self-check, the previous binary is kept, and
  `beforeSwap` writes the trial. `testdata/fakecollector` is a stand-in
  release (prove / outage / crash / legacy) that `fallback_test.go` and
  `scripts/test-systemd-unit.sh` build to crash and roll back for real.
- `internal/envfile/` — `.env` loading. A group/world-readable file is
  tightened to owner-only at startup and logged (doctor loads without
  repairing and reports). Repair-and-warn, never refuse: a refusal would
  stop a collector that ran fine before an automatic update.
- `internal/doctor/` — the operator preflight (`collector doctor
  [--json]`). Five read-only checks, all of which run; exit 0 healthy /
  1 broken / 2 valid-but-not-yet-active; secrets shown as their last
  four characters in every mode.
  It NEVER leases (a diagnostic lease would orphan a real job for its
  TTL) - it reads `/config`, which since 2026-09-11 answers `pending`
  tokens, returns 403 `revoked`, echoes `observed_ip`, and names the one
  CR path (`doctor.cr_path`) doctor may read. `doctor_test.go` pins the
  report's wording.
- `internal/filter/` — what a lease asks the collector to drop before
  submitting (2026-09-11): `filter.battles_after` on a bulk-lane
  battlelog lease (never a live one) is the newest battleTime the hub
  holds, in the API's own spelling; `Battlelog()` keeps the entries
  after it (string compare, never a date parse), returns
  observed/filtered counts, and leaves a non-array
  body untouched so the hub still sees what the API said. The submit
  carries `observed` and `filtered` beside `fetched_at`; the body stays
  the API's array. The hub filters under its own mark regardless, so a
  collector that ignores the filter is correct, only wasteful - which is
  why the field is optional on both sides.
- `internal/crapi`, `internal/breaker` — CR API paths + the 403 breaker
  (shared helpers). `crapi.Fetch` reads at most `MaxBodyBytes`+1 (after
  gzip decoding) and marks a bigger body `TooLarge`, which v2 submits as
  an overflow; `crapi.RefuseDowngrade` is the redirect policy for every
  client that carries a bearer. `internal/worker` (SQS envelopes) and
  `internal/update` (GitHub-polling updater) were DELETED 2026-09-06
  with the transport they served; do not reintroduce either.
- `scripts/install.sh` — one-command install for macOS/Linux (download
  binary + supervise via launchd/systemd). The copies a release
  publishes are pinned by `scripts/pin-installers.sh` to that release's
  tag and binary checksums, and the README has operators verify them
  against the signed `SHA256SUMS` before running; the repository copies
  resolve Latest. `scripts/install.ps1` — the
  Windows equivalent (download .exe + register a Scheduled Task).
  `scripts/elixir-collector.service` (systemd unit, sandboxed; its
  `ReadWritePaths` must be the collector dir or self-update fails, and
  the header documents the `elixir-collector@.service` / `%i` form some
  operators run; `scripts/test-systemd-unit.sh` runs it under real
  systemd in CI, installed as a template), `install.sh` tightens a loose
  `.env`, `install.ps1` sets a user-only ACL between `# BEGIN env-acl`
  markers that `scripts/test-install-acl.ps1` runs on windows-latest,
  `scripts/run-forever.sh` (generic POSIX KeepAlive loop for DSM and
  other hosts without a supervisor; finds the binary in its own dir,
  its parent, or `$PWD`, and exits with a message rather than
  restart-looping when it finds none). Start-up failures go to stderr
  AND are mirrored into the log, because DSM Task Scheduler discards
  stderr; the log rotates at `MAX_LOG_BYTES` (10 MB default, one
  generation) since volunteer hardware runs this for years. `scripts/test-run-forever.sh` (its tests; CI runs them under
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
- `SECURITY.md` — private reporting, the release key, verifying a
  release by hand, key generation/rotation/compromise, and the rollback
  drill. `.github/workflows/sign-release.yml` backfills a signature onto
  a pre-signing release so the hub can name it as a rollback.
- `docs/THREAT-MODEL.md` — what the collector guarantees on the host
  itself vs what each platform's supervisor provides. Keep its tables
  true when the unit, the installers or the client change.
- `docs/GO-PORT.md` — design history (parts superseded by the zero-trust
  transition; see its postscript).

## Running collectors

This repo says how to install and run a collector, never which ones are
running. Running state belongs to the hub: Admin -> Collectors (or the
`elixir_collectors` tool) is the only authority for which collectors
exist, their status and their version. To learn which hub collector a
local install is, run `collector doctor`: it reads `/config` and prints
the collector's card and name. Do not record instances, hosts, service
labels or paths here, the maintainer's included (Jamie, 2026-09-26): a
list in a public install repo goes stale and misleads.

Live collectors run from released artifacts, never from a checkout, so
editing this repo cannot reach one. Do NOT run a staged binary by hand
beside a live `.env` to check its version: that starts a second live
collector on the same identity. Use `collector version`, which reads no
`.env` and touches no network, or `collector doctor`, which never
leases.

---

_This material is unofficial and is not endorsed by Supercell. For more
information see Supercell's Fan Content Policy:
www.supercell.com/fan-content-policy._
