# Kenaz plugin

*Kenaz, a Kameas AI product.*

Kenaz for Claude Code and Codex. One plugin package, loaded by both harnesses from `.claude-plugin/`.

It gives your AI tool your organization's shared skills, team context and a private memory from Kameas Fleet, and in Claude Code it can share usage summaries you choose. Its MCP server and hooks run the `kenaz` program directly, so `kenaz` must be on your `PATH`, and you need a Kameas Fleet sign-in (`kenaz login`).

### Before you install: the `kenaz` program

The plugin runs the `kenaz` program on your computer, so install that first. One line does it:

- macOS and Linux: `curl -fsSL https://downloads.kameas.ai/kenaz-plugin/install.sh | sh`
- Windows (PowerShell): `irm https://downloads.kameas.ai/kenaz-plugin/install.ps1 | iex`

Then run `kenaz init`, as the installer tells you. Other ways to install are on https://kenaz.kameas.ai/, or your administrator may provide it. You also need an account in a Kameas Fleet organization that has joined the Kenaz early-access program.

### Privacy Policy

Our Privacy Policy is at https://kenaz.kameas.ai/privacy.html, and the full list of what Kenaz can send is at https://kenaz.kameas.ai/telemetry. In short:

- **Where data goes:** only to your organization's Kameas Fleet, run by Kameas AI, Inc. Kenaz sends nothing to any other company (see "Your AI tool" below). The `kenaz` program sends nothing to the companies that make your AI tools.
- **Signing in and your device:** when you sign in, Kenaz registers this device with your organization's Fleet: an opaque device ID, a display name you control, the operating system, architecture and software version. Before you sign in, nothing is sent.
- **Usage telemetry, only the classes you turn on:** session counts and timing, tool kinds, error categories, model names, token counts and estimated cost, linked to your account and device. Every class is off until you turn it on (`kenaz optin`). It comes from your AI tool's own OpenTelemetry export, which `kenaz setup` points at the `kenaz` program on your computer, not from the plugin's hooks or tools. Prompts, AI responses, tool inputs and outputs, code, file contents and file paths are never sent.
- **Memory:** notes are kept on your device. They are sent to Kameas Fleet, for your account alone, only if you turn on memory sync, which is off until you turn it on and shows its own notice first. Your organization's administrators and Kameas staff have no view of synced notes.
- **Skills and team context:** searches, and skills or context you open, are sent to your Fleet as requests. Content is sent only when you publish a skill or share a note, and only after you approve the exact content on the local approval page (see [Approving writes](https://github.com/kameas-ai/kenaz-plugin#approving-writes)).
- **Your AI tool:** anything Kenaz gives your AI tool during a session (a memory note, a skill, team context, or teammates' names where your administrator allows it) becomes part of that session and is handled by the company that makes the tool, like the rest of your conversation.

For the usage, memory, skills and context data above, your organization is the controller and Kameas processes it on its behalf. Kameas is the controller for account and sign-in security data, as the Privacy Policy explains. Questions: dpo@kameas.ai.

At session start the hooks start the local `kenaz` relay and give your AI tool a short index of your memory note titles from this device and, when you are signed in, a Fleet status line. Signed out, they send nothing to Kameas Fleet, and with no notes on this device they print nothing.

`kenaz` is the end-user plugin component. It is not the Kameas Fleet admin CLI (`fleet-admin`), a staff-only tool for operating the Kameas Fleet service. `kenaz` never uses AWS credentials; it signs in only with your own Kameas account.

Memory: your agent can save short notes on this device with the `memory_*` tools and finds the newest titles in a short index at the start of each session. Memory works without signing in. Memories are stored on this device. They are sent to Kameas only if you turn on memory sync for your Kameas Fleet account (off by default), and never as telemetry. When your AI tool uses a memory in a session, it becomes part of that session and is sent to your AI tool's provider (for example Anthropic for Claude Code, or OpenAI for Codex) like anything else in the conversation. Manage it with `kenaz memory` (`list`, `search`, `show`, `update`, `forget`, `export`, `forget-all`, `purge`, `path`, `enable`, `disable`, `sync`); `kenaz logout` keeps it; `kenaz memory purge` deletes the signed-out notebook, and an account's notes are deleted only by an approved `kenaz memory forget-all`.

On Windows, Claude Code and Codex start the MCP server without a shell, so `kenaz` must resolve to `kenaz.exe` on your `PATH` (the Windows installer or release zip). A script launcher such as `kenaz.cmd` is not found there.

Claude and Claude Code are trademarks of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz for Claude Code and Codex is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic or OpenAI.
