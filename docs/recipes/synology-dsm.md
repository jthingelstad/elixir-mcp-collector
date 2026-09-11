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

curl -fsSL https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/install.sh | sh
curl -fsSL -o run-forever.sh https://raw.githubusercontent.com/jthingelstad/elixir-mcp-collector/main/scripts/run-forever.sh
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
