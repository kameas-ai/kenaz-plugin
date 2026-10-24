import assert from "node:assert/strict";
import { readFileSync, writeFileSync } from "node:fs";
import { test } from "node:test";

import { createMapper } from "../src/map.js";
import { toLogsData } from "../src/otlp.js";

const FIXED_NOW = 1792767700000;
const here = new URL("./fixtures/", import.meta.url);

// Recorded OpenCode 1.19.4 event streams (see fixtures/README.md).
export function events(name) {
  return readFileSync(new URL(name + ".jsonl", here), "utf8")
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l));
}

function replay(name, mapper = createMapper({ now: () => FIXED_NOW })) {
  const recs = [];
  for (const e of events(name)) recs.push(...mapper.map(e));
  recs.push(...mapper.end());
  return { recs, mapper };
}

// Every content-bearing value planted in the recordings: prompt, file
// content, assistant text, provider error text and the project path.
const CANARIES = ["CANARYprompt", "CANARYfilecontent", "CANARYresponse", "CANARYerr", "CANARYpathdir"];

function assertNoContent(obj) {
  const s = JSON.stringify(obj);
  for (const c of CANARIES) assert.ok(!s.includes(c), `canary ${c} leaked: ${s}`);
}

test("tool session maps to start, model calls, tool result and end", () => {
  const { recs, mapper } = replay("opencode-1.19.4-tool-session");
  assert.deepEqual(
    recs.map((r) => r.name),
    ["opencode.session.start", "opencode.tool_result", "opencode.model_call", "opencode.model_call", "opencode.session.end"],
  );
  const [, tool, first, second] = recs;
  assert.equal(tool.attrs.tool_name, "read");
  assert.equal(tool.attrs.success, true);
  assert.equal(tool.attrs.duration_ms, 25);
  assert.deepEqual(
    { ...first.attrs, "session.id": undefined, "call.id": undefined },
    {
      "session.id": undefined,
      "call.id": undefined,
      model: "mock-1",
      provider: "mock",
      input_tokens: 1000,
      output_tokens: 30,
      reasoning_tokens: 0,
      cache_read_tokens: 200,
      cache_write_tokens: 0,
      cost_usd: 0.00345,
      duration_ms: first.attrs.duration_ms,
    },
  );
  assert.equal(second.attrs.input_tokens, 1500);
  assert.equal(mapper.version, "1.19.4");
  assertNoContent(recs);
});

test("provider error maps to api_error with status and no zero-token model call", () => {
  const { recs } = replay("opencode-1.19.4-api-error");
  assert.deepEqual(
    recs.map((r) => r.name),
    ["opencode.session.start", "opencode.api_error", "opencode.session.end"],
  );
  assert.equal(recs[1].attrs.status_code, 400);
  assertNoContent(recs);
});

test("repeated and late updates are counted once", () => {
  const mapper = createMapper({ now: () => FIXED_NOW });
  const evs = events("opencode-1.19.4-tool-session");
  const first = evs.flatMap((e) => mapper.map(e));
  const again = evs.flatMap((e) => mapper.map(e));
  assert.equal(first.filter((r) => r.name === "opencode.model_call").length, 2);
  assert.deepEqual(again.map((r) => r.name), ["opencode.session.start"]);
});

test("cost of zero stays unknown", () => {
  const m = createMapper({ now: () => FIXED_NOW });
  const [r] = m.map({
    type: "message.updated",
    properties: {
      info: { role: "assistant", id: "msg_1", sessionID: "ses_1", modelID: "m", providerID: "p", cost: 0, time: { created: 1, completed: 5 }, tokens: { input: 3, output: 4, reasoning: 0, cache: { read: 0, write: 0 } } },
    },
  });
  assert.equal(r.name, "opencode.model_call");
  assert.ok(!("cost_usd" in r.attrs));
});

test("subagent sessions report under their root session", () => {
  const m = createMapper({ now: () => FIXED_NOW });
  m.map({ type: "session.created", properties: { info: { id: "ses_root", time: { created: 1 } } } });
  assert.deepEqual(m.map({ type: "session.created", properties: { info: { id: "ses_child", parentID: "ses_root" } } }), []);
  const [r] = m.map({
    type: "message.part.updated",
    properties: { part: { type: "tool", sessionID: "ses_child", callID: "c1", tool: "grep", state: { status: "error", error: "CANARYerr", time: { start: 1, end: 2 } } } },
  });
  assert.equal(r.attrs["session.id"], "ses_root");
  assert.equal(r.attrs.success, false);
  assertNoContent(r);
  assert.deepEqual(m.map({ type: "session.deleted", properties: { info: { id: "ses_child", parentID: "ses_root" } } }), []);
  assert.deepEqual(
    m.map({ type: "session.deleted", properties: { info: { id: "ses_root" } } }).map((x) => x.name),
    ["opencode.session.end"],
  );
  assert.deepEqual(m.end(), []);
});

test("odd tool names and malformed events are contained", () => {
  const m = createMapper({ now: () => FIXED_NOW });
  const [r] = m.map({
    type: "message.part.updated",
    properties: { part: { type: "tool", sessionID: "s", callID: "c", tool: "../CANARYpathdir x", state: { status: "completed", time: {} } } },
  });
  assert.equal(r.attrs.tool_name, "other");
  for (const bad of [null, 1, "x", {}, { type: "message.updated" }, { type: "message.updated", properties: { info: { role: "assistant" } } }]) {
    assert.deepEqual(m.map(bad), []);
  }
});

// The OTLP exports below are the contract with the relay: the Go decoder
// tests (internal/project) post these exact files to the receiver.
for (const name of ["opencode-1.19.4-tool-session", "opencode-1.19.4-api-error"]) {
  test(`OTLP golden ${name}`, () => {
    const { recs, mapper } = replay(name);
    const got = JSON.stringify(toLogsData(recs, { serviceVersion: mapper.version }), null, 2) + "\n";
    const path = new URL(`otlp-${name}.json`, here);
    if (process.env.UPDATE_GOLDEN) writeFileSync(path, got);
    assert.equal(got, readFileSync(path, "utf8"));
    assert.ok(!got.includes("CANARYprompt") && !got.includes("CANARYpathdir"));
  });
}
