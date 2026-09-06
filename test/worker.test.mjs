import { test } from "node:test";
import assert from "node:assert/strict";
import { gunzipSync } from "node:zlib";
import { makeWorker } from "../src/worker.mjs";
import { CircuitBreaker } from "../src/breaker.mjs";
import { crPath } from "../src/cr-api.mjs";
// The queue contract's canonical validator lives in elixir-mcp
// (packages/contracts) and is enforced at ingest; this repo asserts the
// shape it PRODUCES so drift fails here first.
function assertResultShape(msg) {
  const errors = [];
  if (msg.v !== 1) errors.push("v");
  if (!msg.job || typeof msg.job.endpoint !== "string")
    errors.push("job.endpoint");
  if (!msg.job || typeof msg.job.entity_key !== "string")
    errors.push("job.entity_key");
  if (!msg.job || !["live", "bulk"].includes(msg.job.lane))
    errors.push("job.lane");
  if (typeof msg.gateway_id !== "string" || !msg.gateway_id)
    errors.push("gateway_id");
  if (Number.isNaN(Date.parse(msg.fetched_at))) errors.push("fetched_at");
  if (!["ok", "error"].includes(msg.status)) errors.push("status");
  if (msg.status === "ok" && typeof msg.body_gzip_b64 !== "string")
    errors.push("body_gzip_b64");
  return { ok: errors.length === 0, errors };
}

const GW = "00000000-0000-0000-0000-000000000001";

function fakeSqs() {
  const queues = { live: [], bulk: [], results: [] };
  const urls = { live: "q://live", bulk: "q://bulk", results: "q://results" };
  const byUrl = (url) => queues[Object.keys(urls).find((k) => urls[k] === url)];
  return {
    queues,
    urls,
    sqs: {
      async receive(url, _wait) {
        const q = byUrl(url);
        return q.length > 0
          ? { body: q.shift(), receiptHandle: `rh-${url}-${q.length}` }
          : null;
      },
      async send(url, body) {
        byUrl(url).push(body);
      },
      deleted: [],
      async delete(url, rh) {
        this.deleted.push(`${url}:${rh}`);
      },
    },
  };
}

function worker({ fetchResult, breaker, sqsBox, metrics }) {
  return makeWorker({
    sqs: sqsBox.sqs,
    queues: sqsBox.urls,
    crFetch: async () => fetchResult,
    breaker: breaker ?? new CircuitBreaker(),
    gatewayId: GW,
    metrics,
    now: () => new Date("2026-09-03T16:00:00Z"),
  });
}

const JOB = JSON.stringify({
  endpoint: "player",
  entity_key: "#20JJJ2CCRU",
  lane: "bulk",
});

test("CR paths encode the hash and cover all admitted endpoints", () => {
  assert.equal(
    crPath({ endpoint: "player", entity_key: "#20JJJ2CCRU" }),
    "/players/%2320JJJ2CCRU",
  );
  assert.equal(
    crPath({ endpoint: "player_battlelog", entity_key: "#20JJJ2CCRU" }),
    "/players/%2320JJJ2CCRU/battlelog",
  );
  assert.equal(
    crPath({ endpoint: "clan", entity_key: "#J2RGCRVG" }),
    "/clans/%23J2RGCRVG",
  );
  assert.equal(
    crPath({ endpoint: "currentriverrace", entity_key: "#J2RGCRVG" }),
    "/clans/%23J2RGCRVG/currentriverrace",
  );
});

test("live lane drains before bulk", async () => {
  const box = fakeSqs();
  box.queues.live.push(
    JSON.stringify({
      endpoint: "player",
      entity_key: "#20JJJ2CCRU",
      lane: "live",
    }),
  );
  box.queues.bulk.push(JOB);
  const w = worker({
    fetchResult: {
      kind: "http",
      status: 200,
      bodyText: '{"tag":"#20JJJ2CCRU"}',
    },
    sqsBox: box,
  });
  const first = await w.pollOnce();
  assert.equal(first.lane, "live");
  const second = await w.pollOnce();
  assert.equal(second.lane, "bulk");
});

test("ok result is a valid, gunzippable contract message and the lease is deleted", async () => {
  const box = fakeSqs();
  box.queues.bulk.push(JOB);
  const w = worker({
    fetchResult: {
      kind: "http",
      status: 200,
      bodyText: '{"tag":"#20JJJ2CCRU","name":"Jamie"}',
    },
    sqsBox: box,
  });
  const r = await w.pollOnce();
  assert.equal(r.status, "ok");
  assert.equal(box.queues.results.length, 1);
  const msg = JSON.parse(box.queues.results[0]);
  const validated = assertResultShape(msg);
  assert.ok(validated.ok, JSON.stringify(validated));
  const body = JSON.parse(
    gunzipSync(Buffer.from(msg.body_gzip_b64, "base64")).toString(),
  );
  assert.equal(body.name, "Jamie");
  assert.equal(msg.gateway_id, GW);
  assert.equal(
    box.sqs.deleted.length,
    1,
    "request deleted after result posted",
  );
});

test("non-200 posts an http error result; 404 does not trip the breaker", async () => {
  const box = fakeSqs();
  box.queues.bulk.push(JOB);
  const breaker = new CircuitBreaker({ threshold: 2 });
  const w = worker({
    fetchResult: {
      kind: "http",
      status: 404,
      bodyText: '{"reason":"notFound"}',
    },
    sqsBox: box,
    breaker,
  });
  await w.pollOnce();
  const msg = JSON.parse(box.queues.results[0]);
  assert.equal(msg.status, "error");
  assert.equal(msg.http_status, 404);
  assert.equal(breaker.isOpen(), false);
});

test("consecutive 403s open the breaker and leasing stops", async () => {
  const box = fakeSqs();
  box.queues.bulk.push(JOB, JOB, JOB);
  const breaker = new CircuitBreaker({ threshold: 2, now: () => 1000 });
  let breakerOpened = 0;
  const w = worker({
    fetchResult: {
      kind: "http",
      status: 403,
      bodyText: '{"reason":"accessDenied"}',
    },
    sqsBox: box,
    breaker,
    metrics: {
      fetchSucceeded() {},
      overflow() {},
      breakerOpen: () => (breakerOpened += 1),
    },
  });
  await w.pollOnce();
  await w.pollOnce();
  assert.equal(breakerOpened, 1);
  const r = await w.pollOnce();
  assert.equal(r.polled, "breaker_open", "open breaker stops leasing");
  assert.equal(
    box.queues.bulk.length,
    1,
    "remaining job left for another gateway",
  );
  assert.equal(
    box.queues.results.length,
    2,
    "in-flight leases still posted their error results",
  );
});

test("breaker half-opens after cooldown and a success resets it", async () => {
  let clock = 0;
  const breaker = new CircuitBreaker({
    threshold: 2,
    cooldownMs: 100,
    now: () => clock,
  });
  breaker.record403();
  breaker.record403();
  assert.equal(breaker.isOpen(), true);
  clock = 200;
  assert.equal(
    breaker.isOpen(),
    false,
    "half-open probe allowed after cooldown",
  );
  breaker.recordSuccess();
  breaker.record403();
  assert.equal(
    breaker.isOpen(),
    false,
    "reset streak: one 403 no longer opens",
  );
});

test("transport failure posts a transport error result", async () => {
  const box = fakeSqs();
  box.queues.bulk.push(JOB);
  const w = worker({
    fetchResult: { kind: "transport", message: "timeout" },
    sqsBox: box,
  });
  const r = await w.pollOnce();
  assert.equal(r.status, "error");
  const msg = JSON.parse(box.queues.results[0]);
  assert.equal(msg.error.kind, "transport");
});

test("post-compression overflow is a loud error, never a silent fallback", async () => {
  const box = fakeSqs();
  box.queues.bulk.push(JOB);
  // Incompressible body: random bytes as hex still compress poorly enough.
  const big = Buffer.from(
    Array.from({ length: 400_000 }, () => Math.floor(Math.random() * 256)),
  ).toString("base64");
  let overflows = 0;
  const w = worker({
    fetchResult: { kind: "http", status: 200, bodyText: big },
    sqsBox: box,
    metrics: {
      fetchSucceeded() {},
      overflow: () => (overflows += 1),
      breakerOpen() {},
    },
  });
  await w.pollOnce();
  const msg = JSON.parse(box.queues.results[0]);
  assert.equal(msg.status, "error");
  assert.equal(msg.error.kind, "overflow");
  assert.equal(overflows, 1);
});

test("malformed job is left to re-lease toward the DLQ, no CR call spent", async () => {
  const box = fakeSqs();
  box.queues.bulk.push('{"endpoint":"nope","entity_key":"#X","lane":"bulk"}');
  let fetched = 0;
  const w = makeWorker({
    sqs: box.sqs,
    queues: box.urls,
    crFetch: async () => {
      fetched += 1;
      return { kind: "http", status: 200, bodyText: "{}" };
    },
    breaker: new CircuitBreaker(),
    gatewayId: GW,
  });
  const r = await w.pollOnce();
  assert.equal(r.handled, false);
  assert.equal(fetched, 0);
  assert.equal(box.queues.results.length, 0);
  assert.equal(
    box.sqs.deleted.length,
    0,
    "not deleted: visibility timeout re-leases it",
  );
});

test("consecutive fetches are paced to the CR minimum interval", async () => {
  const box = fakeSqs();
  box.queues.bulk.push(JOB, JOB, JOB);
  let clock = 1_000_000;
  const sleeps = [];
  const w = makeWorker({
    sqs: box.sqs,
    queues: box.urls,
    crFetch: async () => ({ kind: "http", status: 200, bodyText: "{}" }),
    breaker: new CircuitBreaker(),
    gatewayId: GW,
    now: () => new Date(clock),
    sleep: async (ms) => {
      sleeps.push(ms);
      clock += ms;
    },
  });
  await w.pollOnce(); // first fetch: no wait
  await w.pollOnce(); // immediate second: must pace
  await w.pollOnce();
  assert.equal(sleeps.length, 2, "second and third fetches paced");
  assert.ok(sleeps.every((ms) => ms > 0 && ms <= 1500));
});

test("429: Retry-After holds the pace before the next fetch (sol-6 F3)", async () => {
  const sqsBox = fakeSqs();
  sqsBox.queues.bulk.push(JOB, JOB);
  let clock = Date.parse("2026-09-03T16:00:00Z");
  const sleeps = [];
  let calls = 0;
  const w = makeWorker({
    sqs: sqsBox.sqs,
    queues: sqsBox.urls,
    crFetch: async () => {
      calls += 1;
      return calls === 1
        ? { kind: "http", status: 429, retryAfterSeconds: "7" }
        : { kind: "http", status: 200, bodyText: "{}" };
    },
    breaker: new CircuitBreaker(),
    gatewayId: GW,
    now: () => new Date(clock),
    sleep: async (ms) => {
      sleeps.push(ms);
      clock += ms;
    },
  });
  await w.pollOnce();
  await w.pollOnce();
  assert.equal(calls, 2);
  const held = sleeps.reduce((a, b) => a + b, 0);
  assert.ok(
    held >= 7000,
    `the named cooldown was waited out before fetch two (slept ${held}ms)`,
  );
});
