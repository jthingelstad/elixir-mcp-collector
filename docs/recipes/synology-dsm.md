# Synology DSM

Free if the NAS is already on; your home connection's IP (see below);
x86_64 or arm depending on the model; DSM Task Scheduler running
`run-forever.sh` at boot. No cloud-init here — this is the hand recipe
from the README, gathered in one place.

## The IP question

A NAS at home reaches the internet through your ISP's address. If that
address is static, or changes so rarely you are willing to update the
key allowlist when it does, this works. If it changes often, a cloud VM
with a reserved IP ([Oracle](oracle-always-free.md) is free) is less
trouble than a NAS. `collector doctor` shows the address in use.

## Steps

Over SSH, as your own user:

```sh
sudo mkdir -p /volume1/elixir-collector
sudo chown "$USER" /volume1/elixir-collector
cd /volume1/elixir-collector

printf 'CR_API_TOKEN=%s\nELIXIR_API_TOKEN=%s\n' "your-cr-key" "emcg_your-token" > .env
chmod 600 .env

# the installer and the supervisor, checked against the release
# signature before anything runs (README, "3. Run it")
base=https://github.com/jthingelstad/elixir-mcp-collector/releases/latest/download
curl -fsSL -O "$base/install.sh" -O "$base/run-forever.sh" -O "$base/SHA256SUMS" -O "$base/SHA256SUMS.sig" &&
echo 'elixir-mcp-collector-release ssh-ed25519 REPLACE_WITH_THE_RELEASE_PUBLIC_KEY' > allowed_signers &&
ssh-keygen -Y verify -f allowed_signers -I elixir-mcp-collector-release \
  -n elixir-mcp-collector-release -s SHA256SUMS.sig < SHA256SUMS &&
grep -E ' (install|run-forever)\.sh$' SHA256SUMS | sha256sum -c - &&
sh install.sh
sh run-forever.sh --check      # prints the binary it found and the log it will write
./collector doctor             # the verdict
```

Then Control Panel → Task Scheduler → Create → **Triggered Task** →
User-defined script, event **Boot-up**, run as your own user:

```sh
cd /volume1/elixir-collector && sh run-forever.sh
```

Always `sh run-forever.sh`, never `./run-forever.sh`: a file fetched
with curl carries no executable bit, and a boot task that dies on
"permission denied" tells you nothing. Run the task once from the
scheduler to start it now.

The log is `/volume1/elixir-collector/collector.log`, rotating at 10 MB.
Start-up failures land there too, because DSM discards stderr.

The Go binary self-updates; `run-forever.sh` restarts it after each
update. Nothing to maintain.
