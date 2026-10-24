# Kenaz adapter for OpenCode (preview)

`@kameas/kenaz-opencode` is a small OpenCode server plugin. It observes
OpenCode's session, message and tool events and reports metadata only to
the local `kenaz` relay, which applies the same closed allowlist, consent
and sign-in rules as for the other harnesses before anything reaches
Kameas Fleet. It never changes what OpenCode does: it registers no tools,
permission hooks or message transforms.

It has no runtime dependencies and no build step, so it loads from the
release archive with no package registry access.

## Install

Run `kenaz setup opencode`. It adds to your global `opencode.json`:

```json
{
  "plugin": [["/path/to/kenaz/opencode", {"state_dir": "<kenaz relay state dir>"}]],
  "mcp": {"kenaz": {"type": "local", "command": ["/path/to/kenaz/bin/kenaz", "mcp"], "enabled": true}}
}
```

`kenaz setup --undo opencode` removes both entries again, and
`kenaz setup doctor` checks them.

## What it reports

| Record | Fields |
|---|---|
| `opencode.session.start` / `opencode.session.end` | session id |
| `opencode.model_call` | session id, message id (deduplication only), model, provider, input/output/reasoning/cache tokens, duration, cost when OpenCode reports a positive cost |
| `opencode.tool_result` | session id, call id (deduplication only), tool id, success, duration |
| `opencode.api_error` | session id, HTTP status |

Session and call ids stay on the device: the relay keeps only keyed
hashes. Prompts, responses, tool input and output, file paths, titles and
error messages are never read.

## Compatibility

Checked against OpenCode 1.19.4 (`@opencode-ai/plugin` 1.19.4 server-plugin
interface). `test/fixtures` holds event streams recorded from that release.
Other releases are not yet qualified.

## Tests

`node --test` (Node 18+, no network), or `make test-opencode` from the
repository root.
