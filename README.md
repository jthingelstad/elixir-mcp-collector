# Elixir MCP Collector

The operator-run half of [Elixir MCP](https://elixir.poapkings.com): a small
worker that leases fetch jobs from a queue, fetches from the Clash Royale API
with your IP-bound key, and posts gzipped results back. Collectors never
choose their own targets and hold no user data. More collectors mean
redundancy and resilience — never a bigger rate budget: the fleet shares one
global 1 rps budget by design.

Running one earns its owner a higher daily tool-call quota, a Clash Royale
card avatar, and a spot on the collector ladder.

You need:

- a machine that stays on (macOS with launchd is the supported path; the
  worker itself is plain Node and runs anywhere),
- a **static public IP** (Supercell keys are IP-allowlisted),
- Node 24+.

## 1. Raise your hand

Sign in at <https://elixir.poapkings.com/dashboard> and use **Run a gateway**:
pick a short name and submit your static IP. Your collector appears as
`pending`.

The owner then does two manual steps for you:

- creates a Clash Royale API key **allowlisting your IP** (the key stays in
  the owner's Supercell developer account — instant revocation, one ToS
  story for the whole fleet),
- creates a per-collector IAM user whose only permissions are: receive/delete
  on the two request queues, send on the results queue, and PutMetricData.

You'll receive the key, the AWS credentials, and your `gateway_id` out of
band (never through this repo or the site).

## 2. Install

```sh
git clone https://github.com/jthingelstad/elixir-mcp-collector.git
cd elixir-mcp-collector && npm install
```

Create `.env` in the repo root, mode 0600 — this file is gitignored and must
never be committed:

```sh
CR_API_TOKEN=<the key you received>
ELIXIR_MCP_GATEWAY_ID=<your gateway_id>
ELIXIR_MCP_GATEWAY_NAME=<your collector name>
AWS_ACCESS_KEY_ID=<per-collector IAM user>
AWS_SECRET_ACCESS_KEY=<per-collector IAM user>
AWS_REGION=us-east-1
```

Then install the LaunchAgent (RunAtLoad + KeepAlive, logs to
`~/Library/Logs/elixir-mcp-gw.log`):

```sh
node scripts/install-launchd.mjs
```

Running more than one collector on a host (each with its own key and
gateway_id): put the second instance's config in `.env.gw2` and install
with `--instance 2` (own label, own log).

Check the log for `leased` / `fetched` lines within a couple of minutes.

## 3. Staying current

The worker self-updates: once an hour it fast-forwards to this repo's
`main` (CI-gated) and restarts itself. A dirty or locally-diverged checkout
never auto-updates — experiment freely; your version shows on the fleet
panel until you rejoin main.

## 4. Probation → active

Once your collector heartbeats, the owner moves it to `probation`. It does
real work immediately; after a few clean days it's flipped to `active`.
`draining` means no new work (planned retirement or a tripped breaker);
`revoked` means the server refuses its results and the key + IAM user are
deleted.

## What a collector can and can't do

- It fetches exactly the `(endpoint, entity_key)` pairs it leases — targets
  are pinned server-side, priority-ordered by Elixir MCP.
- Every payload is schema-validated at ingest regardless of source, and
  every fetch is attributed to your gateway_id.
- It paces itself (1.5 s floor between fetches) and opens a circuit breaker
  on consecutive 403s rather than hammering the API.
- It never sees accounts, emails, or sessions — only public CR data.

---

_This material is unofficial and is not endorsed by Supercell. For more
information see Supercell's Fan Content Policy:
www.supercell.com/fan-content-policy._
