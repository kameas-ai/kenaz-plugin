// Kenaz adapter for OpenCode: a server plugin that observes OpenCode
// events and reports metadata-only records to the local Kenaz relay.
//
// OpenCode API surface used (server plugin module, verified against
// OpenCode 1.19.4 and @opencode-ai/plugin 1.19.4):
//   - default export {id, server}; a path plugin must export an id.
//   - server(input, options): options are the second element of the
//     ["<path>", {...}] entry in the `plugin` config array.
//   - hooks.event({event}) for every bus event of the instance directory.
//   - hooks.dispose() when the instance shuts down.
//
// The adapter only observes. It registers no tools, permission hooks,
// argument or message transforms, and never changes what OpenCode does.
// Errors are swallowed: telemetry must never break a session.
//
// Options (rendered by `kenaz setup opencode`):
//   state_dir  the relay state directory holding daemon.json (required)

import { createMapper } from "./map.js";
import { createRelay } from "./relay.js";

export const PLUGIN_ID = "kenaz";

/** Builds the hooks object. deps lets tests inject a relay and clock. */
export function createHooks(options = {}, deps = {}) {
  const mapper = deps.mapper ?? createMapper({ now: deps.now });
  const stateDir = typeof options?.state_dir === "string" && options.state_dir ? options.state_dir : process.env.KENAZ_STATE_DIR || "";
  const relay =
    deps.relay ??
    createRelay({
      stateDir,
      fetch: deps.fetch,
      now: deps.now,
      serviceVersion: () => mapper.version,
    });

  return {
    async event({ event } = {}) {
      try {
        const recs = mapper.map(event);
        if (recs.length > 0) relay.enqueue(recs);
        if (event?.type === "session.idle") void relay.flush();
      } catch {
        // never disturb OpenCode
      }
    },
    async dispose() {
      try {
        relay.enqueue(mapper.end());
        await relay.close(1500);
      } catch {
        // never disturb OpenCode
      }
    },
  };
}

async function server(_input, options) {
  return createHooks(options);
}

export default { id: PLUGIN_ID, server };
