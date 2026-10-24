import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { test } from "node:test";

import { createRelay, MAX_QUEUE } from "../src/relay.js";

const NONCE = "a".repeat(64);
const DAEMON = JSON.stringify({ pid: 1, port: 47318, nonce: NONCE, version: "0.3.1" });

function rec(i = 0) {
  return { name: "opencode.tool_result", time: 1792767649366, attrs: { "session.id": "s", "call.id": "c" + i, tool_name: "read", success: true, duration_ms: 1 } };
}

// fakeFetch emulates the relay: /healthz answers the HMAC proof (or a
// forged one), /v1/logs answers with the queued statuses.
function fakeFetch({ forge = false, statuses = [200] } = {}) {
  const calls = [];
  const fn = async (url, init = {}) => {
    const u = new URL(url);
    calls.push({ path: u.pathname, method: init.method, body: init.body });
    if (u.pathname === "/healthz") {
      const challenge = u.searchParams.get("challenge");
      const key = forge ? "wrong" : NONCE;
      return { ok: true, status: 200, json: async () => ({ version: "0.3.1", proof: createHmac("sha256", key).update(challenge).digest("hex") }) };
    }
    const status = statuses.length > 1 ? statuses.shift() : statuses[0];
    if (status === "throw") throw new Error("ECONNREFUSED");
    return { ok: status >= 200 && status < 300, status, json: async () => ({}) };
  };
  fn.calls = calls;
  fn.posts = () => calls.filter((c) => c.path === "/v1/logs");
  return fn;
}

function relay(fetch, opts = {}) {
  let t = 1_000_000;
  return createRelay({ stateDir: "/state", fetch, read: async () => DAEMON, now: () => (t += 20_000), flushDelayMs: 1, ...opts });
}

test("sends to the verified relay as OTLP JSON", async () => {
  const f = fakeFetch();
  const r = relay(f);
  r.enqueue([rec(1), rec(2)]);
  await r.flush();
  const posts = f.posts();
  assert.equal(posts.length, 1);
  const body = JSON.parse(posts[0].body);
  assert.equal(body.resourceLogs[0].scopeLogs[0].logRecords.length, 2);
  assert.equal(r.stats.sent, 2);
});

test("never sends to a listener that cannot prove the nonce", async () => {
  const f = fakeFetch({ forge: true });
  const r = relay(f);
  r.enqueue([rec()]);
  await r.flush();
  assert.equal(f.posts().length, 0);
  assert.equal(r.stats.sent, 0);
});

test("no daemon.json keeps records queued and sends nothing", async () => {
  const f = fakeFetch();
  const r = relay(f, { read: async () => { throw new Error("ENOENT"); } });
  r.enqueue([rec()]);
  await r.flush();
  assert.equal(f.calls.length, 0);
});

test("signed out (503) drops the batch on the device", async () => {
  const f = fakeFetch({ statuses: [503] });
  const r = relay(f);
  r.enqueue([rec()]);
  await r.flush();
  assert.equal(r.stats.dropped_unavailable, 1);
  await r.flush();
  assert.equal(f.posts().length, 1);
});

test("a transient failure is retried, then delivered once", async () => {
  const f = fakeFetch({ statuses: ["throw", 200] });
  const r = relay(f);
  r.enqueue([rec()]);
  await r.flush();
  assert.equal(r.stats.sent, 1);
  assert.equal(f.posts().length, 2);
});

test("the queue is bounded", () => {
  const r = relay(fakeFetch(), { flushDelayMs: 60_000 });
  const many = Array.from({ length: MAX_QUEUE + 5 }, (_, i) => rec(i));
  r.enqueue(many);
  assert.equal(r.stats.dropped_full, 5);
  return r.close(10);
});

test("no state dir configured drops instead of buffering forever", async () => {
  const f = fakeFetch();
  const r = createRelay({ stateDir: "", fetch: f, read: async () => DAEMON });
  r.enqueue([rec()]);
  await r.flush();
  assert.equal(f.calls.length, 0);
  assert.equal(r.stats.dropped_no_relay, 1);
});

// The relay exits when idle; another OTLP collector may then bind its port.
// Each drain re-proves the listener, so nothing reaches the newcomer.
test("relay exits and a foreign listener takes the port: nothing is sent", async () => {
  const http = await import("node:http");
  const { mkdtempSync, writeFileSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const { join } = await import("node:path");

  const posts = { relay: 0, foreign: 0 };
  const serve = (who, nonce, port = 0) =>
    new Promise((resolve) => {
      const srv = http.createServer((req, res) => {
        res.setHeader("connection", "close"); // no stale keep-alive socket masks a leak
        const u = new URL(req.url, "http://x");
        req.resume();
        req.on("end", () => {
          if (u.pathname === "/healthz") {
            // The foreign collector answers, but cannot know the nonce.
            const key = nonce ?? "guess";
            res.writeHead(200, { "content-type": "application/json" });
            res.end(JSON.stringify({ proof: createHmac("sha256", key).update(u.searchParams.get("challenge") ?? "").digest("hex") }));
            return;
          }
          posts[who]++;
          res.writeHead(200, { "content-type": "application/json" });
          res.end("{}");
        });
      });
      srv.listen(port, "127.0.0.1", () => resolve(srv));
    });

  const dir = mkdtempSync(join(tmpdir(), "kenaz-relay-"));
  const relaySrv = await serve("relay", NONCE);
  const port = relaySrv.address().port;
  writeFileSync(join(dir, "daemon.json"), JSON.stringify({ pid: 1, port, nonce: NONCE }));

  const r = createRelay({ stateDir: dir, flushDelayMs: 60_000 });
  r.enqueue([rec(1)]);
  await r.flush();
  assert.equal(posts.relay, 1);

  // Relay idle-exits (daemon.json left behind); a collector binds the port.
  await new Promise((res) => relaySrv.close(res));
  const foreign = await serve("foreign", null, port);
  try {
    r.enqueue([rec(2)]);
    await r.flush();
    r.enqueue([rec(3)]);
    await r.flush();
    assert.equal(posts.foreign, 0, "records reached an unverified listener");
    assert.equal(r.stats.sent, 1);
  } finally {
    await new Promise((res) => foreign.close(res));
    await r.close(10);
  }
});
