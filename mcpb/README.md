# Claude Desktop extension (MCPB)

**Not released.** Official releases do not include a Claude Desktop extension
yet: Kenaz's notices cover Claude Code and Codex only, and the extension ships
only once they cover Claude Desktop chat too.

`manifest.json.tmpl` is the [MCP Bundle](https://github.com/modelcontextprotocol/mcpb)
manifest (spec version 0.3) that gives Claude Desktop chat the same MCP tools as
the Claude Code plugin: shared skills, team context, memory and Fleet status.
It runs the bundled `kenaz` executable as `kenaz mcp`. This repository only
holds the template; the official release build renders it and packs it with the
already-signed executable, so nothing is rebuilt.

## Template

The template is valid JSON. Rendering replaces three whole tokens:

| Token | Value |
|---|---|
| `@VERSION@` | the release version, without the leading `v` |
| `@PLATFORM@` | `darwin` or `win32` (Claude Desktop runs only on macOS and Windows) |
| `@EXE@` | `kenaz` or `kenaz.exe` |

Each rendered bundle declares exactly one platform. A bundle is:

```
manifest.json
server/kenaz[.exe]   the signed release executable, byte for byte
licenses/            the release licences
```

`user_config` mirrors the plugin's `userConfig`: `fleet_env` (default `prod`)
and `org_hint`, passed to the server as `KENAZ_ENV` and `KENAZ_ORG_HINT`, like
`plugin/.mcp.json`. Every field has a default, because Claude Desktop passes an
unset field without one through as the literal `${user_config.…}`. The manifest
also sets `KENAZ_MCP_HOST=claude-desktop` (see below).

The description is Kameas's approved Claude Desktop extension text, extended
trademark notice included; it differs from the plugin description in
`plugin/.claude-plugin/plugin.json`, which is unchanged. `license` points at
the official build's licence in `licenses/`. `tools` lists every tool
`kenaz mcp` registers, described by the tool's own title.
`go test ./mcpb ./app` fails if the manifest drifts from either.

## Under Claude Desktop

- **Sign-in** is shared: `kenaz login` writes the same per-user credential store
  (the OS keychain, or the owner-only credentials directory) that the
  extension's server reads, whichever `kenaz` executable ran it. Claude Desktop
  chat has no `/kenaz:login`, so with `KENAZ_MCP_HOST=claude-desktop` a
  signed-out tool call tells the user the terminal command for the executable
  the server runs from (home directory abbreviated). Memory works signed out,
  in the device notebook.
- **Working directory:** Claude Desktop starts extensions in its own directory,
  and chat has no project, so memory's "current project" is the home directory.
- **Relay:** the server starts the shared relay daemon from its own
  executable if none is running, as it does under Claude Code. The daemon runs
  in the Kenaz state directory, not the extension folder, so it never holds
  that folder open (on Windows that would block updating or removing the
  extension).
- **Environment:** Claude Desktop passes only a minimal environment (`HOME`,
  `PATH`, `USER` and similar) plus the manifest's `env`. Nothing in `kenaz mcp`
  depends on `PATH`.
