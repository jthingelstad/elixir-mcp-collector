/**
 * The lease loop — Drop's seam, extended (DESIGN §5.1):
 *  - live lane drained FIRST (interactive requests never queue behind
 *    scheduled polls); bulk long-polls only when live is empty;
 *  - every response body gzipped (SQS 256KB posture); post-compression
 *    overflow is a loud error result + metric, never a silent fallback;
 *  - 403s feed the circuit breaker; open breaker = no leasing;
 *  - the SQS visibility timeout is the lease: post result, then delete.
 */

import { gzipSync } from "node:zlib";
import { crPath } from "./cr-api.mjs";

const MAX_RESULT_BYTES = 250_000; // headroom under the 256KB SQS cap

// CR pacing (§5.2): the docs' ~2s guidance, and overage surfaces as 403.
// The scheduler budget caps the AVERAGE rate; this floor caps the
// INSTANTANEOUS rate — without it a queued burst rips at wire speed and
// trips the breaker (learned live, 2026-09-03: 92-job fan-out, 5x403).
const MIN_FETCH_INTERVAL_MS = 1500;

export function makeWorker({
  sqs, // { receive(queueUrl, waitSeconds), send(queueUrl, body), delete(queueUrl, receiptHandle) }
  queues, // { live, bulk, results }
  crFetch,
  breaker,
  gatewayId,
  gatewaySha = null, // checkout SHA; rides results so the fleet-version panel is real
  metrics = { fetchSucceeded() {}, overflow() {}, breakerOpen() {} },
  log = () => {},
  now = () => new Date(),
  sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
}) {
  let lastFetchStartedAt = 0;

  async function pacedFetch(path) {
    const wait = lastFetchStartedAt + MIN_FETCH_INTERVAL_MS - now().getTime();
    if (wait > 0) await sleep(wait);
    lastFetchStartedAt = now().getTime();
    return crFetch(path);
  }
  function buildResult(job, fetched) {
    const base = {
      v: 1,
      job,
      gateway_id: gatewayId,
      ...(gatewaySha ? { gateway_sha: gatewaySha } : {}),
      fetched_at: now().toISOString(),
    };
    if (fetched.kind === "transport") {
      return {
        ...base,
        status: "error",
        error: { kind: "transport", message: fetched.message },
      };
    }
    if (fetched.status !== 200) {
      return {
        ...base,
        status: "error",
        http_status: fetched.status,
        error: { kind: "http", message: `HTTP ${fetched.status}` },
      };
    }
    const gz = gzipSync(Buffer.from(fetched.bodyText, "utf8"));
    const b64 = gz.toString("base64");
    if (b64.length > MAX_RESULT_BYTES) {
      metrics.overflow();
      return {
        ...base,
        status: "error",
        http_status: 200,
        error: {
          kind: "overflow",
          message: `gzipped body ${b64.length}B exceeds cap`,
        },
      };
    }
    return { ...base, status: "ok", http_status: 200, body_gzip_b64: b64 };
  }

  async function handleLease(queueUrl, message) {
    let job;
    try {
      job = JSON.parse(message.body);
      crPath(job); // throws on unknown endpoint before we spend a CR call
    } catch (err) {
      // Malformed job: never fetch; let it re-lease toward the DLQ.
      log("warn", `unleasable job: ${err.message}`);
      return { handled: false };
    }

    const fetched = await pacedFetch(crPath(job));
    if (fetched.kind === "http" && fetched.status === 403) {
      const opened = breaker.record403();
      if (opened) {
        metrics.breakerOpen();
        log("warn", "circuit breaker OPEN after consecutive 403s");
      }
    } else if (fetched.kind === "http" && fetched.status === 200) {
      breaker.recordSuccess();
    }

    const result = buildResult(job, fetched);
    await sqs.send(queues.results, JSON.stringify(result));
    await sqs.delete(queueUrl, message.receiptHandle);
    if (result.status === "ok") metrics.fetchSucceeded();
    return { handled: true, status: result.status, lane: job.lane };
  }

  /** One iteration: live first, then bulk. Returns what happened. */
  async function pollOnce() {
    if (breaker.isOpen()) {
      return { polled: "breaker_open" };
    }
    let message = await sqs.receive(queues.live, 1);
    let queueUrl = queues.live;
    if (!message) {
      message = await sqs.receive(queues.bulk, 10);
      queueUrl = queues.bulk;
    }
    if (!message) return { polled: "empty" };
    return { polled: "job", ...(await handleLease(queueUrl, message)) };
  }

  return { pollOnce, handleLease, buildResult };
}
