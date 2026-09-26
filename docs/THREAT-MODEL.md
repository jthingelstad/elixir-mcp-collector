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
4. **A compromised release page.** Wants to push code to the fleet.

A local root or administrator is out of scope. They own the machine,
and no user-space program can protect its secrets from them.

## What the collector guarantees itself (every platform)

| Against | Guarantee | Where |
|---|---|---|
| 1 | A group- or world-readable `.env` is tightened to owner-only at startup and logged. Doctor reports it. The collector repairs and warns, and never refuses to start, so an automatic update cannot stop a collector that was running. | `internal/envfile` |
| 1 | Self-update writes a new file under a random name, created exclusively (`O_EXCL`) in the binary's directory, then fsyncs it and checks it is still the file it wrote. It renames it over the binary in one step and fsyncs the directory. Nothing follows a link planted in that directory, and a failure leaves the old binary in place. | `installBinary` in `internal/v2` |
| 2 | The bearer is sent over HTTPS only. A plain `http://` `ELIXIR_API_BASE` is switched to `https://`, except on loopback. A redirect from https down to http is refused, and Go already drops `Authorization` when a redirect goes to another host. | `v2.SecureBase`, `crapi.RefuseDowngrade` |
| 3 | Every response read is capped at its limit plus one byte, after gzip decoding: 8 MiB from the CR API (reported to the hub as an overflow), 1 MiB from the door (a clear error), 200 MiB for an update. A declared `Content-Length` over the limit is refused before any read. | `crapi.Fetch`, `callWithHTTP`, `applyUpdate` |
| 4 | The client installs only the exact version and SHA-256 named by the hub's `/config`, never "latest". | README, "Staying current" |
| all | Logs never carry either secret, the environment, or a response body. Doctor shows only the last four characters of each secret. | tests in `internal/v2`, `internal/doctor` |

## What the host has to provide, by platform

| | Linux + systemd (our unit, cloud-init) | macOS (launchd agent) | Windows (Scheduled Task) | NAS / no supervisor (`run-forever.sh`) |
|---|---|---|---|---|
| `.env` private | mode 600, which the collector repairs itself; the unit also sets `UMask=0077` | mode 600, which the collector repairs itself | a user-only ACL (you and SYSTEM, inheritance off), set and checked by `install.ps1`; **the collector does not check Windows ACLs** | mode 600, which the collector repairs itself. DSM shares can carry ACLs that POSIX modes do not show, so keep the folder in a share only you can read |
| Process sandbox | yes: no capabilities, `NoNewPrivileges`, the filesystem read-only except the collector's directory, private `/tmp` and devices, the kernel hidden, `@system-service` syscalls, IP and Unix sockets only | none; runs as your user with your user's access | none; runs as your user | none; runs as the user the task is set to (keep it off `root`) |
| Binary directory writable only by the collector's account | yes, if you follow the recipe (`/opt/elixir-collector` owned by `collector`) | your home folder | your profile folder | a folder you own (README, Synology steps) |
| Restart on crash, on the watchdog and after an update; stop on exit 2 | `Restart=always`, `RestartPreventExitStatus=2` | `KeepAlive`, **but launchd does not stop on exit 2** | task restart settings; **exit 2 is retried** | the loop stops on exit 2 |
| Tested in CI | the unit under real systemd on ubuntu-latest (x86_64): start, `.env` repair, self-update through the sandbox, exit 2 | `go test` on macos-latest | `go test` and the ACL block of `install.ps1` on windows-latest | `run-forever.sh` under `sh` and `dash` |

Gaps a later change could close:

- The collector does not check the ACL on a Windows `.env`. It would
  need a raw `advapi32` call, and the module has no dependencies.
- The hardened unit is tested on x86_64 only. The directives are not
  architecture-specific, and `SystemCallArchitectures=native` is left
  out on purpose so the armv7 build works on a 64-bit ARM kernel.
- launchd and Task Scheduler restart after exit 2 (a missing token).
  That costs a restart every 10 seconds or every minute, not a failure.
