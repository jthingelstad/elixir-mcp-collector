# AGENTS.md

elixir-mcp-collector: the operator-run fetch worker for Elixir MCP.
Split from the main repo 2026-09-04 so operators clone something small.
`CLAUDE.md` is a symlink to this file. Do not fork them.

## Rules

1. **This repo is PUBLIC and secrets never enter it — or agent context.**
   Config lives in gitignored `.env` files (canonical var `CR_API_TOKEN`),
   mode 0600, handled by file/name reference only. Verify tracking with
   `git ls-files`, never trust `.gitignore` alone.
2. **The server owns the contract.** The queue message shapes are canonical
   in `jthingelstad/elixir-mcp` (`packages/contracts`) and enforced at
   ingest; `test/worker.test.mjs` pins the shape this worker produces so
   drift fails here first. A contract change lands server-side first.
3. **One global rate budget.** More collectors = resilience, never quota
   multiplication (ToS posture). The 1.5 s pacing floor and the 403
   circuit breaker are load-bearing; never remove them.
4. **Deploys are `git push`.** Collectors self-update hourly from green
   `main` (fast-forward only; dirty/diverged checkouts skip). There is no
   other deploy path — keep `main` releasable and CI green.
5. Work lands on `main`; `npm run verify` (prettier + tests) is the
   pre-push gate.
6. Operator docs live in README.md and ship with any behavior change,
   same commit.

## Local services on this host

Jamie's machine runs two instances: `com.poapkings.elixir-mcp-gw`
(repo-root `.env`) and `...-gw2` (`.env.gw2`). Manual restart:
`launchctl kickstart -k gui/$UID/com.poapkings.elixir-mcp-gw`.

---

_This material is unofficial and is not endorsed by Supercell. For more
information see Supercell's Fan Content Policy:
www.supercell.com/fan-content-policy._
