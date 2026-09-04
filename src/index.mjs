#!/usr/bin/env node
/**
 * elixir-mcp-gw entrypoint: config from repo-root .env (CR_API_TOKEN is
 * the canonical var — AGENTS.md rule 1), queue-name indirection via
 * GetQueueUrl (one bundle serves N gateways), split heartbeats (Drop's
 * pattern: process-alive every 60s; work-succeeding only on completed
 * fetches — the second is what catches a silently broken IP binding).
 */

import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  SQSClient,
  GetQueueUrlCommand,
  ReceiveMessageCommand,
  SendMessageCommand,
  DeleteMessageCommand,
} from "@aws-sdk/client-sqs";
import {
  CloudWatchClient,
  PutMetricDataCommand,
} from "@aws-sdk/client-cloudwatch";
import { makeWorker } from "./worker.mjs";
import { makeCrFetch } from "./cr-api.mjs";
import { CircuitBreaker } from "./breaker.mjs";
import { checkForUpdate, currentSha } from "./self-update.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, "..");

async function loadEnv() {
  const env = { ...process.env };
  // Multiple gateways on one host each get their own env file
  // (ELIXIR_MCP_ENV_FILE in the LaunchAgent); default stays repo .env.
  const envFile =
    process.env.ELIXIR_MCP_ENV_FILE ?? path.join(repoRoot, ".env");
  try {
    const text = await readFile(envFile, "utf8");
    for (const line of text.split("\n")) {
      const m = /^([A-Z0-9_]+)=(.*)$/.exec(line.trim());
      if (m && env[m[1]] === undefined) {
        env[m[1]] = m[2];
        // The AWS SDK reads process.env directly; under launchd there is
        // no shell environment, so .env must be exported for real.
        if (process.env[m[1]] === undefined) process.env[m[1]] = m[2];
      }
    }
  } catch {
    /* env-only */
  }
  return env;
}

const env = await loadEnv();
const required = (name) => {
  if (!env[name]) {
    console.error(`missing required config: ${name}`);
    process.exit(2);
  }
  return env[name];
};

const token = required("CR_API_TOKEN");
const gatewayId = required("ELIXIR_MCP_GATEWAY_ID");
const gatewayName = env.ELIXIR_MCP_GATEWAY_NAME ?? "gw";
const region = env.AWS_REGION ?? "us-east-1";

const sqsClient = new SQSClient({ region });
const cw = new CloudWatchClient({ region });

async function queueUrl(name) {
  const { QueueUrl } = await sqsClient.send(
    new GetQueueUrlCommand({ QueueName: name }),
  );
  return QueueUrl;
}

const queues = {
  live: await queueUrl(
    env.ELIXIR_MCP_QUEUE_LIVE ?? "elixir-mcp-cr-requests-live",
  ),
  bulk: await queueUrl(
    env.ELIXIR_MCP_QUEUE_BULK ?? "elixir-mcp-cr-requests-bulk",
  ),
  results: await queueUrl(
    env.ELIXIR_MCP_QUEUE_RESULTS ?? "elixir-mcp-cr-results",
  ),
};

const NAMESPACE = `ElixirMCP/Gateway/${gatewayName}`;
async function putMetric(name, value = 1) {
  try {
    await cw.send(
      new PutMetricDataCommand({
        Namespace: NAMESPACE,
        MetricData: [{ MetricName: name, Value: value, Unit: "Count" }],
      }),
    );
  } catch (err) {
    console.error(`metric ${name} failed: ${err.message}`);
  }
}

const abort = new AbortController();
process.on("SIGINT", () => abort.abort());
process.on("SIGTERM", () => abort.abort());

const workerSha = await currentSha();
const worker = makeWorker({
  gatewaySha: workerSha,
  sqs: {
    async receive(url, waitSeconds) {
      const { Messages } = await sqsClient.send(
        new ReceiveMessageCommand({
          QueueUrl: url,
          MaxNumberOfMessages: 1,
          WaitTimeSeconds: waitSeconds,
          VisibilityTimeout: 60,
        }),
      );
      const m = Messages?.[0];
      return m ? { body: m.Body, receiptHandle: m.ReceiptHandle } : null;
    },
    async send(url, body) {
      await sqsClient.send(
        new SendMessageCommand({ QueueUrl: url, MessageBody: body }),
      );
    },
    async delete(url, receiptHandle) {
      await sqsClient.send(
        new DeleteMessageCommand({
          QueueUrl: url,
          ReceiptHandle: receiptHandle,
        }),
      );
    },
  },
  queues,
  crFetch: makeCrFetch({ token }),
  breaker: new CircuitBreaker(),
  gatewayId,
  metrics: {
    fetchSucceeded: () => putMetric("FetchSucceeded"),
    overflow: () => putMetric("ResultOverflow"),
    breakerOpen: () => putMetric("BreakerOpen"),
  },
  log: (level, msg) =>
    console.error(JSON.stringify({ t: new Date().toISOString(), level, msg })),
});

// Process-alive heartbeat, independent of work outcomes.
const heartbeat = setInterval(() => putMetric("Heartbeat"), 60_000);
putMetric("Heartbeat");

console.error(
  JSON.stringify({
    t: new Date().toISOString(),
    level: "info",
    msg: "gateway up",
    sha: workerSha,
  }),
);

// Self-update: hourly; on update, exit 0 and let KeepAlive restart us
// onto the new code. First check is delayed a minute so a crash-looping
// bad release still gets updated by the NEXT push rather than blocking.
const selfUpdate = setInterval(async () => {
  const updated = await checkForUpdate(
    (level, msg) =>
      console.error(
        JSON.stringify({ t: new Date().toISOString(), level, msg }),
      ),
    workerSha,
  );
  if (updated) {
    clearInterval(heartbeat);
    clearInterval(selfUpdate);
    abort.abort();
  }
}, 3_600_000);
while (!abort.signal.aborted) {
  try {
    const r = await worker.pollOnce();
    if (r.polled === "breaker_open")
      await new Promise((res) => setTimeout(res, 5000));
  } catch (err) {
    console.error(
      JSON.stringify({
        t: new Date().toISOString(),
        level: "error",
        msg: err.message,
      }),
    );
    await new Promise((res) => setTimeout(res, 5000));
  }
}
clearInterval(heartbeat);
