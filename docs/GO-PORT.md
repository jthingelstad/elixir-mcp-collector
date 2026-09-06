# Go port — design of record (awaiting Jamie's review)

Jamie, 2026-09-06: "I'd rather jump straight to Go and not mess with
node." This doc is the review-before-build gate. The worker is ~520
lines of Node; the port itself is small. What deserves design is the
**self-update model** (a compiled binary can't `git pull`), the
**queue-contract mirror** (a second language must track
`packages/contracts` forever), and the **migration** (three live
collectors self-update from this repo's main — a careless push bricks
the fleet).

## 1. Shape

- Go 1.23+, pure Go (no cgo) → trivial cross-compilation.
- Release targets: `darwin-arm64` (the Macs), `linux-armv7`
  (the cabin DS416), `linux-arm64`, `linux-amd64`.
- Layout: `cmd/collector/main.go` + `internal/{worker,crapi,breaker,queue,update}`.
  Node stays in `src/` untouched until Phase 5 (see §5) — the live
  fleet keeps self-updating safely through the whole port.
- Config: the exact same `.env` in the repo/binary directory, same
  variable names — credentials are drop-in. `--instance 2` /`.env.gw2`
  parity kept.
- AWS SDK for Go v2 (sqs, cloudwatch). Everything else is stdlib
  (net/http, compress/gzip, crypto/sha256).

## 2. Behavior parity (the checklist the port is tested against)

Constants and behaviors pinned from the Node worker, not re-derived:

- 1.5s pacing floor between CR fetches (`MIN_FETCH_INTERVAL_MS 1500`).
- Circuit breaker: 5 consecutive 403s → open (learned live 2026-09-03).
- Live lane drained FIRST; bulk long-poll wait 4s so worst-case live
  pickup stays ~5s inside the server's 12s live window.
- Gzipped payloads; results carry the same metadata fields the server
  stamps today (gateway_id, sha/version, fetched_at).
- CloudWatch heartbeat: same Namespace/MetricName/Unit — the dead-man
  alarm and fleet panel must not notice the language change.
- Self-update refusal parity: a locally-built binary (version `dev`)
  NEVER auto-updates — the compiled equivalent of the dirty-checkout
  rule.

Golden-transcript test: capture real leased-job → fetched-result
message pairs from the Node worker (sanitized fixtures, committed);
the Go worker must produce byte-equivalent results envelopes.

## 3. Self-update: git-checkout model → signed-by-CI releases

Today: checkout IS the deployment; CI gates main; worker fast-forwards
hourly and exits; supervisor restarts. A binary needs a new contract:

- **CI release pipeline** (GitHub Actions, this repo): on every green
  main push — test, build the four targets with
  `-ldflags "-X main.version=<git describe>"`, publish a GitHub
  Release (`v0.X.Y` tags cut by CI, plus a moving `latest`) with
  `SHA256SUMS`.
- **Worker update loop** (hourly, same cadence as today): GET the
  latest release metadata; if its version differs from the embedded
  one, download the artifact for GOOS/GOARCH + SHA256SUMS, verify the
  checksum, write to a temp file next to the binary, atomic-rename
  over itself, exit(0). The supervisor (launchd KeepAlive / systemd
  Restart=always / run-forever.sh) restarts on the new code —
  identical semantics to the git model.
- **Failure = keep running**: any error in the update path (GitHub
  down, rate-limited, checksum mismatch) logs and skips the cycle;
  checksum mismatch also alarms via a heartbeat metric dimension.
  An unreachable GitHub never stops collection.
- **Pinning/rollback**: `COLLECTOR_PIN_VERSION=v0.3.2` in `.env`
  freezes a machine (shows on the fleet panel like a stale SHA does
  today); deleting the pin rejoins latest. [Historical: this env var is
  wired only into the retired v1 SQS path. A zero-trust v2 collector
  takes the version its server names, with no pin; to control your own
  version, run the Python twin or a self-built `dev` binary. README
  "Staying current" documents the live behavior.]
- **Integrity, honestly scoped**: SHA256 verification over HTTPS from
  the same release. Artifact _signing_ (key held outside GitHub) is
  deliberately deferred — with the signing key in GitHub Actions
  secrets it dies in the same compromise as the release itself, so it
  buys nothing until outside operators justify an offline-key
  ceremony. Recorded as the upgrade trigger: first non-Jamie operator.

## 4. Queue-contract mirror

`packages/contracts` (TypeScript, in elixir-mcp) stays the single
source of truth; this repo gains hand-written Go structs plus
**committed JSON fixtures** of canonical queue messages. Rules:

- Contract changes land server-side first (existing rule, unchanged);
  the same change updates the fixtures here, and the Go marshaling
  tests fail loudly on drift.
- The Go structs use exact wire names via json tags — never renamed
  Go-side.

## 5. Migration (order matters — the fleet self-updates from main)

0. **This doc reviewed by Jamie.** ← we are here
1. Go implementation + parity tests, additive in this repo. Node
   `src/` untouched; pushes stay safe for the live fleet.
2. CI release pipeline; first tagged release.
3. **Cabin DS416 goes first**: new gateway enrolled (cabin CR key on
   the cabin's static IP, per-gateway IAM user), runs the
   `linux-armv7` binary under Task Scheduler + run-forever.sh. Soaks
   for a few days — the newest code on the newest site, where a
   failure costs redundancy, not capture.
4. Cut over the Macs one at a time (jamie-mac-2 → kitchen-mac →
   jamie-mac), each: install binary + updated launchd plist, watch a
   full self-update cycle complete.
5. Remove `src/` (Node), rewrite README for the binary story, keep
   run-forever.sh/systemd/launchd scripts pointing at the binary.

Server-side (elixir-mcp) changes: none. The results envelope and
heartbeats are unchanged; `last_seen_sha` simply starts carrying the
embedded version string.

## 6. Risks on record

- Contract drift across languages → fixtures + server-first rule (§4).
- Release-pipeline compromise → checksums now, offline-key signing
  when outside operators arrive (§3).
- GitHub availability coupling → update-skip-and-keep-running (§3).
- armv7 is a first-class Go target; no unofficial anything.
- DSM has no user systemd → Task Scheduler + run-forever.sh (already
  shipped, works unchanged with `NODE_BIN` simply unused). [Historical:
  `NODE_BIN` is gone with the Node worker; run-forever.sh now falls back
  to the Python twin via `PYTHON_BIN`.]

## Postscript (2026-09-06): superseded by zero-trust v2, then deleted

The phased Mac cutover this doc planned happened in one night, for a
better reason than Go parity: the zero-trust transition
(elixir-mcp `docs/COLLECTOR-ZERO-TRUST.md`). Collectors are now pure
API clients; the Mac runs a Go + Python v2 pair; the Node worker is
retired; self-update trust moved from GitHub releases to the server's
update authority. Sections 3-5 of this doc are historical.

Second pass the same day: the SQS transport and the GitHub-polling
updater this doc designed were not just bypassed but removed.
`internal/worker`, `internal/update` and every AWS SDK dependency are
gone, leaving a stdlib-only module. Read §3-§5 as the reasoning that
led here, not as a description of anything that still runs.
