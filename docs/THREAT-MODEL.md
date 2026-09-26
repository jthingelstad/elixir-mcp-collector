# Collector threat model

What a collector protects on the machine it runs on, what it relies on
the host for, and how that differs by platform. Written for issue #6;
the operator-facing summary is in the README.

## What is worth protecting

A collector holds two secrets and one capability:

| Asset | If it leaks |
|---|---|
| `ELIXIR_API_TOKEN` (`emcg_…`) | Someone can lease and submit as this collector until the maintainer revokes it. Revocation is instant and total. |
| `CR_API_TOKEN` | Only usable from the key's allowlisted IP, so mostly by someone already on this machine or its network. They could spend the key's rate budget. |
| The binary's directory | Whoever can write there can replace the collector, and the supervisor will run it. |

It holds nothing else: no AWS credentials, no database, no user data.
Elixir MCP stamps a collector's identity from its token and checks
every submit on the hub, so a compromised collector can submit wrong
data under its own name. It cannot impersonate another collector or
reach anything beyond three HTTPS endpoints.

## Who the adversaries are

1. **Another account on the same host** (a shared NAS, a multi-user
   Linux box). Wants the secrets or a foothold through the binary.
2. **The network between the collector and the hub or the CR API.**
   Wants the bearer, or to feed the collector hostile responses.
3. **A hostile or broken upstream**: a CR API response or door answer
   built to exhaust memory.
4. **A compromised release page, GitHub account or workflow.** Wants
   to push code to the fleet.
5. **A compromised hub** (its database or release rows). Wants the
   same, through the update authority.

A local root or administrator is out of scope. They own the machine,
and no user-space program can protect its secrets from them.

## What the collector guarantees itself (every platform)

| Against | Guarantee | Where |
|---|---|---|
| 1 | A group- or world-readable `.env` is tightened to owner-only at startup and logged. Doctor reports it. The collector repairs and warns, and never refuses to start, so an automatic update cannot stop a collector that was running. | `internal/envfile` |
| 1 | Self-update writes a new file under a random name, created exclusively (`O_EXCL`) in the binary's directory, then fsyncs it and checks it is still the file it wrote. It runs it once with `version` (no tokens, no `.env`) and requires the named version, then renames it over the binary in one step and fsyncs the directory. Nothing follows a link planted in that directory, and a failure leaves the old binary in place. The replaced binary is kept until the new one gets an answer from the hub; a new one that keeps crashing before that is rolled back and its version refused on this host. | `installBinary`, `fallback.go` in `internal/v2` |
| 2 | The bearer is sent over HTTPS only. A plain `http://` `ELIXIR_API_BASE` is switched to `https://`, except on loopback. A redirect from https down to http is refused, and Go already drops `Authorization` when a redirect goes to another host. | `v2.SecureBase`, `crapi.RefuseDowngrade` |
| 3 | Every response read is capped at its limit plus one byte, after gzip decoding: 8 MiB from the CR API (reported to the hub as an overflow), 1 MiB from the door (a clear error), 200 MiB for an update. A declared `Content-Length` over the limit is refused before any read. | `crapi.Fetch`, `callWithHTTP`, `applyUpdate` |
| 4, 5 | The client installs only the exact version and SHA-256 named by the hub's `/config`, never "latest", and only when, before anything runs the download: the URL is this repository's release asset for that version and platform, with redirects limited to GitHub's asset hosts over HTTPS; the release's `SHA256SUMS` is signed (ed25519, SSHSIG) by the key compiled into the client and lists that SHA-256 and that version; and the version is not below the install floor. So neither the hub nor GitHub alone can hand a collector code. A compromised hub can choose only among signed releases at or above the floor, and a compromised release page cannot sign. | `trust.go` in `internal/v2`, SECURITY.md |
| all | Logs never carry either secret, the environment, or a response body. Doctor shows only the last four characters of each secret. | tests in `internal/v2`, `internal/doctor` |

## What the collector reports, and what that is worth

Every door call carries, beside the bearer, three headers describing
the build: `x-collector-version`, `x-collector-binary-sha256` (the
SHA-256 of the running executable, hashed once at startup) and
`x-collector-release-key` (the `SHA256:` fingerprint of each compiled
release key, comma-separated during a rotation). None is secret; the
test suite checks neither carries the token.

This is **self-reported telemetry**. An operator controls the binary
and the network and can send any value, so the hub page built on it
is for honest operators: it shows which collectors run a signed
release and which run a dev build, a fork or a stale binary. It is not
an attestation and must not gate anything that matters against
adversary 1 or a dishonest operator.

Within that, the two headers carry different weight. The key
fingerprint alone proves nothing: the key is in the source, so a dev
build or a fork carries it too; it says only which key the collector
trusts. The evidence is the binary hash matching the signed hash the
hub holds for that version and platform from naming it. Code in
`internal/v2/report.go`.

## What the host has to provide, by platform

| | Linux + systemd (our unit, cloud-init) | macOS (launchd agent) | Windows (Scheduled Task) | NAS / no supervisor (`run-forever.sh`) |
|---|---|---|---|---|
| `.env` private | mode 600, which the collector repairs itself; the unit also sets `UMask=0077` | mode 600, which the collector repairs itself | a user-only ACL (you and SYSTEM, inheritance off), set and checked by `install.ps1`; **the collector does not check Windows ACLs** | mode 600, which the collector repairs itself. DSM shares can carry ACLs that POSIX modes do not show, so keep the folder in a share only you can read |
| Process sandbox | yes: no capabilities, `NoNewPrivileges`, the filesystem read-only except the collector's directory, private `/tmp` and devices, the kernel hidden, `@system-service` syscalls, IP and Unix sockets only | none; runs as your user with your user's access | none; runs as your user | none; runs as the user the task is set to (keep it off `root`) |
| Binary directory writable only by the collector's account | yes, if you follow the recipe (`/opt/elixir-collector` owned by `collector`) | your home folder | your profile folder | a folder you own (README, Synology steps) |
| Restart on crash, on the watchdog and after an update; stop on exit 2 | `Restart=always`, `RestartPreventExitStatus=2` | `KeepAlive`, **but launchd does not stop on exit 2** | task restart settings; **exit 2 is retried** | the loop stops on exit 2 |
| Update swap never leaves the start path empty | yes (link aside, one rename) | yes | two renames; `run-collector.cmd` restores `collector.exe.prev` if power is lost between them (installs from before 2026-09-26 need `install.ps1` re-run) | yes |
| A crash-looping update is restarted into the rollback | yes: a trial panic dies by SIGABRT, which `RestartPreventExitStatus=2` does not match | `KeepAlive` restarts any exit | task restart settings (exit 2 is retried) | yes: SIGABRT is not exit 2 |
| Tested in CI | the unit under real systemd on ubuntu-latest (x86_64) and ubuntu-24.04-arm (arm64): start, `.env` repair, self-update through the sandbox, refusal of a release signed by another key, rollback of a release that crashes at startup, exit 2 | `go test` on macos-latest, including the fallback's real-process rollback | `go test` (including the rollback of a running .exe) and the ACL, wrapper and download-and-verify blocks of `install.ps1` on windows-latest | `run-forever.sh` under `sh` and `dash` |

Gaps a later change could close:

- The collector does not check the ACL on a Windows `.env`. It would
  need a raw `advapi32` call, and the module has no dependencies.
- The hardened unit is tested on x86_64 and arm64, not armv7.
  `SystemCallArchitectures=native` is left out on purpose so the armv7
  build works on a 64-bit ARM kernel.
- The update rollback cannot tell a release whose every request fails
  before a response (broken TLS setup, say) from a hub outage, so it
  does not roll that back. It also runs inside the release it guards,
  so a release that breaks the guard itself is not covered.
- A compromised hub can still name an older signed release at or above
  the install floor (a replay). The floor only moves up in a signed
  release, so narrowing that is a release away, not instant. A leaked
  signing key plus a compromised hub is fleet-wide code execution until
  the key is rotated out (SECURITY.md).
- The first release that verifies signatures was installed by a client
  that did not, on the strength of the hub's hash alone.
- launchd and Task Scheduler restart after exit 2 (a missing token).
  That costs a restart every 10 seconds or every minute, not a failure.
