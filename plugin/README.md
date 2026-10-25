# Kenaz plugin

*Kenaz, a Kameas AI product.*

Kenaz for Claude Code and Codex. One plugin package, loaded by both harnesses from `.claude-plugin/`.

It needs the `kenaz` binary on your `PATH` and a Kameas Fleet sign-in (`kenaz login`). Install the binary first, from the Kameas downloads page, with your package manager, or as provided by your administrator. The plugin's MCP server and hooks run `kenaz` directly.

`kenaz` is the end-user plugin component. It is not the Kameas Fleet admin CLI (`fleet-admin`), a staff-only tool for operating the Kameas Fleet service. `kenaz` never uses AWS credentials; it signs in only with your own Kameas account.

Memory: your agent can save short notes on this device with the `memory_*` tools and finds the newest titles in a short index at the start of each session. Memory works without signing in. Memories are stored on this device. They are sent to Kameas only if you turn on memory sync for your Kameas Fleet account (off by default), and never as telemetry. When your AI tool uses a memory in a session, it becomes part of that session and is sent to your AI tool's provider (for example Anthropic for Claude Code, or OpenAI for Codex) like anything else in the conversation. Manage it with `kenaz memory` (`list`, `search`, `show`, `update`, `forget`, `export`, `forget-all`, `purge`, `path`, `enable`, `disable`, `sync`); `kenaz logout` keeps it; `kenaz memory purge` deletes the signed-out notebook, and an account's notes are deleted only by an approved `kenaz memory forget-all`.

On Windows, Claude Code and Codex start the MCP server without a shell, so `kenaz` must resolve to `kenaz.exe` on your `PATH` (the Windows installer or release zip). A script launcher such as `kenaz.cmd` is not found there.

Claude and Claude Code are trademarks of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz for Claude Code and Codex is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic or OpenAI.
