# AGENTS.md

elixir-mcp-collector: the operator-run fetch worker for Elixir MCP.
Split from the main repo 2026-09-04 so operators clone something small.
`CLAUDE.md` is a symlink to this file. Do not fork them.

## Rules

1. **This repo is PUBLIC and secrets never enter it — or agent context.**
   Config lives in gitignored `.env` files (canonical var `CR_API_TOKEN`),
   mode 0600, handled by file/name reference only. Verify tracking with
   `git ls-files`, never trust `.gitignore` alone.
2. **The server owns the contract — and, in v2, the behavior.** The
   zero-trust clients (Go `internal/v2`, `python/collector.py`) speak
   three HTTPS routes on Elixir MCP (config/lease/submit): the server
   computes CR paths, assigns the channel, and hands out pacing/breaker
   constants at launch. Collection changes never require a client
   change. The legacy SQS queue shapes remain canonical in
   `jthingelstad/elixir-mcp` (`packages/contracts`).
3. **One global rate budget.** More collectors = resilience, never quota
   multiplication (ToS posture). Pacing and the 403 breaker are
   load-bearing and server-configured; never remove them. 429s honor
   Retry-After.
4. **Self-update obeys the UPDATE AUTHORITY.** A release build installs
   only the exact version + SHA-256 the server's config endpoint names
   (key `go-<GOOS>-<GOARCH>`); a compromised release page alone cannot
   push code to operators. Dev builds never self-update. The old
   git-pull/GitHub-releases-trusting paths are retired with the Node
   worker.
5. Work lands on `main`; `npm run verify` (prettier + tests) is the
   pre-push gate.
6. Operator docs live in README.md and ship with any behavior change,
   same commit.

## Local services on this host

Jamie's machine runs the zero-trust v2 pair (cutover 2026-09-06):
`com.poapkings.elixir-mcp-gw-go` (Go binary `bin/collector-v2`, env
`.env.v2-go`) and `com.poapkings.elixir-mcp-gw2-py`
(`python/collector.py`, env `python/.env`). Deliberate runtime
diversity: one bad client release can never silence both. Manual
restart: `launchctl kickstart -k gui/$UID/<label>`. The old Node
services (`...-gw`, `...-gw2`) are unloaded; the Node worker under
`src/` is retired and slated for deletion.

---

_This material is unofficial and is not endorsed by Supercell. For more
information see Supercell's Fan Content Policy:
www.supercell.com/fan-content-policy._
