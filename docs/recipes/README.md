# Recipes: where to run a collector

The hosting provider is not the real variable. What differs between
setups is two things — **how you get a stable outbound IPv4** (the
Clash Royale key is allowlisted to it) and **what keeps the process
running** — so there is one canonical cloud recipe and the provider
pages are short deltas.

## Which page

| You have | Static public IP? | Cost | Go to |
|---|---|---|---|
| Nothing always-on | — | $0 | [Oracle Always Free](oracle-always-free.md) — a reserved IP and a VM that costs nothing |
| Nothing always-on, would rather pay a little | — | ~€4/mo | [Hetzner](hetzner.md) — a primary IP that outlives the server |
| Any other cloud VM with cloud-init | reserved/elastic IP | provider's | [The cloud VM recipe](cloud-init.md) — the same YAML everywhere |
| A Synology NAS | your ISP's | $0 | [Synology DSM](synology-dsm.md) |
| A Mac that stays awake | your ISP's | $0 | [macOS](macos.md) |
| A Windows PC that stays on | your ISP's | $0 | [Windows](windows.md) |

"Your ISP's" means the collector goes out through your home or office
address. That is fine if it is static or changes rarely (you update the
key's allowlist when it does); if it changes often, a $0 cloud VM with a
reserved IP is less trouble.

Every recipe ends the same way: `collector doctor` (or
`python3 collector.py --check`), which says in words whether the box is
healthy, waiting to be promoted, or broken and how.

## Not documented on purpose

**DigitalOcean.** Its Reserved IP is an alias: outbound traffic may
still leave from the Droplet's original address, which is exactly the
property a collector cannot have, and IPv4 has become a separate line
item there. Not worth a page with an ambiguous egress story.

## Contribute one

Raspberry Pi, Proxmox LXC, unRAID, OPNsense, another VPS: if you run a
collector somewhere not listed, copy [`_template.md`](_template.md),
write the delta from the nearest recipe, end with your doctor output,
and open a pull request adding a row above.
