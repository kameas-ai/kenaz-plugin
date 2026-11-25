# Kenaz plugin

*Kenaz, a Kameas AI product.*

Kenaz for Claude Code and Codex. One plugin package, loaded by both harnesses from `.claude-plugin/`.

It gives your AI tool your organization's shared skills, team context and your own memory from Kameas Fleet, and in Claude Code it can share usage summaries you choose. Its MCP server and hooks run the `kenaz` program directly, so `kenaz` must be on your `PATH`, and you need a Kameas Fleet sign-in (`kenaz login`). Requires a Kameas Fleet account.

### Before you install: the `kenaz` program

The plugin runs the `kenaz` program on your computer, so install that first. One line does it:

- macOS and Linux: `curl -fsSL https://downloads.kameas.ai/kenaz-plugin/install.sh | sh`
- Windows (PowerShell): `irm https://downloads.kameas.ai/kenaz-plugin/install.ps1 | iex`

Then run `kenaz init`, as the installer tells you. Other ways to install are on https://kenaz.kameas.ai/, or your administrator may provide it. You also need an account in a Kameas Fleet organization whose plan includes Kenaz for Claude Code and Codex. Requires a Kameas Fleet account. If your organization's plan doesn't include it, `kenaz login` and `kenaz status` say so, and nothing is collected or held on your device until it does.

### Privacy Policy

Our Privacy Policy is at https://kenaz.kameas.ai/privacy.html, and the full list of what Kenaz can send is at https://kenaz.kameas.ai/telemetry. In short:

- **Where data goes:** to your organization's Kameas Fleet, run by Kameas AI, Inc. Kameas runs Fleet and sign-in with its service providers (sign-in uses ZITADEL). Kenaz sends nothing to any other company for its own purposes, unless you turn on automatic memory capture and set it to use another company's model to write your notes. Session content then goes to that company. See "Your AI tool" below.
- **Signing in and your device:** when you sign in, Kenaz registers this device with your organization's Fleet: an opaque device ID, a display name you control, the operating system, architecture and software version. Before you sign in, nothing is sent.
- **Usage telemetry, only the classes you turn on:** session counts and timing, tool kinds, error categories, model names, token counts and estimated cost, linked to your account and device. Every class is off until you turn it on (`kenaz optin`). It comes from your AI tool's own OpenTelemetry export, which `kenaz setup` points at the `kenaz` program on your computer, not from the plugin's hooks or tools. Prompts, AI responses, tool inputs and outputs, code, file contents and file paths are never sent.
- **Memory:** notes are kept on your device. They are sent to Kameas Fleet, for your account alone, only if you turn on memory sync, which is off until you turn it on and shows its own notice first. Your organization's administrators and Kameas staff have no screen for viewing synced notes. They are not end-to-end encrypted: our systems can read them, and people at Kameas with administrator access to those systems could technically read them.
- **Skills and team context:** searches, and skills or context you open, are sent to your Fleet as requests. Content is sent only when you publish a skill or share a note, and only after you approve the exact content on the local approval page (see [Approving writes](https://github.com/kameas-ai/kenaz-plugin#approving-writes)). There is one exception, which is off unless you turn it on: after you run `kenaz context grant on` in a terminal, your AI tool can share a context file with any team you are in, including your organization's all-members team, without you approving or seeing the text first, up to 30 files per clock hour. Text your AI tool marked as drafted from your memory notes still needs your approval; text it didn't mark can't be recognised. `kenaz context grant off` turns it off. Approving guards against mistakes. It isn't a security boundary against an agent that controls your computer: use your AI tool's own permissions for that.
- **Your AI tool:** anything Kenaz gives your AI tool during a session (a memory note, a skill, team context, or teammates' names where your administrator allows it) becomes part of that session and is handled by the company that makes the tool, like the rest of your conversation. If you turn on automatic memory capture and use another company's model, session content, including tool inputs and outputs, goes to that company under your account or key, and under its terms.

For the usage, memory, skills and context data above, your organization is the controller and Kameas processes it on its behalf. Kameas is the controller for account and sign-in security data, as the Privacy Policy explains. Questions: dpo@kameas.ai.

### What this plugin runs, sends and fetches

This is the complete list of what the Kenaz plugin does on your computer and over the network, so you can check it before you install. The [Privacy Policy](https://kenaz.kameas.ai/privacy.html) and [what Kenaz can send](https://kenaz.kameas.ai/telemetry) have the full detail.

#### The `kenaz` program is installed separately

The plugin package itself contains no executable code: only its manifests, hook and MCP configuration, slash commands and a skill. Every hook and its MCP server run the `kenaz` program from your `PATH`, which you install separately:

- macOS and Linux: `curl -fsSL https://downloads.kameas.ai/kenaz-plugin/install.sh | sh`
- Windows (PowerShell): `irm https://downloads.kameas.ai/kenaz-plugin/install.ps1 | iex`
- Other ways (release archives, package managers) are on https://kenaz.kameas.ai/. Then run `kenaz init`.

Without `kenaz` the hooks and tools fail to start and nothing is sent.

#### Commands it runs

| Registered in | Command | What it does |
|---|---|---|
| `.mcp.json` (Claude Code, Cowork); `.codex-mcp.json` (Codex) | `kenaz mcp` | A local MCP server over stdio. It provides the `memory_*`, `context_*`, `skills_*`, `team_members`, `fleet_status` and `approval_status` tools. Tools that write to Kameas Fleet only prepare a request: nothing is uploaded until you approve the exact content (see [Approving writes](https://github.com/kameas-ai/kenaz-plugin#approving-writes)), except context file shares after you turn on the context share grant yourself in a terminal, and memory notes, which sync to your account without an approval for each once you turn on memory sync. |
| `SessionStart` hook | `kenaz hook session-start` | Starts the local `kenaz` relay (`kenaz daemon`) if it isn't running. Adds to the session: a short Fleet status and notice line when you are signed in, an index of your memory note titles for the current project, and a list of your local context files. In Claude Code it asks for a skills reload when skills from Fleet changed. It sends nothing itself. |
| `SessionStart` hook (startup, clear, compact) | `kenaz hook memory-index` | Adds the memory index (note titles and ids, and the summary of your last turn if automatic capture is on). It reads only the working directory from the hook input. |
| `UserPromptSubmit`, `PostToolUse` (async), `Stop` hooks | `kenaz hook memory-prompt`, `kenaz hook memory-tool`, `kenaz hook memory-stop` | Automatic memory capture. **Off by default:** until you turn capture on, they exit without reading their input. When it is on, they queue the prompt, tool use or end of turn on this device (common credential shapes, email addresses, home directory paths and `<private>` blocks are removed first: a safety net, not a guarantee) and start `kenaz memory capture worker` in the background to write notes from the queue. Claude Code only: Codex has no capture path, and under Codex these hooks always exit at once. |
| `SessionEnd` hook | `kenaz hook session-end` | Tells the local relay that a session ended (a hash of the session id and which AI tool), so it can close that session's usage summary, which is uploaded only if you opted in. |

Each hook has a 1 to 5 second limit and always exits successfully, so it can't block or change your session. No hook makes permission decisions or rewrites tool input. `kenaz` also runs `open` (macOS), `xdg-open` (Linux) or `rundll32` (Windows) to open sign-in and approval pages in your browser, and `claude --version` and `codex --version` to name your AI tools' versions in the telemetry notice it shows you. On macOS it runs `/usr/bin/security` to store sign-in tokens and the queue key in your Keychain. Only when you run `kenaz setup` or `kenaz setup doctor`, it runs `claude plugin …` or `codex plugin …` to add, list or remove this plugin.

#### Where it connects

| Destination | When | What is sent |
|---|---|---|
| Kameas Fleet API: `https://api.fleet.kameas.ai` (`https://api.dev.fleet.kameas.ai` only if you choose the dev environment). Links Kenaz opens in your browser go to the Fleet dashboard, `https://fleet.kameas.ai`. | Only after you sign in | Device registration (an opaque device id, a display name you control, operating system, architecture and Kenaz version). Usage telemetry, only for the classes you opt in to: session counts and timing, tool kinds, error categories, model names, token counts and estimated cost. Skill searches and installs, team context searches and reads, and content you publish or share. Memory notes, only if you turn on memory sync. Checks of your organization's plan, notices and settings. A check, about every 5 minutes while you are signed in, for skills your organization requires. |
| Kameas sign-in: `https://prod-bq6woq.us1.zitadel.cloud` (ZITADEL, Kameas's identity provider, US region), with sign-in pages at `https://login.kameas.ai`. | `kenaz login`, and to refresh your sign-in | A standard OAuth 2.0 sign-in with PKCE in your browser, redirected back to a one-time listener on `127.0.0.1`. Kenaz receives sign-in tokens and keeps them in your OS keychain. Kenaz sends the one-time code, and later its refresh token, to that identity provider. |
| Your AI tool's model provider | Only if you turn on automatic memory capture and choose a writer that uses one | Session content (prompts, tool inputs and outputs, responses) for the notes it writes. The `host-cli` writer runs `claude --safe-mode -p` under your own Claude login. The `api-key` writer runs `claude --bare -p` with your own Anthropic API key, or with your Amazon Bedrock, Google Vertex AI or Microsoft Foundry credentials if you set Claude Code to use them. The content goes to Anthropic or that cloud provider under your terms with it. The `local` writer runs a command you name. The `session-agent` writer asks your current session to write the note. |
| `127.0.0.1` only | While the relay runs | The relay listens on loopback for your AI tool's OpenTelemetry export (port 4318 by default), the MCP server's control calls and the local approval page (`http://localhost:<port>/approvals/`). Nothing on your network can reach it. |

Nothing else. The plugin and `kenaz` have no analytics or crash reporting of their own, send nothing to Kameas before you sign in, and don't check for updates: your AI tool updates the plugin from its marketplace, and you update `kenaz` with its installer or package manager. Prompts, AI responses, tool inputs and outputs, code, file contents and file paths are never sent as telemetry. Usage telemetry comes from your AI tool's own OpenTelemetry export, which `kenaz setup` points at the local relay, not from the plugin's hooks or tools.

#### What it keeps on your computer

Everything is under `~/.kenaz/plugin` (`%USERPROFILE%\.kenaz\plugin` on Windows; `KENAZ_HOME` moves it), readable only by you:

- `config.toml`: your settings.
- `<env>/`: the relay's state, the encrypted queue of telemetry waiting to upload, pending approvals and installed-skill records.
- `memory/`: your memory notes (SQLite files) and, when capture is on, its queue in `memory/capture/queue/`, which `kenaz memory capture disable` deletes.
- `credentials/`: sign-in tokens and the queue key, only on a machine with no OS keychain. Otherwise they are in the macOS Keychain, Windows Credential Manager or the Linux Secret Service.

Skills from Fleet are written to `~/.claude/skills/` (Claude Code) and `~/.agents/skills/` (Codex): ones you install and, while you are signed in, ones your organization's administrator requires, which the relay installs without asking. A required skill keeps its `allowed-tools` and hooks, which can pre-approve tools and run commands in your AI tool, only if an administrator reviewed exactly that version; every other skill from Fleet has them removed. `kenaz logout` removes them. Your personal context files are in `~/.kenaz/contexts/`, and team context files you sync are in `~/.kenaz/team-contexts/`.

#### Turning each part off

- **The whole plugin:** disable or uninstall it in your AI tool (in Claude Code, `/plugin`), or run `kenaz setup --undo claude` (or `codex`). `kenaz logout` signs out and deletes queued uploads.
- **Usage telemetry:** off until you opt in. `kenaz optin --all-off` turns every class off again.
- **Memory:** `kenaz memory disable` turns off the tools and the session index; your notes stay. `kenaz memory purge` deletes the signed-out notebook.
- **Skills from Fleet:** uninstall one you installed with the `skills_uninstall` tool or `/kenaz:skills`; `kenaz logout` removes every skill from Fleet, including ones your organization requires.
- **Automatic memory capture:** off until you turn it on. `kenaz memory capture disable` turns it off; `kenaz memory capture exclude [path]`, or a `.kenaz-no-capture` file in a project, excludes a project.
- **Memory sync:** off until you turn it on. `kenaz memory sync disable` stops it.
- **Context share grant:** off until you turn it on. `kenaz context grant off` turns it off.
- **Context index at session start:** `context_index = "off"` in `config.toml`.

The plugin registers hooks in your AI tool so that automatic memory capture can work. They do nothing until you turn capture on. Your AI tool may ask you to trust them first.

At session start the hooks start the local `kenaz` relay and give your AI tool a short index of your memory note titles from this device (and, if automatic capture is on, the summary of your last turn) and, when you are signed in, a Fleet status line. Signed out, they send nothing to Kameas Fleet, and with no notes on this device they print nothing.

`kenaz` is the end-user plugin component. It is not the Kameas Fleet admin CLI (`fleet-admin`), a staff-only tool for operating the Kameas Fleet service. `kenaz` never uses AWS credentials; it signs in only with your own Kameas account.

Memory: your agent can save short notes on this device with the `memory_*` tools and finds the newest titles in a short index (and, if automatic capture is on, the summary of your last turn) at the start of each session. Memory works without signing in. Memories are stored on this device. They are sent to Kameas only if you turn on memory sync for your Kameas Fleet account (off by default), and never as telemetry. If you turn on automatic memory capture, session content can also go to the model that writes your notes, as its own notice explains. When your AI tool uses a memory in a session, it becomes part of that session and is sent to your AI tool's provider (for example Anthropic for Claude Code, or OpenAI for Codex) like anything else in the conversation. By default chat reads only notes not tied to a project. You can let it read notes from all your projects in the Kenaz extension's settings in the Claude desktop app, or with `kenaz memory recall all`. Those notes can then go to that app's AI provider. Manage it with `kenaz memory` (`list`, `search`, `show`, `update`, `forget`, `export`, `forget-all`, `purge`, `path`, `enable`, `disable`, `sync`); `kenaz logout` keeps it; `kenaz memory purge` deletes the signed-out notebook, and an account's notes are deleted only by an approved `kenaz memory forget-all`.

On Windows, Claude Code and Codex start the MCP server without a shell, so `kenaz` must resolve to `kenaz.exe` on your `PATH` (the Windows installer or release zip). A script launcher such as `kenaz.cmd` is not found there.

Claude and Claude Code are trademarks of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz for Claude Code and Codex is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic or OpenAI.
