# Claude Desktop extension (MCPB)

**Off by default.** An official release includes the Claude Desktop extension
only when its release build enables the extension channel, which happens once
Kenaz's notices cover Claude Desktop chat. Claude's directory
no longer accepts `.mcpb` submissions, so the bundles ship by direct download
(and Team/Enterprise admin upload) only.

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
icon.png             icon.png from this directory (512x512 RGBA PNG)
server/kenaz[.exe]   the signed release executable, byte for byte
licenses/            the release licences
```

The manifest also carries `privacy_policies` (the Kenaz privacy policy),
`support` (the same troubleshooting page as the winget listing),
`documentation`, `repository`, `keywords` and `compatibility.claude_desktop`.

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
- **Skills:** Claude Desktop chat loads no skill folders. With
  `KENAZ_MCP_HOST=claude-desktop`, `skills_install` still installs for the
  coding tools on the machine (if any) and also returns the skill's `SKILL.md`
  as `skill_md`, so the chat can follow it.
