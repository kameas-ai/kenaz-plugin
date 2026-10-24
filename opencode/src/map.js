// Maps OpenCode server-plugin events onto the Kenaz relay's metadata-only
// records. This is the only module that knows OpenCode's event shapes.
//
// Event shapes were recorded from OpenCode 1.19.4 (see test/fixtures):
//   session.created / session.deleted  properties.info {id, parentID?, version}
//   message.updated                    properties.info (AssistantMessage:
//                                      id, sessionID, modelID, providerID,
//                                      time {created, completed?}, cost,
//                                      tokens {input, output, reasoning,
//                                      cache {read, write}}, error?)
//   message.part.updated               properties.part (ToolPart: sessionID,
//                                      callID, tool, state {status, time})
//   session.idle                       properties.sessionID
//
// Only the fields named above are read. Prompts, responses, tool input and
// output, titles, paths, diffs and error messages are never read, so they
// cannot reach a record.

const TOOL_RE = /^[A-Za-z0-9_.-]{1,64}$/;
const MODEL_RE = /^[A-Za-z0-9._:/@+-]{1,128}$/;
const PROVIDER_RE = /^[A-Za-z0-9._-]{1,64}$/;
const ID_RE = /^[A-Za-z0-9_.:-]{1,128}$/;
const VERSION_RE = /^[0-9]{1,4}(\.[0-9]{1,6}){1,3}$/;

const MAX_REMEMBERED = 4096;

/** A bounded insertion-ordered set. */
class Recent {
  constructor(max) {
    this.max = max;
    this.items = new Map();
  }
  has(k) {
    return this.items.has(k);
  }
  get(k) {
    return this.items.get(k);
  }
  set(k, v = true) {
    this.items.delete(k);
    this.items.set(k, v);
    while (this.items.size > this.max) {
      this.items.delete(this.items.keys().next().value);
    }
  }
  delete(k) {
    this.items.delete(k);
  }
  keys() {
    return [...this.items.keys()];
  }
}

function id(v) {
  return typeof v === "string" && ID_RE.test(v) ? v : "";
}

function count(v) {
  return Number.isFinite(v) && v > 0 ? Math.floor(v) : 0;
}

function ms(start, end) {
  return Number.isFinite(start) && Number.isFinite(end) && end >= start ? Math.floor(end - start) : 0;
}

/**
 * Creates a mapper. map(event) returns zero or more records
 * {name, time, attrs}; end() returns session-end records for every root
 * session seen and not yet ended (plugin dispose).
 */
export function createMapper({ now = () => Date.now() } = {}) {
  const parents = new Recent(MAX_REMEMBERED); // child session -> root session
  const live = new Recent(MAX_REMEMBERED); // root sessions seen
  const counted = new Recent(MAX_REMEMBERED); // message / call ids already emitted
  let version = "";

  const root = (sid) => {
    let cur = sid;
    for (let i = 0; i < 8 && parents.has(cur); i++) cur = parents.get(cur);
    return cur;
  };

  const rec = (name, sid, time, attrs = {}) => {
    const r = root(sid);
    live.set(r);
    return { name, time: Number.isFinite(time) ? time : now(), attrs: { "session.id": r, ...attrs } };
  };

  function sessionInfo(info, type) {
    const sid = id(info?.id);
    if (!sid) return [];
    if (typeof info.version === "string" && VERSION_RE.test(info.version)) version = info.version;
    const parent = id(info.parentID);
    if (type === "session.created") {
      if (parent) {
        parents.set(sid, parent);
        return [];
      }
      return [rec("opencode.session.start", sid, info.time?.created)];
    }
    // session.deleted: only a root session ends the conversation.
    if (parent || parents.has(sid)) return [];
    if (!live.has(sid)) return [];
    live.delete(sid);
    return [{ name: "opencode.session.end", time: now(), attrs: { "session.id": sid } }];
  }

  function assistant(info) {
    if (info?.role !== "assistant") return [];
    const sid = id(info.sessionID);
    const mid = id(info.id);
    const done = info.time?.completed;
    if (!sid || !mid || !Number.isFinite(done)) return [];
    if (counted.has("m:" + mid)) return [];
    counted.set("m:" + mid);

    const out = [];
    const t = info.tokens ?? {};
    const tokens = count(t.input) + count(t.output) + count(t.reasoning) + count(t.cache?.read) + count(t.cache?.write);
    if (tokens > 0 || !info.error) {
      const attrs = {
        "call.id": mid,
        input_tokens: count(t.input),
        output_tokens: count(t.output),
        reasoning_tokens: count(t.reasoning),
        cache_read_tokens: count(t.cache?.read),
        cache_write_tokens: count(t.cache?.write),
        duration_ms: ms(info.time?.created, done),
      };
      if (typeof info.modelID === "string" && MODEL_RE.test(info.modelID)) attrs.model = info.modelID;
      if (typeof info.providerID === "string" && PROVIDER_RE.test(info.providerID)) attrs.provider = info.providerID;
      // OpenCode reports 0 both for free calls and for models it has no
      // price for, so only a positive cost is passed on as known.
      if (Number.isFinite(info.cost) && info.cost > 0) attrs.cost_usd = info.cost;
      out.push(rec("opencode.model_call", sid, done, attrs));
    }
    const err = info.error;
    if (err && typeof err === "object") {
      if (err.name === "APIError") {
        const sc = err.data?.statusCode;
        out.push(rec("opencode.api_error", sid, done, Number.isInteger(sc) && sc > 0 && sc < 1000 ? { status_code: sc } : {}));
      } else if (err.name === "ProviderAuthError") {
        out.push(rec("opencode.api_error", sid, done, { status_code: 401 }));
      }
      // MessageAbortedError (user interrupt), MessageOutputLengthError and
      // UnknownError are not provider API failures.
    }
    return out;
  }

  function toolPart(part) {
    if (part?.type !== "tool") return [];
    const status = part.state?.status;
    if (status !== "completed" && status !== "error") return [];
    const sid = id(part.sessionID);
    const cid = id(part.callID) || id(part.id);
    if (!sid || !cid || counted.has("t:" + cid)) return [];
    counted.set("t:" + cid);
    const tool = typeof part.tool === "string" && TOOL_RE.test(part.tool) ? part.tool : "other";
    return [
      rec("opencode.tool_result", sid, part.state?.time?.end, {
        "call.id": cid,
        tool_name: tool,
        success: status === "completed",
        duration_ms: ms(part.state?.time?.start, part.state?.time?.end),
      }),
    ];
  }

  return {
    /** The OpenCode version seen on session events, or "". */
    get version() {
      return version;
    },
    map(event) {
      if (!event || typeof event !== "object") return [];
      const p = event.properties ?? {};
      switch (event.type) {
        case "session.created":
        case "session.deleted":
          return sessionInfo(p.info, event.type);
        case "message.updated":
          return assistant(p.info);
        case "message.part.updated":
          return toolPart(p.part);
        default:
          return [];
      }
    },
    end() {
      const out = live.keys().map((sid) => ({ name: "opencode.session.end", time: now(), attrs: { "session.id": sid } }));
      for (const sid of live.keys()) live.delete(sid);
      return out;
    },
  };
}
