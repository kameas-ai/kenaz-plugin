// Delivers adapter records to the local Kenaz relay.
//
// Discovery reuses the relay's own state file: the relay picks its port
// (4318, then 47318, then an ephemeral port) and records it with a random
// nonce in <state_dir>/daemon.json (mode 0600, owner only). Before sending,
// the adapter proves the listener is the relay with the same
// challenge-response the kenaz CLI uses (GET /healthz?challenge=..., answer
// HMAC-SHA256(nonce, challenge)), so a foreign OTLP collector on 4318 never
// receives anything. The proof is redone at the start of every drain: the
// relay exits when idle, and another program may then own its port.
//
// The relay owns consent, sign-in, queueing and forwarding to Kameas Fleet.
// While nobody is signed in it answers 503 and the batch is dropped here.
// The queue is bounded and nothing is written to disk.

import { createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join } from "node:path";

import { toLogsData } from "./otlp.js";

export const MAX_QUEUE = 1000;
export const MAX_BATCH = 200;
export const MAX_ATTEMPTS = 3;
const PROBE_TIMEOUT_MS = 750;
const SEND_TIMEOUT_MS = 3000;
const REDISCOVER_MS = 15000;

function proof(nonce, challenge) {
  return createHmac("sha256", nonce).update(challenge).digest("hex");
}

function sameHex(a, b) {
  if (typeof a !== "string" || a.length !== b.length) return false;
  return timingSafeEqual(Buffer.from(a), Buffer.from(b));
}

async function withTimeout(fetchFn, url, init, ms) {
  const ctl = new AbortController();
  const t = setTimeout(() => ctl.abort(), ms);
  try {
    return await fetchFn(url, { ...init, signal: ctl.signal });
  } finally {
    clearTimeout(t);
  }
}

/**
 * createRelay({stateDir}) returns {enqueue(records), flush(), close(ms), stats}.
 * fetch, read, now and setTimer are injectable for tests.
 */
export function createRelay({
  stateDir,
  fetch: fetchFn = globalThis.fetch,
  read = (p) => readFile(p, "utf8"),
  now = () => Date.now(),
  serviceVersion = () => "",
  flushDelayMs = 1000,
} = {}) {
  const queue = [];
  const stats = { sent: 0, dropped_full: 0, dropped_unavailable: 0, dropped_rejected: 0, dropped_no_relay: 0 };
  let endpoint = null; // {base}, verified for the current drain only
  let lastFailure = -Infinity;
  let timer = null;
  let inflight = null;

  async function discover() {
    if (endpoint) return endpoint;
    if (!stateDir || now() - lastFailure < REDISCOVER_MS) return null;
    const fail = () => {
      lastFailure = now();
      return null;
    };
    try {
      const st = JSON.parse(await read(join(stateDir, "daemon.json")));
      const port = st?.port;
      if (!Number.isInteger(port) || port < 1 || port > 65535 || typeof st.nonce !== "string" || !st.nonce) return fail();
      const base = `http://127.0.0.1:${port}`;
      const challenge = randomBytes(32).toString("hex");
      const res = await withTimeout(fetchFn, `${base}/healthz?challenge=${challenge}`, { method: "GET" }, PROBE_TIMEOUT_MS);
      if (!res.ok) return fail();
      const body = await res.json();
      if (!sameHex(body?.proof, proof(st.nonce, challenge))) return fail();
      endpoint = { base };
      return endpoint;
    } catch {
      return fail();
    }
  }

  async function post(batch) {
    const ep = await discover();
    if (!ep) return "unavailable";
    try {
      const res = await withTimeout(
        fetchFn,
        `${ep.base}/v1/logs`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify(toLogsData(batch, { serviceVersion: serviceVersion() })),
        },
        SEND_TIMEOUT_MS,
      );
      if (res.ok) return "ok";
      if (res.status === 503) return "not_signed_in";
      if (res.status >= 400 && res.status < 500) return "rejected";
      endpoint = null;
      return "retry";
    } catch {
      endpoint = null; // relay restarted or moved: rediscover next time
      return "retry";
    }
  }

  async function drain() {
    // Never reuse a proof from an earlier drain: re-read daemon.json and
    // redo the challenge before anything is sent.
    endpoint = null;
    while (queue.length > 0) {
      const batch = queue.slice(0, MAX_BATCH);
      let outcome = "retry";
      for (let attempt = 0; attempt < MAX_ATTEMPTS && outcome === "retry"; attempt++) {
        outcome = await post(batch.map((e) => e.rec));
      }
      if (outcome === "unavailable") {
        // No relay yet (it starts with the kenaz MCP server). Keep the
        // records, bounded by MAX_QUEUE, and try again later.
        if (!stateDir) {
          stats.dropped_no_relay += queue.length;
          queue.length = 0;
        }
        return;
      }
      queue.splice(0, batch.length);
      if (outcome === "ok") stats.sent += batch.length;
      else if (outcome === "not_signed_in") stats.dropped_unavailable += batch.length;
      else stats.dropped_rejected += batch.length;
    }
  }

  function flush() {
    if (timer) {
      clearTimeout(timer);
      timer = null;
    }
    if (!inflight) {
      inflight = drain().finally(() => {
        inflight = null;
        if (queue.length > 0) schedule(REDISCOVER_MS);
      });
    }
    return inflight;
  }

  function schedule(delay = flushDelayMs) {
    if (timer) return;
    timer = setTimeout(() => {
      timer = null;
      flush();
    }, delay);
    timer.unref?.();
  }

  return {
    stats,
    enqueue(records) {
      for (const rec of records) {
        queue.push({ rec });
        if (queue.length > MAX_QUEUE) {
          queue.shift();
          stats.dropped_full++;
        }
      }
      if (records.length > 0) schedule();
    },
    flush,
    /** close flushes what it can within ms and stops the timer. */
    async close(ms = 1500) {
      lastFailure = -Infinity;
      await Promise.race([flush(), new Promise((r) => setTimeout(r, ms).unref?.())]);
      if (timer) clearTimeout(timer);
      timer = null;
    },
  };
}
