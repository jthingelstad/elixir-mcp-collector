#!/usr/bin/env node
/**
 * Install elixir-mcp-gw as a launchd LaunchAgent (Drop's installer
 * pattern): RunAtLoad + KeepAlive, ThrottleInterval 10, pinned Node
 * binary (brew upgrades must not move the runtime under the service),
 * stdout/stderr to ~/Library/Logs. Re-run to update.
 */

import { writeFile, mkdir } from "node:fs/promises";
import { execFileSync } from "node:child_process";
import path from "node:path";
import os from "node:os";
import { fileURLToPath } from "node:url";

// --instance N installs an additional gateway on this host with its own
// label, log, and env file (repo .env.gwN) — multi-gateway operation.
const instArg = process.argv.indexOf("--instance");
const instance = instArg > -1 ? Number(process.argv[instArg + 1]) : 1;
const suffix = instance > 1 ? String(instance) : "";
const LABEL = `com.poapkings.elixir-mcp-gw${suffix}`;
const here = path.dirname(fileURLToPath(import.meta.url));
const entry = path.resolve(here, "../src/index.mjs");
const node = process.execPath;
const home = os.homedir();
const logPath = path.join(home, `Library/Logs/elixir-mcp-gw${suffix}.log`);
const plistPath = path.join(home, "Library/LaunchAgents", `${LABEL}.plist`);

const plist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${node}</string>
    <string>${entry}</string>
  </array>
  ${
    instance > 1
      ? `<key>EnvironmentVariables</key>
  <dict><key>ELIXIR_MCP_ENV_FILE</key><string>${path.resolve(here, `../.env.gw${instance}`)}</string></dict>`
      : ""
  }
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>${logPath}</string>
  <key>StandardErrorPath</key><string>${logPath}</string>
</dict>
</plist>
`;

await mkdir(path.dirname(plistPath), { recursive: true });
await writeFile(plistPath, plist);
const uid = process.getuid();
const domain = `gui/${uid}`;
try {
  execFileSync("launchctl", ["bootout", domain, plistPath], {
    stdio: "ignore",
  });
} catch {
  /* not loaded */
}
execFileSync("launchctl", ["bootstrap", domain, plistPath]);
execFileSync("launchctl", ["kickstart", "-k", `${domain}/${LABEL}`]);
console.log(
  `installed + started ${LABEL}\n  node: ${node}\n  log:  ${logPath}`,
);
