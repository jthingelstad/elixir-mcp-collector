# Security

The collector runs on volunteers' machines and replaces its own binary
when Elixir MCP tells it to. That makes the update path the thing to
protect, and this page is about it: how to report a problem, what a
collector verifies before it runs new code, the key that signs
releases, and what the maintainer does when something goes wrong.

## Reporting a vulnerability

Report privately through GitHub: this repository's **Security** tab →
**Report a vulnerability**. Please do not open a public issue for
anything that could let someone run code on a collector host, read an
operator's tokens, or impersonate a collector. You will get an answer in
the advisory thread. Once a fix is named to the fleet, the advisory is
published with credit if you want it.

Only the release Elixir MCP currently names is supported: collectors
update themselves to it within the hour, so a fix ships by naming it.

## What a collector checks before it runs an update

The hub (Elixir MCP's `/config`) is the update authority. It names one
version, SHA-256 and download URL per platform, and a collector installs
nothing else. On top of that, a release has to prove who published it.
All of this happens before anything executes the download:

1. **The URL** must be exactly this repository's release asset for the
   named version and the collector's platform:
   `https://github.com/jthingelstad/elixir-mcp-collector/releases/download/<version>/<asset>`.
   A redirect may go only to GitHub's asset hosts
   (`objects.githubusercontent.com`, `release-assets.githubusercontent.com`)
   over HTTPS.
2. **The signature.** The release's `SHA256SUMS` must carry a valid
   `SHA256SUMS.sig` made by the release key compiled into the running
   binary (`internal/v2/releasekey.go`). The signed file must list the
   hub-named hash for this platform, and a `VERSION` line whose hash
   is that of the named version. The `VERSION` line is what stops a
   signed file from one release being replayed as another.
3. **The download** must match the hub-named SHA-256.
4. **The floor.** Nothing older than `installFloor` in
   `internal/v2/trust.go` is installed, however it is signed. That is
   the version-monotonicity rule. Above the floor, naming an older
   release is the hub's rollback lever and keeps working.

Only then does the updater run the new binary once (`collector version`,
no tokens, no network) and swap it in. After that comes the trial and
rollback described in the README's "Staying current". A release that
fails any check is refused, loudly (`self-update REFUSED`). The
collector keeps collecting on the binary it has and checks again at the
next hourly config call.

So neither the hub alone nor GitHub alone can push code to a collector.
A compromised hub can name only releases this repository published and
signed, at or above the floor. A compromised release page or asset has
no valid signature, and a compromised signing key still needs the hub to
name its release.

The rest of the host-side picture is in
[`docs/THREAT-MODEL.md`](docs/THREAT-MODEL.md).

## The release key

```
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFvN1mZGTcFXSGnIXf8h33cxAhvrHPYn80BO5FkELh28 elixir-mcp-collector-release
```

This is an OpenSSH ed25519 key. Its private half exists only as the
`COLLECTOR_SIGNING_KEY` secret of the repository's `release`
environment, whose deployment branches are `main` only (plus the maintainer's
offline backup). `release.yml` signs every release's `SHA256SUMS` with
it under the namespace `elixir-mcp-collector-release`. It then verifies
the result with the collector's own verifier and with `ssh-keygen`
before publishing. A release build refuses to run while the key above
is still the placeholder, or while the secret is missing.

The same line is compiled into the collector, printed in the README,
and baked into `docs/recipes/cloud-init.yaml`. A test fails if any of
them drift apart.

### Verifying a release by hand

With OpenSSH 8.1 or newer (macOS 11+, current Linux distributions,
Windows 10 1809+ with the OpenSSH client):

```sh
base=https://github.com/jthingelstad/elixir-mcp-collector/releases/latest/download
# (or .../releases/download/<tag> for a particular release)
curl -fsSL -O "$base/SHA256SUMS" -O "$base/SHA256SUMS.sig"
echo 'elixir-mcp-collector-release ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFvN1mZGTcFXSGnIXf8h33cxAhvrHPYn80BO5FkELh28' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I elixir-mcp-collector-release \
  -n elixir-mcp-collector-release -s SHA256SUMS.sig < SHA256SUMS
```

`Good "elixir-mcp-collector-release" signature` means every hash in
`SHA256SUMS` is the one this repository published. Check any file
against it with `sha256sum -c` (or `shasum -a 256 -c` on macOS).

### Generating the release key (maintainer, once)

Run this locally, never in CI or a shared session. The key has no
passphrase because CI uses it unattended. Its protection is the Actions
secret, plus an offline backup you keep yourself.

```sh
umask 077
mkdir -p ~/.elixir-mcp-release-key && cd ~/.elixir-mcp-release-key
ssh-keygen -t ed25519 -N '' -C elixir-mcp-collector-release -f collector-release

# The private half goes straight from the file into the secret; it is
# never printed or pasted. The release environment must already exist,
# with deployment branches limited to main (Settings -> Environments).
gh secret set COLLECTOR_SIGNING_KEY --env release --repo jthingelstad/elixir-mcp-collector < collector-release

ssh-keygen -lf collector-release.pub   # the fingerprint collectors will log
```

Back up `collector-release` offline, for example as a secure note in
your password manager. Without it, signing moves to a new key through
rotation (below), which needs a release signed with the old one. Then,
in a checkout of this repository, put the public half everywhere the
placeholder is:

```sh
PUB="$(cut -d' ' -f1,2 ~/.elixir-mcp-release-key/collector-release.pub)"
perl -pi -e "s|ssh-ed25519 REPLACE_WITH_THE_RELEASE_PUBLIC_KEY|$PUB|g" \
  internal/v2/releasekey.go README.md SECURITY.md docs/recipes/cloud-init.yaml
go test ./internal/v2
REQUIRE_RELEASE_KEY=1 go test ./internal/v2 -run '^TestCompiledReleaseKey$' -v
```

Commit that as its own change. Its push is the first release that is
signed and that verifies signatures.

### Rotating the key

`releasePublicKeys` takes several lines, and a collector trusts a
signature by any of them.

1. Generate the new key as above, but do not replace the secret yet.
2. Add its public line beside the old one in `releasekey.go`, the
   README, this file and the cloud-init, macOS and Synology recipes
   (`TestPublishedKeyMatchesTheCompiledOne` checks all of them). Release and name it.
   This release is signed with the old key and trusts both.
3. Wait until the fleet runs it (Admin → Collectors, Version column).
4. Replace the `release` environment's secret with the new private key. Remove the old line,
   then release and name that. It is signed with the new key, and the
   fleet already trusts it.

### If the signing key leaks

A leaked key alone does not reach the fleet. The attacker also needs
the hub to name their release, at a URL in this repository's releases.
Still, treat a leak as urgent:

1. Rotate as above, as fast as the fleet takes a release, and delete
   the old secret.
2. Check the release list for anything you did not publish, and the
   hub's `collector_release` rows for anything you did not name.
3. If a release signed with the leaked key must never run again, raise
   `installFloor` above it in the rotation release.

If the hub itself is compromised, it can still name any signed release
at or above the floor. Raising the floor in a new release narrows that,
and revoking the hub's release rows stops it.

## Rolling the fleet back

Rollback is naming, not deleting (elixir-mcp
`docs/RELEASING-COLLECTOR.md`): name the previous release and every
collector installs it at its next config call, through the same checks
and the same trial as any update.

- **To a signed release:** name it. Nothing else is needed.
- **To a release from before signing:** a verifying collector refuses
  it, because it has no `SHA256SUMS.sig`. Run the **sign-release**
  workflow (Actions → sign-release → Run workflow, tag `v2.0.NN`)
  first. It checks the release's `SHA256SUMS` against its own assets,
  adds `VERSION`, and signs it. Then name it. Do this ahead of time for
  the release you would fall back to, not during an incident. A release
  from before signing does not verify updates itself, so name a signed
  release again as soon as the problem is fixed.
- **Below `installFloor`:** not possible by naming. A release below the
  floor has a reason to be there.

**Drill it.** Once a quarter, or before a risky release: name the
previous release, watch one collector you control log `rolling back`,
then `update trial` and `proven`. Then name the current release again
and watch it come back. Confirm the fleet followed on Admin →
Collectors. A rollback lever that is never pulled is not known to work.
