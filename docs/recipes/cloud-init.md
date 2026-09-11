# The cloud VM recipe

Every Linux cloud VM with cloud-init is the same recipe. What differs
between providers is only how you get a **stable outbound IPv4** and
where the **user data** box is on their create-instance form, so this
page is the whole procedure and the provider pages are short deltas.

The file is [`cloud-init.yaml`](cloud-init.yaml). Read it once; it is
short. It creates an unprivileged `collector` user, writes your two
secrets to `/opt/elixir-collector/.env` at mode 600, installs the
collector binary with the repository's own installer (SHA-256-verified
against the release), installs a hardened systemd unit, starts it, and
runs `collector doctor` so the cloud-init log ends with a verdict.

## Steps

1. **Raise your hand** at <https://elixir.poapkings.com> → Status →
   Collectors and get your `emcg_` token (README step 1). Do this
   first: the token is the thing you paste.
2. **Reserve a static public IPv4** on the provider *before* creating
   the instance where the provider lets you (Oracle: reserved public IP;
   Hetzner: primary IP). That is the address you allowlist on your Clash
   Royale key, and reserving it separately from the VM is what lets you
   destroy and rebuild the VM without touching the key.
3. **Create a Clash Royale API key** at
   <https://developer.clashroyale.com> allowlisted to that IP.
4. **Paste `cloud-init.yaml`** into the provider's user-data field with
   the two `REPLACE_WITH_…` values filled in. Any Linux image with
   cloud-init works; the smallest x86_64 or arm64 shape is plenty (the
   collector is idle most of the time).
5. **Boot, wait a minute, read the verdict:**

   ```sh
   sudo tail -20 /var/log/cloud-init-output.log
   sudo systemctl status elixir-collector
   sudo journalctl -u elixir-collector -f
   ```

   The doctor output at the end of the cloud-init log says `healthy`,
   or says in words what is wrong. Its `egress` line is the IP the
   world sees; if it is not the one on your key, the `clash_royale`
   line says so and names the fix. Rerun any time:

   ```sh
   sudo runuser -u collector -- /opt/elixir-collector/collector doctor
   ```

Within a few minutes the collector shows on
<https://elixir.poapkings.com/data/status> under its card name.

## Two caveats, stated plainly

**Secrets in user data are not private to you.** Whatever you paste is
readable from inside the instance at the metadata service
(`169.254.169.254`) and in the provider's console for the life of the
instance. For a box that runs nothing but this collector that is an
acceptable trade for one-paste setup. If it is not acceptable to you,
delete the `.env` entry from `write_files`, boot, and write the file by
hand over SSH before starting the service:

```sh
sudo install -o collector -g collector -m 600 /dev/null /opt/elixir-collector/.env
sudoedit /opt/elixir-collector/.env      # CR_API_TOKEN=... and ELIXIR_API_TOKEN=emcg_...
sudo systemctl restart elixir-collector
```

Exit code 2 from the collector (no `.env`) stops the service rather than
looping, so a box booted without the file simply waits for you.

**Pin, don't float.** The YAML fetches the installer at a release tag
(`TAG=`), and the installer verifies the binary it downloads against
that release's `SHA256SUMS`. Keep the tag: a rebuild next year then does
what this one did. The running binary self-updates to whatever the
server names, so pinning costs you nothing in currency.

## Variations

- **Python instead of the Go binary.** Set `ExecStart=/usr/bin/python3
  /opt/elixir-collector/collector.py` and replace the installer line
  with the release download from the README ("Prefer Python"). Know
  that the Python twin never self-updates; on a box nobody logs into,
  the Go binary is the better default.
- **Rebuilding.** Destroy the VM, keep the reserved IP, paste the same
  YAML. Nothing on the Elixir side changes: same token, same identity.
- **Removing a collector.** Ask the maintainer to revoke the token (or
  do it yourself if the console offers it), then delete the VM and
  release the IP. There is nothing else to tear down.
