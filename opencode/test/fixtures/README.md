# OpenCode event fixtures

`opencode-1.19.4-*.jsonl` are the bus events an OpenCode 1.19.4 server
plugin received through its `event` hook (one JSON event per line),
recorded by running `opencode run` against a local mock OpenAI-compatible
provider:

- `tool-session`: one prompt, a `read` tool call, two model calls, cost
  configured on the mock model.
- `api-error`: the provider answers HTTP 400 until OpenCode gives up.

Content was planted as canaries (`CANARYprompt`, `CANARYfilecontent`,
`CANARYresponse`, `CANARYerr`); the recording machine's directory was
replaced with `/home/user/CANARYpathdir`. Nothing else was edited.

`otlp-*.json` are the adapter's OTLP output for those streams
(`UPDATE_GOLDEN=1 node --test` rewrites them). The Go tests in
`internal/project` post them to the relay receiver and check that no
canary, native id or path reaches the projected Fleet batches.
