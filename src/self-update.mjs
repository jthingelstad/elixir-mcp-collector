/**
 * Gateway self-update (Jamie, 2026-09-04): launchd runs the worker from
 * the git checkout, so the checkout IS the deployment. Hourly, compare
 * the checkout against origin/main; when behind, fast-forward pull and
 * EXIT — launchd's KeepAlive restarts on the new code within seconds.
 *
 * Only green main lands (CI gates every push), and a misbehaving
 * gateway is one click from revoked — the trust model already covers
 * the residual risk. A dirty or diverged checkout never auto-updates:
 * operators experimenting locally keep their state, and the stale SHA
 * shows in the fleet-version panel instead.
 */

import { execFile } from "node:child_process";
import { promisify } from "node:util";
import path from "node:path";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repoRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);

async function git(...args) {
  const { stdout } = await exec("git", ["-C", repoRoot, ...args], {
    timeout: 60_000,
  });
  return stdout.trim();
}

/** The checkout's current SHA (short) — reported in heartbeats. */
export async function currentSha() {
  try {
    return await git("rev-parse", "--short", "HEAD");
  } catch {
    return "unknown";
  }
}

/**
 * Returns true when an update was applied (caller should exit so
 * launchd restarts onto the new code).
 */
export async function checkForUpdate(log, startupSha = null) {
  try {
    // If the checkout has moved since this process started (a deploy on
    // a dev machine, or a prior pull), restart regardless of origin —
    // the running code is stale even when HEAD == origin/main.
    if (startupSha) {
      const headShort = await git("rev-parse", "--short", "HEAD");
      if (headShort !== startupSha) {
        log(
          "info",
          `self-update: checkout moved ${startupSha} -> ${headShort} under the running process; exiting for restart`,
        );
        return true;
      }
    }
    const dirty = await git("status", "--porcelain");
    if (dirty) {
      log("info", "self-update: checkout dirty, skipping");
      return false;
    }
    await git("fetch", "--quiet", "origin", "main");
    const local = await git("rev-parse", "HEAD");
    const remote = await git("rev-parse", "origin/main");
    if (local === remote) return false;
    const behind = await git("rev-list", "--count", `HEAD..origin/main`);
    const ahead = await git("rev-list", "--count", `origin/main..HEAD`);
    if (Number(ahead) > 0) {
      log("warn", "self-update: checkout diverged from origin/main, skipping");
      return false;
    }
    await git("merge", "--ff-only", "origin/main");
    log(
      "info",
      `self-update: advanced ${behind} commits to ${remote.slice(0, 8)}; exiting for restart`,
    );
    return true;
  } catch (err) {
    log("warn", `self-update failed (will retry next cycle): ${err.message}`);
    return false;
  }
}
