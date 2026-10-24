import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import plugin, { createHooks } from "../src/index.js";

function fakeRelay() {
  const sent = [];
  return {
    sent,
    flushes: 0,
    enqueue(recs) {
      sent.push(...recs);
    },
    async flush() {
      this.flushes++;
    },
    async close() {},
  };
}

function events(name) {
  return readFileSync(new URL(`./fixtures/${name}.jsonl`, import.meta.url), "utf8")
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l));
}

test("module shape is an OpenCode path plugin: default {id, server}", async () => {
  assert.equal(plugin.id, "kenaz");
  assert.equal(typeof plugin.server, "function");
  const hooks = await plugin.server({}, { state_dir: "" });
  // Observe-only: no tools, permission, chat or tool-execution hooks.
  assert.deepEqual(Object.keys(hooks).sort(), ["dispose", "event"]);
  await hooks.dispose();
});

test("replayed session reaches the relay and dispose ends it", async () => {
  const relay = fakeRelay();
  const hooks = createHooks({ state_dir: "/state" }, { relay, now: () => 1 });
  for (const e of events("opencode-1.19.4-tool-session")) await hooks.event({ event: e });
  assert.ok(relay.flushes >= 1, "session.idle flushes");
  await hooks.dispose();
  assert.deepEqual(
    relay.sent.map((r) => r.name),
    ["opencode.session.start", "opencode.tool_result", "opencode.model_call", "opencode.model_call", "opencode.session.end"],
  );
});

test("hook failures never propagate to OpenCode", async () => {
  const relay = {
    enqueue() {
      throw new Error("boom");
    },
    flush() {
      throw new Error("boom");
    },
    close() {
      throw new Error("boom");
    },
  };
  const hooks = createHooks({}, { relay });
  for (const e of events("opencode-1.19.4-tool-session")) await hooks.event({ event: e });
  await hooks.event();
  await hooks.dispose();
});
