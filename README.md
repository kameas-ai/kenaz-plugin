# Kenaz for Claude Code and Codex

*Kenaz, a Kameas AI product.*

Kenaz for Claude Code and Codex connects your Claude Code and Codex sessions to your organization's Kameas Fleet. You can:

- **Sign in** with your Kameas account, and register this device under a display name you control.
- **Report session metadata** to your organization's Fleet analytics: session counts and durations, tool kinds (built-in tools by name; tools and servers you added are reported only as "mcp" or "other"), latency and success, error categories, model names, token usage, and cost when the harness itself reports it (Kenaz does not estimate prices). These reports never include prompts, responses, code, file paths or tool output. Nothing is reported until you sign in yourself and opt in.
- **Use your organization's skills**, and share the skills you write.
- **Search and share team context.**
- **Remember things across sessions** with memory stored on your device: your AI tool can save short notes (facts, decisions, conventions, fixes) and find them again later, with optional sync to your own Kameas Fleet account. See [Memory](#memory).

It never approves, blocks or rewrites what your agent does.

What Kenaz for Claude Code and Codex reports about your sessions is decided by open-source code you can read. The part that connects to Kameas Fleet (sign-in and transport) is proprietary.

Release binaries are official builds that include a proprietary module and are licensed under the Kameas Client Software License. The source in this repository is Apache-2.0 and can be built without that module, but such builds cannot connect to Kameas Fleet.

## Before you install: the `kenaz` program

The plugin registers hooks in your AI tool so that automatic memory capture can work. They do nothing until you turn capture on. Your AI tool may ask you to trust them first.

The plugin runs the `kenaz` program on your computer, so install that first. One line does it:

- macOS and Linux: `curl -fsSL https://downloads.kameas.ai/kenaz-plugin/install.sh | sh`
- Windows (PowerShell): `irm https://downloads.kameas.ai/kenaz-plugin/install.ps1 | iex`

Then run `kenaz init`, as the installer tells you. Other ways to install are on https://kenaz.kameas.ai/. You also need an account in a Kameas Fleet organization whose plan includes Kenaz for Claude Code and Codex. Requires a Kameas Fleet account. If your organization's plan doesn't include it, `kenaz login` and `kenaz status` say so, and nothing is collected or held on your device until it does.

## Privacy Policy

Our Privacy Policy is at https://kenaz.kameas.ai/privacy.html, and the full list of what Kenaz can send is at https://kenaz.kameas.ai/telemetry. In short:

- **Where data goes:** to your organization's Kameas Fleet, run by Kameas AI, Inc. Kameas runs Fleet and sign-in with its service providers (sign-in uses ZITADEL). Kenaz sends nothing to any other company for its own purposes, unless you turn on automatic memory capture and set it to use another company's model to write your notes. Session content then goes to that company. See "Your AI tool" below.
- **Signing in and your device:** when you sign in, Kenaz registers this device with your organization's Fleet: an opaque device ID, a display name you control, the operating system, architecture and software version. Before you sign in, nothing is sent.
- **Usage telemetry, only the classes you turn on:** session counts and timing, tool kinds, error categories, model names, token counts and estimated cost, linked to your account and device. Every class is off until you turn it on (`kenaz optin`). It comes from your AI tool's own OpenTelemetry export, which `kenaz setup` points at the `kenaz` program on your computer, not from the plugin's hooks or tools. Prompts, AI responses, tool inputs and outputs, code, file contents and file paths are never sent.
- **Memory:** notes are kept on your device. They are sent to Kameas Fleet, for your account alone, only if you turn on memory sync, which is off until you turn it on and shows its own notice first. Your organization's administrators and Kameas staff have no screen for viewing synced notes. They are not end-to-end encrypted: our systems can read them, and people at Kameas with administrator access to those systems could technically read them.
- **Skills and team context:** searches, and skills or context you open, are sent to your Fleet as requests. Content is sent only when you publish a skill or share a note, and only after you approve the exact content on the local approval page (see [Approving writes](#approving-writes)). The one exception is a context file your AI tool shares after you have turned on the context share grant, which is off unless you turn it on (see [Sharing context files without an approval for each](#sharing-context-files-without-an-approval-for-each)).
- **Your AI tool:** anything Kenaz gives your AI tool during a session (a memory note, a skill, team context, or teammates' names where your administrator allows it) becomes part of that session and is handled by the company that makes the tool, like the rest of your conversation. If you turn on automatic memory capture and use another company's model, session content, including tool inputs and outputs, goes to that company under your account or key, and under its terms.

For the usage, memory, skills and context data above, your organization is the controller and Kameas processes it on its behalf. Kameas is the controller for account and sign-in security data, as the Privacy Policy explains. Questions: dpo@kameas.ai.

## What this plugin runs, sends and fetches

This is the complete list of what the Kenaz plugin does on your computer and over the network, so you can check it before you install. The [Privacy Policy](https://kenaz.kameas.ai/privacy.html) and [what Kenaz can send](https://kenaz.kameas.ai/telemetry) have the full detail.

### The `kenaz` program is installed separately

The plugin package itself contains no executable code: only its manifests, hook and MCP configuration, slash commands and a skill. Every hook and its MCP server run the `kenaz` program from your `PATH`, which you install separately:

- macOS and Linux: `curl -fsSL https://downloads.kameas.ai/kenaz-plugin/install.sh | sh`
- Windows (PowerShell): `irm https://downloads.kameas.ai/kenaz-plugin/install.ps1 | iex`
- Other ways (release archives, package managers) are on https://kenaz.kameas.ai/. Then run `kenaz init`.

Without `kenaz` the hooks and tools fail to start and nothing is sent.

### Commands it runs

| Registered in | Command | What it does |
|---|---|---|
| `.mcp.json` (Claude Code, Cowork); `.codex-mcp.json` (Codex) | `kenaz mcp` | A local MCP server over stdio. It provides the `memory_*`, `context_*`, `skills_*`, `team_members`, `fleet_status` and `approval_status` tools. Tools that write to Kameas Fleet only prepare a request: nothing is uploaded until you approve the exact content (see [Approving writes](https://github.com/kameas-ai/kenaz-plugin#approving-writes)), except context file shares after you turn on the context share grant yourself in a terminal, and memory notes, which sync to your account without an approval for each once you turn on memory sync. |
| `SessionStart` hook | `kenaz hook session-start` | Starts the local `kenaz` relay (`kenaz daemon`) if it isn't running. Adds to the session: a short Fleet status and notice line when you are signed in, an index of your memory note titles for the current project, and a list of your local context files. In Claude Code it asks for a skills reload when skills from Fleet changed. It sends nothing itself. |
| `SessionStart` hook (startup, clear, compact) | `kenaz hook memory-index` | Adds the memory index (note titles and ids, and the summary of your last turn if automatic capture is on). It reads only the working directory from the hook input. |
| `UserPromptSubmit`, `PostToolUse` (async), `Stop` hooks | `kenaz hook memory-prompt`, `kenaz hook memory-tool`, `kenaz hook memory-stop` | Automatic memory capture. **Off by default:** until you turn capture on, they exit without reading their input. When it is on, they queue the prompt, tool use or end of turn on this device (common credential shapes, email addresses, home directory paths and `<private>` blocks are removed first: a safety net, not a guarantee) and start `kenaz memory capture worker` in the background to write notes from the queue. Claude Code only: Codex has no capture path, and under Codex these hooks always exit at once. |
| `SessionEnd` hook | `kenaz hook session-end` | Tells the local relay that a session ended (a hash of the session id and which AI tool), so it can close that session's usage summary, which is uploaded only if you opted in. |

Each hook has a 1 to 5 second limit and always exits successfully, so it can't block or change your session. No hook makes permission decisions or rewrites tool input. `kenaz` also runs `open` (macOS), `xdg-open` (Linux) or `rundll32` (Windows) to open sign-in and approval pages in your browser, and `claude --version` and `codex --version` to name your AI tools' versions in the telemetry notice it shows you. On macOS it runs `/usr/bin/security` to store sign-in tokens and the queue key in your Keychain. Only when you run `kenaz setup` or `kenaz setup doctor`, it runs `claude plugin …` or `codex plugin …` to add, list or remove this plugin.

### Where it connects

| Destination | When | What is sent |
|---|---|---|
| Kameas Fleet API: `https://api.fleet.kameas.ai` (`https://api.dev.fleet.kameas.ai` only if you choose the dev environment). Links Kenaz opens in your browser go to the Fleet dashboard, `https://fleet.kameas.ai`. | Only after you sign in | Device registration (an opaque device id, a display name you control, operating system, architecture and Kenaz version). Usage telemetry, only for the classes you opt in to: session counts and timing, tool kinds, error categories, model names, token counts and estimated cost. Skill searches and installs, team context searches and reads, and content you publish or share. Memory notes, only if you turn on memory sync. Checks of your organization's plan, notices and settings. A check, about every 5 minutes while you are signed in, for skills your organization requires. |
| Kameas sign-in: `https://prod-bq6woq.us1.zitadel.cloud` (ZITADEL, Kameas's identity provider, US region), with sign-in pages at `https://login.kameas.ai`. | `kenaz login`, and to refresh your sign-in | A standard OAuth 2.0 sign-in with PKCE in your browser, redirected back to a one-time listener on `127.0.0.1`. Kenaz receives sign-in tokens and keeps them in your OS keychain. Kenaz sends the one-time code, and later its refresh token, to that identity provider. |
| Your AI tool's model provider | Only if you turn on automatic memory capture and choose a writer that uses one | Session content (prompts, tool inputs and outputs, responses) for the notes it writes. The `host-cli` writer runs `claude --safe-mode -p` under your own Claude login. The `api-key` writer runs `claude --bare -p` with your own Anthropic API key, or with your Amazon Bedrock, Google Vertex AI or Microsoft Foundry credentials if you set Claude Code to use them. The content goes to Anthropic or that cloud provider under your terms with it. The `local` writer runs a command you name. The `session-agent` writer asks your current session to write the note. |
| `127.0.0.1` only | While the relay runs | The relay listens on loopback for your AI tool's OpenTelemetry export (port 4318 by default), the MCP server's control calls and the local approval page (`http://localhost:<port>/approvals/`). Nothing on your network can reach it. |

Nothing else. The plugin and `kenaz` have no analytics or crash reporting of their own, send nothing to Kameas before you sign in, and don't check for updates: your AI tool updates the plugin from its marketplace, and you update `kenaz` with its installer or package manager. Prompts, AI responses, tool inputs and outputs, code, file contents and file paths are never sent as telemetry. Usage telemetry comes from your AI tool's own OpenTelemetry export, which `kenaz setup` points at the local relay, not from the plugin's hooks or tools.

### What it keeps on your computer

Everything is under `~/.kenaz/plugin` (`%USERPROFILE%\.kenaz\plugin` on Windows; `KENAZ_HOME` moves it), readable only by you:

- `config.toml`: your settings.
- `<env>/`: the relay's state, the encrypted queue of telemetry waiting to upload, pending approvals and installed-skill records.
- `memory/`: your memory notes (SQLite files) and, when capture is on, its queue in `memory/capture/queue/`, which `kenaz memory capture disable` deletes.
- `credentials/`: sign-in tokens and the queue key, only on a machine with no OS keychain. Otherwise they are in the macOS Keychain, Windows Credential Manager or the Linux Secret Service.

Skills from Fleet are written to `~/.claude/skills/` (Claude Code) and `~/.agents/skills/` (Codex): ones you install and, while you are signed in, ones your organization's administrator requires, which the relay installs without asking. A required skill keeps its `allowed-tools` and hooks, which can pre-approve tools and run commands in your AI tool, only if an administrator reviewed exactly that version; every other skill from Fleet has them removed. `kenaz logout` removes them. Your personal context files are in `~/.kenaz/contexts/`, and team context files you sync are in `~/.kenaz/team-contexts/`.

### Turning each part off

- **The whole plugin:** disable or uninstall it in your AI tool (in Claude Code, `/plugin`), or run `kenaz setup --undo claude` (or `codex`). `kenaz logout` signs out and deletes queued uploads.
- **Usage telemetry:** off until you opt in. `kenaz optin --all-off` turns every class off again.
- **Memory:** `kenaz memory disable` turns off the tools and the session index; your notes stay. `kenaz memory purge` deletes the signed-out notebook.
- **Skills from Fleet:** uninstall one you installed with the `skills_uninstall` tool or `/kenaz:skills`; `kenaz logout` removes every skill from Fleet, including ones your organization requires.
- **Automatic memory capture:** off until you turn it on. `kenaz memory capture disable` turns it off; `kenaz memory capture exclude [path]`, or a `.kenaz-no-capture` file in a project, excludes a project.
- **Memory sync:** off until you turn it on. `kenaz memory sync disable` stops it.
- **Context share grant:** off until you turn it on. `kenaz context grant off` turns it off.
- **Context index at session start:** `context_index = "off"` in `config.toml`.

## Not the Kameas Fleet admin CLI

`kenaz` is the end-user component of Kenaz for Claude Code and Codex: the plugin and the local program it runs. It is not the Kameas Fleet admin CLI (`fleet-admin`), which is a staff-only tool for operating the Kameas Fleet service. `kenaz` never uses AWS credentials. It signs in only with your own Kameas account (`kenaz login`) and talks only to your organization's Kameas Fleet.

## How it works

Each harness exports its own OpenTelemetry to a receiver on `127.0.0.1` inside the local `kenaz` process. The projector (`internal/project`) maps that telemetry onto a closed allowlist (`fleetapi/allowlist`) and drops everything else on the device. It then hands the result to the Fleet connection behind the `fleetapi` interfaces.

- `plugin/` is the plugin package. It's in Claude Code plugin format, which Codex also loads.
- `opencode/` is the OpenCode adapter (preview, see [OpenCode](#opencode-preview)).
- `.claude-plugin/marketplace.json` is the `kenaz` marketplace (install id `kenaz@kenaz`).
- `cmd/kenaz` is the public build. It uses `fleetapi/stub` and cannot connect to Kameas Fleet.

See [ARCHITECTURE.md](ARCHITECTURE.md).

## Getting started: `kenaz init`

After you unpack a release archive (or run its `install.sh`), run its `kenaz` by its full path once:

```
"<archive>/bin/kenaz" init
```

`kenaz init` sets everything up in one go, asking before every change:

1. **PATH.** If `kenaz` is not on your `PATH` yet, it shows the change and asks `Add <dir> to PATH in <profile>? [y/N]`. On macOS and Linux it adds one marked block (`# >>> kenaz PATH >>>`) to your login shell's profile: `~/.zshrc` (or `$ZDOTDIR/.zshrc`); for bash on macOS the first of `~/.bash_profile`, `~/.bash_login` and `~/.profile` that exists (else a new `~/.bash_profile`), and `~/.bashrc` on Linux; or `~/.config/fish/conf.d/kenaz.fish`. Running it again changes nothing, and a new install directory replaces the block. The profile as it was before kenaz first changed it is kept as `<profile>.kenaz-backup`. An existing profile keeps its permissions (a new one gets the usual ones for a new file, following your umask); it is replaced by a new file, so extended attributes, ACLs and other hard links to it are not carried over. A profile that is a symbolic link (dotfiles managers) is edited where the link points, and the link is kept. A profile that is read-only, a broken link or a link loop, or has a kenaz block with no end marker is left alone, and init prints the line to add yourself. So is a profile you edit while the question is open: init writes nothing, and you can run it again. On Windows it adds the folder to the front of your user `Path` (`HKCU\Environment`) unless it is already there, also when written with variables such as `%LOCALAPPDATA%` (in an expandable `Path` value; in a plain string value Windows does not expand them, and neither does init). If the user `Path` changes while the question is open, init writes nothing, and you can run it again. For an installer-managed install it adds `install/current/bin`, so upgrades keep working. With `--no-path` it changes nothing and prints the line to add. Open a new terminal afterwards, and quit and reopen Claude Desktop and ChatGPT so they see it.
2. **Sign-in.** If you are not signed in, it runs `kenaz login`, which also starts the Kenaz relay.
3. **Apps.** It looks for every app Kenaz can serve and lists what it found:

   | App | How Kenaz connects |
   |---|---|
   | Claude Code (CLI) | `kenaz setup claude` |
   | Claude Desktop, Code tab | Covered by `kenaz setup claude`: local Code tab sessions run Claude Code and read the same `~/.claude/settings.json` (its `env`, `enabledPlugins` and `extraKnownMarketplaces`). SSH sessions read the remote machine's settings instead. |
   | Codex CLI | `kenaz setup codex` |
   | ChatGPT desktop app (Codex) and the Codex desktop app | Covered by `kenaz setup codex`: they run Codex with the same `~/.codex/config.toml`, plugins and marketplaces as the CLI. |

4. **Setup.** It runs `kenaz setup claude` and `kenaz setup codex` for the apps it found (Claude Desktop counts for Claude, and the ChatGPT or Codex desktop app for Codex). Each one shows its diff and asks `Apply? [y/N]`. If `claude` or `codex` is not on `PATH`, setup uses the copy Claude Desktop, ChatGPT or the Codex app ships (macOS).
5. **Summary.** It ends with `kenaz setup doctor` and `kenaz status`.

`--yes` accepts every change without asking (use it in scripts). `--env`, `--bundle DIR`, `--no-bundle` and `--no-browser` are passed on to setup and login.

## Installing from a release archive

A release archive is self-contained: `bin/kenaz`, the `plugin/` package with its local marketplace, the OpenCode adapter in `opencode/`, and `bundle.json`, which lists every file's SHA-256. `kenaz init` (above) does everything below for you; to set up one app at a time, run setup with the archive's own executable, by its absolute path:

```
"<archive>/bin/kenaz" setup claude|codex
```

Setup needs the Kenaz relay's port. If the relay is not running, setup starts it (you no longer need to run `kenaz login` first).

When `kenaz` runs from an archive (or with `--bundle <dir>`), setup first verifies every file against `bundle.json` and refuses a modified, incomplete or mixed-version archive. It then registers the archive so that nothing depends on `PATH`, npm or the network:

- **Claude Code and Codex:** setup writes a per-user copy of the plugin under the Kenaz state directory, with the MCP server and hook commands pointing at the archive's `kenaz` by absolute path, and registers it with `claude plugin marketplace add` / `plugin install` or `codex plugin marketplace add` / `plugin add` (Codex has no `plugin install`). The archive itself is never modified.
- **OpenCode:** `mcp.kenaz` runs the archive's `kenaz` and the adapter entry points at its `opencode/` directory.

**Which plugin is used.** The plugin is pinned to a signed release, and setup never lets it track a moving branch. If you already registered the `kenaz` marketplace from GitHub, setup uses it only when it is pinned to the commit of the release you are running; a marketplace that follows the default branch, a branch or a tag is replaced by the archive's verified copy, with a warning. The pinned forms are:

- **Codex:** `codex plugin marketplace add kameas-ai/kenaz-plugin --ref <commit>`, where `<commit>` is the full commit the release tag `vX.Y.Z` points at (`kenaz version --json` prints it as `public_commit`). Codex checks the ref out after cloning. That is not tamper-proof on its own: Codex runs `git checkout <ref>` without verifying the result, and git prefers a branch named like the commit, so whoever controls the repository could serve other content under that name. Setup and `kenaz setup doctor` therefore read the HEAD of Codex's clone (`~/.codex/.tmp/marketplaces/kenaz`) and require the release commit, and, when you run them from a release archive, require the clone's `plugin/` to match the archive's verified copy. A clone that differs only in line endings still matches (git on Windows checks files out with CRLF by default). Codex does not run the plugin from that clone: it runs the copy it made when the plugin was installed (`~/.codex/plugins/cache/kenaz/kenaz/<version>`), and keeps that copy when you remove the marketplace and add it again with `--ref`. So when the marketplace is pinned, setup also compares that copy with the clone's `plugin/` (ignoring the skills Codex generates from the plugin's commands), and reinstalls the plugin (`codex plugin remove kenaz@kenaz`, then `codex plugin add kenaz@kenaz`) when they differ. `kenaz setup doctor` makes the same comparison and warns.
- **Claude Code:** a GitHub or git marketplace takes a branch or tag (`#vX.Y.Z`) but not a commit, so there is no pinned form of `claude plugin marketplace add`, and setup never counts such a ref as pinned, whatever it looks like. Use the archive's copy (the default), or the `extraKnownMarketplaces` entry from `kenaz setup print-managed claude --raw` (in managed settings or your own `~/.claude/settings.json`), an inline catalog that lists the plugin at the release tag and its commit with auto-update off. Setup never replaces or duplicates an inline (`settings`) `kenaz` marketplace, because Claude Code allows one marketplace per name; when it is not pinned to this release, setup warns and configures telemetry only.

When setup uses a pinned GitHub marketplace and its plugin is not installed yet, setup installs it (`claude plugin install kenaz@kenaz` or `codex plugin add kenaz@kenaz`), and `--undo` uninstalls only what setup installed; the marketplace stays yours. In Claude Code, an install at project or local scope runs in that one repository only, so it does not count as installed: setup installs the plugin for your user as well, and `--undo` removes only that. If you installed the plugin and turned it off, setup leaves it off (and installed on `--undo`) and tells you how to turn it on. Turned off means explicitly off: `false` in Claude Code's `enabledPlugins`, or `enabled = false` in Codex's `[plugins."kenaz@kenaz"]` table (a table with no `enabled` key is on, which is Codex's default). `kenaz setup doctor` reads it the same way. The GitHub plugin starts `kenaz` from `PATH`, so setup warns when `kenaz` is not on `PATH` (`kenaz init` fixes that). With `--no-bundle`, or a Homebrew or npm install (no archive copy to fall back on), setup still uses an unpinned GitHub marketplace, and warns with the pinned form to switch to. Two flags override this:

- `--bundle <dir>` registers that archive's plugin and replaces the GitHub marketplace. `--undo` removes it and prints the command that adds the GitHub marketplace back.
- `--no-bundle` never registers an archive and uses `kenaz` from `PATH`.

Any other `kenaz` marketplace that setup did not register (a different repository or a local directory) is refused, and setup asks you to remove it, so the plugin is never loaded twice.

Claude Code and Codex each hold one `kenaz` plugin registration and one set of telemetry settings, so each can be set up for one Kameas Fleet environment at a time. If one is already set up for another environment (for example by `kenaz init --env dev`), setup refuses, names that environment, and gives the command that removes it there: `kenaz setup --undo <host> --env <that environment>`. Run that, then run setup or `kenaz init` again. Until then `kenaz setup doctor` reports that the app exports to the other environment's relay.

The `stage` environment has been retired. Selecting it with `--env stage`, `KENAZ_ENV=stage` or `env = "stage"` in `config.toml` stops the command with "The stage environment has been retired. Use dev or prod." Nothing already on this device for `stage` is deleted or moved. To remove an old registration or sign-in for it, `kenaz setup --undo <host> --env stage` and `kenaz logout --env stage` still work. When that setup is recorded in an earlier release's folder (the one `kenaz migrate` names), the undo is careful not to remove a setup you made since, because an app has one plugin registration and one settings file for every environment. It removes the plugin only while the app still loads it from that folder, and removes the stale record. It never changes the app's settings from such a record: a later setup that found them as it would write them left no record of its own, so they may be the current setup's. Telemetry settings the earlier setup wrote can therefore stay behind. `kenaz setup --undo <host>` for your current environment removes a current setup with its settings, `kenaz init` sets the app up again, and settings no current setup owns can be deleted from the app's settings file by hand. A setup that registered no plugin of its own (`--no-bundle`, or a marketplace you had added) is never undone in the app from such a record. An app that still starts Kenaz with a stored `stage` setting keeps its tools that work signed out: the MCP server starts, never contacts Kameas Fleet, and `fleet_status` and each Kameas Fleet tool say that the app is still set up for `stage` and what to change: the plugin's environment option in Claude Code, the extension's setting in Claude Desktop, `KENAZ_ENV` in the environment the app starts in, or the `env` line in `config.toml`. `kenaz migrate` uses no environment, so it still runs with `stage` selected and says that it ignores the selection.

Paths with spaces work. Re-running setup changes nothing; running it from a newer archive re-registers that one. `kenaz setup --undo <host>` removes the registration and restores your configuration, and `kenaz setup doctor` checks it.

**Upgrades.** When the archive sits in a directory that an upgrade removes, setup launches `kenaz` through a path that doesn't change between versions:

- **Archive installer:** `install/current/bin/kenaz`.
- **Homebrew** (`brew install kameas-ai/tap/kenaz`): `$(brew --prefix)/bin/kenaz`, the link that `brew upgrade` moves to each new version. The versioned `Caskroom/kenaz/<version>/` directory isn't used.

After an upgrade, the hosts run the new `kenaz` with the previous version's plugin files. Those files include Codex's list of tools that ask before they write, so a write tool added by the new version has no entry in that list until the files are refreshed. `kenaz setup doctor` reports this, `kenaz mcp` (the server the hosts start) writes one line about it to its error output each time it starts, and running `kenaz init` again refreshes the files. Setups made by 0.5.x on Homebrew launched the versioned path, so `brew upgrade` broke them. `kenaz status`, `kenaz doctor` and `kenaz login` say when a registration points at a `kenaz` that no longer exists, in the Fleet environment the command runs in (`--env`, or the default), and `kenaz init` fixes it (the message says `kenaz init --env <env>` when that is not the default environment).

### Where Kenaz keeps its files

Kenaz keeps its files in `~/.kenaz/plugin` on every platform (`%USERPROFILE%\.kenaz\plugin` on Windows), next to the Kenaz desktop app's `~/.kenaz/harness`:

| | |
|---|---|
| `~/.kenaz/plugin/config.toml` | Configuration |
| `~/.kenaz/plugin/<env>/` | One Kameas Fleet environment's state (`prod`, `dev`): the relay, the queue, setup and skills. A `stage` folder left by an earlier release is kept as it is (see below) |
| `~/.kenaz/plugin/memory/`, `credentials/` | Memory, and the credentials files on a machine with no OS keychain |
| `~/.kenaz/plugin/.session.lock` | An empty file the official build locks while it signs in, signs out or refreshes its sign-in, so that two Kenaz processes don't do so at once. Its presence means nothing |

`KENAZ_HOME` replaces `~/.kenaz` (the archive installer uses it too, for `$KENAZ_HOME/install`).

**Moving from an earlier release.** Releases up to 0.6.x kept these files in `~/Library/Application Support/kameas/kenaz` (macOS), `$XDG_CONFIG_HOME/kameas/kenaz` and `$XDG_STATE_HOME/kameas/kenaz` (Linux), `%AppData%\kameas\kenaz` and `%LocalAppData%\kameas\kenaz\state` (Windows), or `$KENAZ_HOME/config` and `$KENAZ_HOME/state`. `KENAZ_HOME` now means the root that holds `plugin/` (and the installer's `install/`).

- **Nothing moves on its own.** After an upgrade, everything keeps working from the old location.
- **To move,** close Claude Code, Codex and Claude Desktop, then run `kenaz init` (or `kenaz migrate`). It moves the state and `config.toml` with one rename each. If anything is in the way (an app still running, a file open on Windows, another volume, missing permissions), nothing changes and the old location stays in use. Nothing is ever copied or deleted.
- It leaves a link at the old location (a junction on Windows where symlinks aren't allowed), so the apps keep loading the plugin from the path setup registered, and a `MOVED.txt` file that says where the files went. `kenaz init` also registers the apps from the new location; after `kenaz migrate`, run `kenaz init`.
- If `~/.kenaz/plugin` already has its own files (for example from a newer build), nothing is merged: `~/.kenaz/plugin` is used, and `kenaz migrate` and `kenaz doctor` list the old files. An old folder still holds that release's memories and sign-in data, and an app may still load the plugin from it: the listing says so ("do not delete it") and names the command to run first. Only a registration made by the same build from the new location replaces an old one, so `kenaz-dev init` never frees a folder the `kenaz` plugin still loads from. `kenaz init` lists the old files before it registers the apps, so it only says to keep them; run `kenaz doctor` afterwards to see whether they can be deleted.
- `kenaz doctor` says when files are still used from the old location. `kenaz status`, `kenaz doctor` and `kenaz login` say when an app still uses the plugin from there.

Downgrading to 0.6.x or earlier after moving isn't supported.

## OpenCode (preview)

**OpenCode is not part of the first release.** Released builds leave it out: `kenaz setup opencode` isn't offered, doctor and status don't mention OpenCode, and the relay drops any OpenCode activity on the device. The adapter stays in this repository; build with `go build -tags kenaz_opencode ./cmd/kenaz` to try it.

OpenCode support is new and not yet qualified on every platform. OpenCode does not load the Claude Code plugin format, so Kenaz connects to it in two pieces, which `kenaz setup opencode` registers in your global `opencode.json` (with a diff, a backup, and `kenaz setup --undo opencode` to restore it):

- the `kenaz` MCP server, for the status, context, skills and memory tools;
- the Kenaz OpenCode adapter (`opencode/`), a small OpenCode plugin with no dependencies. It observes OpenCode's session, message and tool events and reports only metadata (the same closed allowlist as for the other harnesses) to the local `kenaz` relay. It never changes what OpenCode does.

Pass `--adapter <dir>` if the adapter is not in the `opencode/` directory next to the `kenaz` binary's `bin/`. Setup edits only `opencode.json`, which must be plain JSON, and needs OpenCode 1.19.4 or later. The adapter was checked against events recorded from OpenCode 1.19.4. Skills synced from your organization appear in OpenCode through `~/.agents/skills`.

Differences from Claude Code and Codex: there is no session-start banner or memory index (use the `fleet_status` and `memory_*` tools), and OpenCode activity is sent only once your organization's Kameas Fleet accepts OpenCode as a client. Until then it is dropped on the device.

## Signing in and where credentials are kept

`kenaz login` opens your browser to sign in. Sign-in redirects back to a one-time listener on `127.0.0.1` (a random port) in the `kenaz` process. If no browser can be opened (for example on a machine with no display), or with `kenaz login --no-browser`, `kenaz` prints the sign-in link instead and keeps waiting for the redirect.

Sign-in tokens and the key that encrypts queued uploads are kept in the OS keychain: the macOS Keychain, the Windows Credential Manager, or the Secret Service on Linux. On a machine with no usable OS keychain (such as a Linux VM with no Secret Service), `kenaz` keeps them as files instead, in `~/.kenaz/plugin/credentials` (or `$KENAZ_HOME/plugin/credentials`). The directory is created with mode 0700 and each file with mode 0600, and `kenaz` refuses to read a credentials file that other users can read. `kenaz logout` deletes these files. `kenaz doctor` and `kenaz status` show which store is in use (`credential store: OS keychain` or `credential store: files (no OS keychain)`).

The first sign-in or relay start records the choice in `credentials/STORE`, and `kenaz` uses the recorded store from then on, without checking again. If the recorded store is the OS keychain and it can't be reached, commands stop with `OS keychain unavailable; try again or run kenaz logout to reset`; they never switch to files on their own, which would sign you out. `kenaz logout` removes the record, so the next sign-in chooses again. `kenaz doctor` shows the recorded store and whether a fresh check agrees with it. The harness hooks and `kenaz mcp` never check for a keychain (they have to start quickly). Until a store is recorded, they treat the device as signed out.

**Without an OS keychain, sign-in tokens and the spool key are protected only by file permissions in your home directory.** Anyone who can read your files as you, or as an administrator of the machine, can use them.

### Signing in from inside a VM (such as a Kenaz Workbench)

The same `kenaz` runs inside or outside a VM; there is no separate mode. Run `kenaz login` (or `kenaz login --no-browser`) in the VM and open the printed link in a browser:

- **A browser inside the VM** completes sign-in directly, because the redirect goes to `127.0.0.1` in the VM.
- **A browser on the host** completes sign-in only if the host's `127.0.0.1:<port>` reaches the VM's `127.0.0.1:<port>`, for the port in the printed link's `redirect_uri`. Some VMs forward localhost ports to the guest automatically; others need a forward set up after the link is printed, for example `ssh -L <port>:127.0.0.1:<port> <vm>`. Without a forward, the host browser shows a connection error after you sign in.
- If the host browser ends on that connection error, the address bar holds the full redirect (`http://127.0.0.1:<port>/callback?code=…&state=…`). Running `curl '<that address>'` inside the VM, while `kenaz login` is still waiting, delivers it and completes sign-in. The code is single-use and only works with the waiting `kenaz login`.

## Approving writes

Tools that write to Kameas Fleet (`skills_publish`, `context_share`, `context_promote`, and `context_share_file` in builds with the context file library) and memory writes that reach Kameas Fleet (forget-all, sending held changes, moving notebook notes into your account, and changing or deleting a note Fleet already holds) don't write when your agent, or a `kenaz memory` command, asks for them. They prepare a request and return a `request_id`, `status: "pending"` and an `approve_url`. Nothing is uploaded until you say so, in either of two ways; the one exception is the context share grant, described below. Check the exact text, then approve or decline:

- **The button:** open the link and press **Approve** or **Decline**. The page is served by the local `kenaz` relay at `http://localhost:<port>/approvals/`. The text it shows is written by the relay from the exact stored content, not by the agent.
- **The command:** run `kenaz approve [id]` or `kenaz decline [id]` in a terminal. It shows the same content and asks once; Enter, `y` or `yes` means yes. With no id and one request waiting, it uses that one. With several, it lists them. It needs a real terminal, so a call from an agent's shell without one is refused.
- **Status:** `kenaz approvals` lists requests and their status, `kenaz approvals open <request_id>` opens one, and the `approval_status` tool lets the agent check.
- **Expiry:** requests expire 5 minutes after the agent prepares them. `kenaz logout` or switching accounts cancels open requests and deletes their content. If the relay stops in the middle of a write, the request shows `reconciling`. Kenaz then checks whether the write arrived and never sends it again blindly. Signing out closes a `reconciling` request as `outcome_unknown`; it is not retried after you sign back in. A request still `reconciling` after 7 days is closed as `failed` (`outcome_unknown`).
- **Many notes:** one approval lists at most 64 notes, each with its full text (the page shows a preview and a "Show full text" control). With more, the command approves the first 64 and says how many remain; run it again after approving to review the next batch.
- **Opened from another site:** an approval link opened from a web page, chat or email is refused. Paste it into the address bar, or use `kenaz approve`.
- **Unavailable:** without an OS keychain (`credential store: files`), or with no relay, the write tools return `approval_unavailable` and nothing is written, because the pending requests couldn't be kept safely.
Approving guards against mistakes. It isn't a security boundary against an agent that controls your computer: use your AI tool's own permissions for that.

### Teams you can read but can't share to

You can share context only with a team you are a member of. Kameas Fleet may also let you read context from teams you aren't in: the teams above yours in your organization's team tree, and the teams below a team of your own (other than the all-members team). Kameas Fleet decides whether your organization has this; where it doesn't, every team listed is one of yours and nothing here changes.

- **Reading works for all of them:** `context_search`, `context_pull`, `context_pull_files` and `kenaz context sync --team <name>` take any team you can read, by name or id.
- **Sharing works only for your own:** `context_share`, `context_share_file` and the sending half of `kenaz context sync` refuse a team you can only read, before anything is prepared: no approval request, no file id held, and nothing of the context share grant used. The refusal has the code `not_team_member`, says that you can read that team's context but aren't a member of it, and lists the teams you can share to, by name and id. This holds for a team named in the call, for the `default_team` setting, and for a file that is already shared with a team you have since left but can still read.
- **"Your only team" is the one team you belong to,** however many others you can read. With one team of your own and no team named, shares go to it and reads are about it.
- **The all-members team:** you can share with it only if you are a member of it. Kameas Fleet can list it as a team you only read; a result still says that everyone in the organization reads what is shared there, and a share to it from you is refused.
- **A copy you pulled from a team you only read stays a copy.** `kenaz context sync --team <name>` for such a team brings its files down into `~/.kenaz/team-contexts/<team>/`, says why nothing goes up, and stops: files you add or change in that folder aren't sent. A folder that already holds files is never taken for a team you only read: if `<team>/` has files of yours, it isn't bound to that team and your files in it stay where they are; the team's copies go into a folder beside it (`<team>-<part of the team's id>/`), and sync says which is which. One kind of file does move: a copy in `<team>/` that this device's record shows you pulled from that same team, under this account, is moved into the team's folder with the rest of its copies. What sync says about a folder whose owner it can't read, and its exit code for that, are the same as for a team of yours. `context_share_file` refuses to update such a copy, even a file you wrote there once. To share your version, share it as a new file of your own (`as_new`) with one of your teams.
- **`team_members` tells them apart:** each team has `membership`, `direct` for a team of yours and `inherited` for one you only read, and when some are inherited a `note` says which are which in words. A Kameas Fleet that doesn't report membership gives neither, and the output is as it was.
- **If Kameas Fleet refuses anyway** (your membership changed after the check), the request fails with `not_team_member` and the result says the share was refused because you aren't a member of that team. It isn't a matter of your role, and an administrator's role doesn't lift it: an administrator can add you to the team.

- **Teams Kenaz couldn't read:** if Kameas Fleet sends a team in a form this version of Kenaz can't read, that team is hidden, not guessed at. `kenaz status`, the `fleet_status` tool and `team_members` then say so in one sentence, with a count and nothing else ("Kameas Fleet sent 2 teams this version of Kenaz could not read; they are hidden. Updating Kenaz may fix this."), and a tool that finds no team of yours says that too, and doesn't tell you that you are in no team.

Other reasons Kameas Fleet can give for refusing one shared item (the file belongs to another member, its team no longer exists, a reviewer already rejected this exact text) don't reach Kenaz under those names yet, so its results don't tell them apart: where Kameas Fleet reports one of them as "not permitted", Kenaz says that only the author can change the file.

### Sharing context files without an approval for each

This is in builds with the context file library (`kenaz_context_library`), which the official release compiles in. It is off unless you turn it on.

**It changes a rule.** Until now a person approved the exact text of every context share an agent prepared. With the context share grant on, your AI tool can share a context file with one of your teams and you don't see the text first. Nothing else about approving writes changes.

- **Turning it on:** run `kenaz context grant on` in a terminal. It says what the grant does and asks once; only `y` or `yes` turns it on. It needs a real terminal, so a call from an agent's shell without one is refused. No tool, hook, relay control route, environment variable or `config.toml` setting turns it on. `kenaz context grant off` turns it off, and needs no terminal.
- **What it covers:** the `context_share_file` tool, for the account signed in when you turned it on. While it is on, a file share runs as soon as your AI tool asks, through the same steps as an approved one: the same checks of your sign-in, team and plan, the same check for credentials and hidden characters (made again by the relay), the same record of which file is which, and the same request to Kameas Fleet, which checks the text again. The tool's result tells your AI tool that the file was shared without an approval, with which team, and how to turn the grant off.
- **What still asks every time:** text your AI tool marked as drafted from your memory notes; promoting a note to your whole organization (`context_promote`); shared notes (`context_share`); skills (`skills_publish`); every memory write. The refusals stay too: a file already shared stays with its team, and a file with a credential in it isn't shared.
- **Memory-derived text is recognised only by its mark.** Your AI tool sets the mark when it saves or shares text it drafted from your notes. Kenaz can't detect memory-derived text that wasn't marked, so with the grant on such text can be shared without your approval.
- **Which teams:** every team you are a member of, including your organization's all-members team (usually named Everyone) when you are a member of it. A file shared with that team is readable by your whole organization, so with the grant on your AI tool can publish to the whole organization without asking. A team you aren't in is never shared with, and that includes a team whose context you can only read (see "Teams you can read but can't share to"): the tool refuses it before the grant is asked, and the relay's own check of the grant refuses it again.
- **Limit:** 30 files in a clock hour (the count starts again on the hour). Past that, a share waits for your approval as before, and the result says so. **What counts:** every share the grant takes, whatever becomes of it: shared, still being sent, canceled, or failed for any reason but one. A failed share counts because it was an attempt, and may have reached Kameas Fleet. The one that doesn't count is a share Kameas Fleet refused outright because you aren't a member of the team (`not_team_member`): nothing was shared, the same request can never succeed, and its place in the hour is given back. The tools refuse such a share before the grant is asked, so this happens only when your membership changes in between. `kenaz context grant status` lists it as not shared and not counted. **Those refusals have a limit of their own:** 10 in a clock hour. Up to 10 give their place back; any more keep it; and once an hour has 10, the grant shares nothing more until the next hour, every share waits for your approval, and the result and `kenaz context grant status` say so. The record on this device never drops a line that counts toward the current hour to make room for one that doesn't. The count is kept by the running relay and in the record below; it guards against a runaway agent, and something that can delete files in the relay's state folder and restart the relay could reset it. Both hourly limits (the 30 shares, and the 10 refusals described below) go by this device's clock and that record: moving the clock into another hour, or deleting the record and restarting the relay, starts the count again. A person at the terminal can do either; an AI tool acting through Kenaz's tools can do neither, since no tool sets the clock, deletes those files or restarts the relay. The memory grant for new notes allows 300; this is lower because a shared file is readable by a whole team.
- **Afterwards:** `kenaz context grant status` says whether it is on and lists the files shared this way from a record on this device: the short name, the team, the SHA-256 of the text and the time, never the text. The record keeps the last 500, is deleted when you sign out, and can be erased by anything that can delete files in the relay's state folder; Kameas Fleet's own record of what your team holds doesn't depend on it. `kenaz approvals` marks those requests, and `kenaz status` and the `fleet_status` tool say whether the grant is on.
- **When it ends:** `kenaz context grant off`, `kenaz logout` (from any release), and another account signing in. It never carries to another account or organization, and once ended it stays ended: a copy of the grant's file put back later isn't a grant.
- **Where it is kept:** in a file under the relay's state folder, sealed with keys from the OS keychain: the one that seals pending requests, a random id made when you turn the grant on and deleted when it ends, and a key that every sign-out deletes. Without an OS keychain it can't be turned on.
- **Your organization:** Kameas Fleet has no setting for this yet. The code has one place where such a setting will be read; when it says no, the grant can't be turned on and one already given is ignored.

What this protects against: an agent that can only use Kenaz's tools, or can only write files, can't turn the grant on or bring an ended one back. It doesn't stop a program running as you that can read your keychain or drive a terminal of its own. Such a program can already approve a single request today; with the grant it could turn on approval for many. Like approving, the grant guards against mistakes and isn't a security boundary against an agent that controls your computer.

## Memory

Run `kenaz memory on` in a terminal. It shows the notices once and asks once, then turns on what your build and plan allow: automatic capture, sync and bringing over notes from claude-mem. `kenaz memory off` stops capture and sync; your notes stay.

Memories are stored on this device. They are sent to Kameas only if you turn on memory sync for your Kameas Fleet account (off by default), and never as telemetry. If you turn on automatic memory capture, session content can also go to the model that writes your notes, as its own notice explains. When your AI tool uses a memory in a session, it becomes part of that session and is sent to your AI tool's provider (for example Anthropic for Claude Code, or OpenAI for Codex) like anything else in the conversation.

Kenaz memory gives Claude Code and Codex a small notebook on your device. The agent saves short notes with the `memory_save` tool (durable facts, decisions, conventions and fixes), finds them again with `memory_search`, `memory_get` and `memory_recent` (keyword search on this device; no embeddings), and can change or delete them with `memory_update`, `memory_forget` and `memory_forget_all`. At the start of each session, the agent gets a short index of the newest note titles for the current project (titles and ids; if automatic capture is on, also the summary of your last turn). Every result says where a change took effect: on this device, waiting for your approval, queued for or synced to your account, or refused.

- **Where:** SQLite files under the Kenaz state directory, readable only by you (directory 0700, files 0600). Notes saved while signed out go to the device notebook, `memory/memory.db` (`kenaz memory path` prints it); everyone who shares an OS account sees those. Notes saved while signed in go to a separate store for that Kameas Fleet account (`memory/profiles/`), which is shown only while that account is signed in: after you sign out or switch accounts, the previous account's notes leave your agent's results and stay on disk.
- **No sign-in needed:** memory works whether or not you are signed in to Kameas Fleet, and telemetry choices don't affect it.
- **Chat apps:** By default chat reads only notes not tied to a project. You can let it read notes from all your projects in the Kenaz extension's settings in the Claude desktop app, or with `kenaz memory recall all`. Those notes can then go to that app's AI provider.
- **Limits:** notes are up to 8 KiB with a 200-character title; each store holds up to 10,000 notes or 128 MiB. When it is full, nothing is deleted automatically: remove notes with `kenaz memory forget <id>`, or empty the device notebook with `kenaz memory purge`.
- **Credentials:** `memory_save` refuses text that obviously contains a credential (for example API keys, access tokens or private keys), and a stricter check keeps credential-like notes from ever being synced. These are basic checks, not a scanner: never ask your agent to save secrets or personal data.
- **Your approval:** while you are signed in, saves sync after you approve once; changing or deleting a synced note, and forget-all, wait for your approval each time (see [Approving writes](#approving-writes)); and neither does a flag your agent sets. If no approval is possible (no OS keychain, or the relay is not running), saves and edits still apply on this device but their upload waits until you approve it (`kenaz memory sync approve`); a new text never reuses an approval you gave for an earlier one. A deletion only hides the note: it is deleted when you approve, and `kenaz memory revert <id>` shows it again. Deleting everything, turning on sync and moving notebook notes into your account are refused (`approval_unavailable`); turning on sync uploads exactly the notes listed on the approval page. Signed out, the device notebook works as before: save, edit, delete and `kenaz memory purge` need no approval, because nothing leaves the device.
- **Your controls:**

  ```sh
  kenaz memory list [--project current|all] [--limit N]
  kenaz memory search <query>
  kenaz memory show <id>
  kenaz memory update <id> [--title T] [--body B] [--tags a,b]
  kenaz memory forget <id>
  kenaz memory export          # your notes as NDJSON
  kenaz memory forget-all      # this account's notes, here and in Kameas Fleet (needs approval)
  kenaz memory purge [--yes]   # delete the device notebook (signed-out notes only)
  kenaz memory path
  kenaz memory disable         # or enable; memory is on by default
  ```

  Turning memory off stops the tools and the session index and keeps the stores; `kenaz memory purge` deletes the device notebook, and an account's notes are deleted only by an approved `kenaz memory forget-all`. `kenaz status` and `kenaz doctor` show whether memory is on, how many notes it holds and whether sync is on.

### Memory sync (optional)

With an official build, a plan that includes memory sync, and your sign-in, you can sync the notes of your Kameas Fleet account to your other enrolled devices. It is off until you turn it on with `kenaz memory on`, or `kenaz memory sync enable` for sync alone, in a real terminal: it shows the Memory Sync Notice, then Enter or y turns it on. This consent is separate from telemetry consent: turning one on or off never changes the other.

- **What is sent when sync is on:** for each note of the signed-in account in a synced scope (notes saved with `project=global`): its text and title, a SHA-256 fingerprint of the text, when it was created, which tool saved it (Claude Code, Codex, OpenCode or the Kenaz CLI), and change timestamps that keep edits in order, plus your edits and deletions. Nothing else: no tags, no local paths, no telemetry.
- **What is never sent:** notes saved while signed out (unless you move them into your account with `kenaz memory sync adopt`), notes that belong to one project (Kameas Fleet does not sync project notes yet), and credential-like notes.
- **Who can see it:** only you, signed in to your own account. Kameas Fleet has no administrator, team or staff view of it, and it is never shared with a team.
- **Turning it off:** `kenaz memory sync disable` stops sending new notes. Notes already synced stay in your account until you delete them: forget them one by one (the deletion is still sent), or run `kenaz memory forget-all`, which erases them in Kameas Fleet and on this device. If your plan stops including memory sync, Kameas Fleet keeps synced notes for 90 days and then deletes them; deleting them keeps working.
- **Status:** `kenaz memory sync status` (or the `memory_sync_status` tool) shows what is synced, queued, waiting for your approval, on this device only, or refused, and why.

  ```sh
  kenaz memory sync status | enable | disable | approve
  kenaz memory sync adopt <id>... | --all
  ```

## Uninstall

`kenaz logout` signs out and deletes queued Fleet data, but it keeps your memory on this device, because memory is your data, not telemetry: it stops memory sync, and your account's notes leave your agent's results until you sign in again. To delete the device notebook, run `kenaz memory purge`. An account's notes, on this device and in Kameas Fleet, are deleted only by an approved `kenaz memory forget-all` (or `kenaz memory forget <id>`) while signed in to that account. The device notebook is per OS user, so it holds the memories of everyone who uses this OS account. `kenaz setup --undo claude|codex|opencode` restores your Claude Code, Codex or OpenCode configuration and also leaves memory in place.

## Development

```sh
make check   # fmt, vet, tests, policy and naming gates
make build   # bin/kenaz (unofficial build)
```

## License

Apache-2.0 (see [LICENSE](LICENSE) and [NOTICE](NOTICE)). Official release binaries are licensed as described above.

Claude and Claude Code are trademarks of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz for Claude Code and Codex is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic or OpenAI.
