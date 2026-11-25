# Kenaz in Anthropic's directory: submission packet

Kenaz goes into Anthropic's directory as two listings, both submitted from the developer portal at https://claude.ai/directory/manage (**Submit new**):

- **A. Plugin bundle:** the customer plugin in `plugin/`, read from the public repository `kameas-ai/kenaz-plugin`. It runs in Claude Code and in Cowork sessions on the user's computer.
- **B. MCP connector:** the hosted Kameas Fleet connector at `https://mcp.fleet.kameas.ai/mcp`. It works in Claude on the web, desktop and mobile, and in Cowork.

Anthropic asks for a remote MCP server to be submitted as a connector even when a plugin also uses it. Submit B first and then A, from the same Claude organization, so the portal can offer to pair them.

Nobody signs in to the portal, submits or publishes on Kameas's behalf. Every portal step below is the owner's. This file prepares the answers and records the checks run before submitting.

Sources (read 2026-11-24):

- Publish to the directory: https://claude.com/docs/directory/publish.md
- Submit a plugin: https://claude.com/docs/plugins/submit.md
- Plugin pre-submission checklist: https://claude.com/docs/plugins/pre-submission-checklist.md
- Submit a connector: https://claude.com/docs/connectors/building/submission.md
- Connector pre-submission checklist: https://claude.com/docs/connectors/building/review-criteria.md
- Authentication for connectors: https://claude.com/docs/connectors/building/authentication.md
- Anthropic Software Directory Policy: https://support.claude.com/en/articles/13145358-anthropic-software-directory-policy
- Anthropic Software Directory Terms: https://support.claude.com/en/articles/13145338-anthropic-software-directory-terms

## Before you start (owner, both listings)

- [ ] **Owner account on a paid plan.** Submit from a claude.ai account on Pro, Max, Ultra, Team or Enterprise. On Team or Enterprise, submit as an Owner, or as a member an Owner has given the **Directory** permission (Enterprise only). The listing belongs to the organization you submit from, and for a plugin bundle the first organization to submit a repository folder holds that listing for good. Use the Kameas AI organization.
- [ ] **GitHub.** Connect a GitHub account that can push to `kameas-ai/kenaz-plugin` to claude.ai in that same Claude organization. The portal checks push access before it creates, submits or publishes a plugin. The repository is public, so the Claude GitHub App is optional for validation; install it on `kameas-ai/kenaz-plugin` anyway if you want push updates through the webhook (that needs admin access to the repository).
- [ ] **Read the Directory Terms and Policy** linked above. Both compliance steps ask you to accept them.
- [ ] **v0.9.2 is tagged and public.** The plugin bundle is submitted from tag `v0.9.2` of `kameas-ai/kenaz-plugin`, the first release with the listing icon. Check that `git show v0.9.2:plugin/.claude-plugin/icon.png` exists in the public repository before the first save.
- [ ] **Support contact.** Use support@kameas.ai (confirmed by the owner) wherever the portal asks for a support or contact email.

## A. Plugin bundle

### Portal fields

| Step | Field | Value |
|---|---|---|
| Submit new | What would you like to submit? | **Plugin bundle** |
| Source | Repository | `kameas-ai/kenaz-plugin` |
| Source | Plugin path | `plugin` (the folder that holds `.claude-plugin/plugin.json`) |
| Source | Branch or tag | `v0.9.2`, the first tag with the listing icon (`plugin/.claude-plugin/icon.png`). Don't submit v0.9.1 or any earlier tag: the portal takes the icon only on the first save |
| Source | Validate | Run it, and fix any **Blocking** finding before going on |
| Listing details | Name, description, icon | Read by the portal from the tag: the name and the approved manifest description from `plugin/.claude-plugin/plugin.json`, the icon from `plugin/.claude-plugin/icon.png`, and `plugin/README.md`. They can't be typed in; see "Listing copy" below |
| Data handling | Four questions | See "Data handling answers" |
| Compliance | Contact email | support@kameas.ai |
| Compliance | Acknowledgements | All four, as owner steps below |
| Review and submit | How new versions reach the directory | **Scheduled check only** while the listing tracks a tag (a tag stays on its commit, so the webhook adds nothing). Switch to **GitHub push webhook** if you later track a branch |
| Review and submit | Auto-publish passing versions | Off for the first listing. Anthropic decides the publish setting at approval |

A tracked tag stays on its commit. To release a later version, change **Tracked branch or tag** on the plugin's **Settings** tab to the new tag and select **Publish update** when it passes. The version in `plugin.json` must go up with every release.

### Listing copy

**The listing description is the approved manifest description** (section 6 of the approved listing copy), because the portal reads it from `plugin/.claude-plugin/plugin.json` at the tag, with the one-phrase change noted below. It reads, verbatim:

> Kenaz, a Kameas AI product. Connects Claude Code and Cowork on your computer to your organization's Kameas Fleet: shared skills, team context and your own memory, and in Claude Code, usage summaries you choose to share. Requires the kenaz program and a Kameas Fleet account. Claude, Claude Code and Cowork are trademarks of Anthropic, PBC. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic.

It ends with the Anthropic-only attribution, which `TestDisclaimerPresent` enforces. Don't change it in the portal; if a reviewer edits it during review, the listing keeps the reviewer's text.

**"Your own memory" (owner decision, 2026-11-25).** The manifest description above, the section 8 short and long descriptions below and the marketplace description say "your own memory" where the approved listing copy says "your private memory" (or "a private memory"). If the organization allows it, administrators and the team leads above a member can read the member's synced notes, and each read is logged (privacy policy section 13), so "private" would overstate it. This deviates from the approved copy on that phrase only; every other character is as approved. Legal is to be informed.

**Approved directory copy for the fields the portal asks you to type** (section 8 of the approved listing copy), verbatim except for "your own memory" (see above). Use it only where the portal offers a free-text field; it doesn't replace the manifest description. It names Claude Code and Cowork, which are the approved names for this surface. The listing name and every command name contain no third-party mark, and the text names Anthropic products only in the "for" and "works with" sense, so the attribution is the Anthropic-only form.

- **Name:** Kenaz
- **Publisher:** Kameas AI, Inc.
- **Short description (one line):** Your organization's shared skills, team context and your own memory from Kameas Fleet, in Claude Code and Cowork.
- **Long description:**
  > Kenaz, a Kameas AI product, connects Claude Code and Cowork sessions on your computer to your organization's Kameas Fleet. Search and install your team's shared skills, search and add to your team's context, and keep your own memory that follows you between the AI tools you use. In Claude Code you can also choose to share usage summaries with your organization (session counts, tool kinds, model names, token counts and cost); Kenaz never shares prompts, AI responses or file contents as usage data, and nothing is shared until you sign in and choose. In Cowork, Kenaz works in sessions that run on your computer. Early access.
- **Prerequisites:** The `kenaz` program installed on your computer (one-line install: https://kenaz.kameas.ai/). An account in a Kameas Fleet organization that has joined the Kenaz early-access program.
- **Privacy policy:** https://kenaz.kameas.ai/privacy.html
- **What Kenaz sends:** https://kenaz.kameas.ai/telemetry
- **Supported tools:** https://docs.kameas.ai/docs/kenaz-plugin/supported-tools
- **Support:** https://docs.kameas.ai/docs/kenaz-plugin/troubleshooting
- **Licence:** Apache-2.0 (plugin package); the `kenaz` program is licensed under the Kameas Client Software License, https://kenaz.kameas.ai/client-license
- **Attribution (end of long description, or the portal's legal or notes field if it has one):** Claude, Claude Code and Cowork are trademarks of Anthropic, PBC. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic.

Both texts name Claude Code and Cowork, the approved names for this surface; the listing name and every command name contain no third-party mark. Trim section 8 text only by dropping whole sentences from the end of the long description, never the prerequisites or the attribution.

Before submitting, check the long description against the Directory Terms and Policy as accepted at submission, in particular the rules against collecting extraneous conversation data and against building user profiles (Policy 1E and 1G). Usage telemetry comes from Claude Code's own OpenTelemetry export, which `kenaz init` points at the local `kenaz` program; it doesn't come from the plugin's hooks or tools. In Cowork the plugin's hooks send nothing. Confirm that this is how the reviewer reads it.

### Data handling answers

| Question | Answer |
|---|---|
| Does the plugin read or store personal data? | **Yes.** On the user's computer, under `~/.kenaz/plugin`, readable only by the user: sign-in tokens (in the OS keychain where there is one), the account and organization ids, a device display name the user chooses, memory notes the user's AI tool saves, pending approvals, installed-skill records, the queue of usage telemetry waiting to upload, and, only if the user turns on automatic memory capture, a queue of session content waiting to be written into notes. It reads team context and skills from the user's Kameas Fleet when the user's AI tool asks. Usage telemetry, only for the classes the user opts in to, is linked to the user's account and device. Prompts, AI responses, tool inputs and outputs, code, file contents and file paths are never sent as telemetry. Memory notes reach Kameas Fleet only if the user turns on memory sync. If the organization allows it, administrators and the team leads above the member can read synced notes written after the member is told, and each read is logged (privacy policy section 13). |
| Does it send data to services other than its declared connectors? | **Yes.** The bundle declares no remote connector: its MCP server is the local `kenaz` program. That program sends data to (1) the Kameas Fleet API, `https://api.fleet.kameas.ai`, run by Kameas AI, Inc., only after the user signs in: device registration, the telemetry classes the user opts in to, skill and context searches and reads, content the user approves for sharing or publishing (one exception: once the user turns on the context share grant in a terminal, context file shares go out without a per-file approval), and memory notes only if the user turns on memory sync; (2) Kameas's sign-in provider, ZITADEL (`https://prod-bq6woq.us1.zitadel.cloud`, sign-in pages at `https://login.kameas.ai`), for a standard OAuth sign-in with PKCE and token refresh; (3) only if the user turns on automatic memory capture and picks a note writer that uses one, the model provider the user chose (Anthropic, or Amazon Bedrock, Google Vertex AI or Microsoft Foundry as Claude Code is set up to use), under the user's own login or key. The relay also listens on `127.0.0.1` for Claude Code's OpenTelemetry export and for the local approval page. Nothing else: no analytics, crash reporting or update checks of its own. |
| How long does it keep data? | On the computer, until the user deletes it: `kenaz logout` deletes queued uploads, pending approvals and installed skills from Fleet; `kenaz memory capture disable` deletes the capture queue; `kenaz memory purge` and an approved `kenaz memory forget-all` delete notes. Approval requests expire after 5 minutes. In Kameas Fleet, retention follows the Privacy Policy, https://kenaz.kameas.ai/privacy.html (section 5, "How long we keep personal information", and the Data Processing Addendum it links). Synced memory notes stay in the user's account until the user deletes them, and are deleted 90 days after the organization's plan stops including memory sync (README, "Memory sync (optional)"). |
| Is it intended for people under 18? | **No.** Kenaz is a business product for members of a Kameas Fleet customer organization. |

### Compliance acknowledgements (owner)

The Compliance step shows four acknowledgements and requires all four. Anthropic's docs don't publish their wording, so read each one in the portal before selecting it.

- [ ] Check the contact email shows support@kameas.ai.
- [ ] Read and select acknowledgement 1.
- [ ] Read and select acknowledgement 2.
- [ ] Read and select acknowledgement 3.
- [ ] Read and select acknowledgement 4.

If any of them asks for something this packet doesn't cover (for example a commitment about the separately installed program), stop and raise it before selecting it.

### Expected human-review triggers

A person reviews every new listing. These parts of the bundle are the ones most likely to be held or questioned, and each is disclosed in `plugin/README.md` at v0.9.1 and v0.9.2 (section "What this plugin runs, sends and fetches") and in the root README's section of the same name.

| Trigger | Why it is likely flagged | How the README addresses it |
|---|---|---|
| **The separately installed `kenaz` program** | The MCP server (`kenaz mcp`) and all six hooks run `kenaz` from the user's `PATH`, not a file inside the plugin folder. The checklist holds a local MCP server that doesn't run a file in the plugin (**MCP server command wasn't read**), and wants everything a hook or server runs inside the plugin folder. The release binaries include a proprietary sign-in and transport module, so not all of what runs is readable source. | "The `kenaz` program is installed separately": the plugin package has no executable code; the install commands; without `kenaz` the hooks and tools fail to start and nothing is sent. The top of the README says that what Kenaz reports is decided by open-source code in the repository, that the part connecting to Kameas Fleet is proprietary, and that release binaries are official builds under the Kameas Client Software License. |
| **Hooks** | Six command hooks on `SessionStart` (two), `SessionEnd`, `UserPromptSubmit`, `PostToolUse` (async) and `Stop`. The session-start hooks add a status line, the memory note index and the list of local context files to every session. | "Commands it runs": a table of every hook, its command and what it does. Each hook has a 1 to 5 second limit, always exits successfully, makes no permission decision and never rewrites tool input. The session-start hook sends nothing itself. |
| **Memory capture** | The `UserPromptSubmit`, `PostToolUse` and `Stop` hooks can queue prompts, tool use and turn endings to write memory notes, and the memory index changes what the next session starts with. Policy 1E and 1G (no extraneous conversation data; no persistent storage of conversation content to build profiles or change Claude's behaviour across sessions) are the rules a reviewer will test this against. | "Commands it runs" and "Turning each part off": capture is off by default and the hooks exit without reading their input until the user turns it on in a terminal; when on, the queue stays on the device, common credential shapes, email addresses, home paths and `<private>` blocks are removed first, and the README says this is a safety net, not a guarantee. "Where it connects" names the model provider that receives session content when capture is on. `kenaz memory capture disable` turns it off and deletes the queue; `kenaz memory capture exclude` and `.kenaz-no-capture` exclude a project. |
| Skills from Fleet (possible) | While signed in, the relay installs skills the organization's administrator requires into `~/.claude/skills/`. A required skill keeps its `allowed-tools` and hooks only if an administrator reviewed exactly that version. The security scan looks for changes to Claude's permission settings. | "What it keeps on your computer" says which skills are installed without asking and when `allowed-tools` and hooks are kept; `kenaz logout` removes every skill from Fleet. |

If the review objects to the capture hooks, the fallback is a release that moves them into `kenaz setup` and ships the bundle with only the session-start and session-end hooks.

### Self-check against the blocking items (public tag v0.9.1, plus the v0.9.2 icon)

Run on 2026-11-24 against `v0.9.1` of `kameas-ai/kenaz-plugin` (commit `54a0d0c`), plugin path `plugin`, by reading the tag with `git show v0.9.1:<path>`, extracting it with `git archive v0.9.1`, and running `claude plugin validate ./plugin` (Claude Code 2.1.326). These are the checklist rows whose result is **Blocks** or **Validation stops**. v0.9.2 adds only the listing icon (and the version bump) to the plugin folder; the icon PR ran `claude plugin validate` on the result, and the icon warning is gone. Once v0.9.2 is tagged, run the portal's **Validate** on it: it runs more checks than this.

`claude plugin validate` result: **Validation passed**, with one directory warning (no icon, below) and one piece of advice (no marketplace install line in the README).

| Checklist item (blocking) | Result | Evidence |
|---|---|---|
| Submit a folder that contains `.claude-plugin/plugin.json` | Pass | `plugin/.claude-plugin/plugin.json` exists |
| Submit one plugin at a time | Pass | `.claude-plugin/marketplace.json` lists one plugin, `kenaz`, source `./plugin` |
| No `plugin.json` path outside the folder; no `..` in a command or script | Pass | `plugin.json` declares no component paths; no command contains `..` |
| Regular files, not symlinks, submodules or LFS pointers, for what the plugin loads | Pass | Every entry under `plugin/` is mode `100644`; no symlink or submodule anywhere in the tag; no `.gitattributes` |
| No `.DS_Store`, `Thumbs.db`, `desktop.ini` or `__MACOSX` | Pass | None in the tag |
| File names valid on Windows and macOS | Pass | No colon, trailing dot or space, device name, or names differing only by case, anywhere in the tag |
| Folder names on the plugin path use letters, digits, dots, hyphens and underscores | Pass | The path is `plugin` |
| No `export-ignore`, `export-subst` or content filters in `.gitattributes` | Pass | The tag has no `.gitattributes` |
| Repository under 50 MiB archived, 256 MiB unpacked, under 10,000 entries; each plugin file under 5 MiB | Pass | 1.4 MB as a `.tar.gz`, 6.8 MB unpacked, 739 entries; the largest plugin file is `README.md` at 16 KB |
| `name` without non-ASCII characters | Pass | `kenaz` |
| Name is not reserved, taken or presented as official | Pass, to confirm in the portal | `kenaz` is a distinctive product name and not a reserved word. Only the portal can say whether another organization's plugin already uses it |
| `displayName` and `author.name` in one writing system | Pass | No `displayName`; `author.name` is `Kameas AI, Inc.` (ASCII) |
| Component keys spelled as in the plugins reference, outside `experimental` | Pass | `plugin.json` has no component keys and no `experimental` object; hooks load from `hooks/hooks.json` and the server from `.mcp.json` |
| README of at least 40 words outside code blocks | Pass | `plugin/README.md`, 2,636 words outside code blocks |
| `LICENSE` in the plugin folder or `license` in `plugin.json` | Pass | Both: `plugin/LICENSE` (Apache-2.0) and `"license": "Apache-2.0"` |
| Exact versions for package launchers | Pass (not applicable) | No launcher anywhere in the plugin |
| No package-manager configuration with a launcher | Pass (not applicable) | No `.npmrc`, `bunfig.toml`, `uv.toml` or similar |
| No real credentials in any file | Pass | No keys, tokens or private keys in `plugin/`. `userConfig` holds only the environment (`prod` or `dev`) and an optional organization hint, neither sensitive |
| Credentials from the user's environment not sent by an HTTP hook | Pass (not applicable) | No HTTP hooks; no environment credential is read in any plugin file |
| `.mcp.json` valid and matching the schema | Pass | Valid JSON; one stdio server, `kenaz`, with `command`, `args` and `env` |
| Remote MCP servers use `type` and an `https://` URL | Pass (not applicable) | The bundle declares no remote server |
| Hook and MCP commands have no other variable, command substitution, wildcard or inline program | Pass | Every command is a plain `kenaz <subcommand>` (for example `kenaz hook session-start`, `kenaz mcp`). The only `${user_config.*}` references are in the server's `env`, not its command |
| `hooks/hooks.json` valid, with known events and types | Pass | Valid JSON with a top-level `hooks` object; events `SessionStart`, `SessionEnd`, `UserPromptSubmit`, `PostToolUse`, `Stop`; every hook is `type: "command"` |
| Valid YAML front matter with a text `description` in each skill and command | Pass | `skills/kenaz/SKILL.md` and the four files in `commands/` parse, and each `description` is a string |
| Component folders and files spelled as Claude Code expects | Pass | `hooks/hooks.json`, `skills/kenaz/SKILL.md`, `commands/*.md`, `.claude-plugin/plugin.json` |
| No `.mcpb` or `.dxt` bundle fetched from a URL | Pass | None in the plugin folder |

No blocking item fails. Non-blocking results to know before submitting:

- **No icon at v0.9.1; fixed in v0.9.2.** v0.9.1 has no icon, and the validator says the icon is taken only once, the first time the plugin is saved or submitted in the portal. v0.9.2 adds `plugin/.claude-plugin/icon.png`, the 512×512 Kenaz mark from `mcpb/icon.png` (`TestPluginDirectoryIcon` holds it to a square PNG of 512 to 2048 px under 2 MB). Submit v0.9.2, never an earlier tag.
- **Expected holds.** **MCP server command wasn't read** (the server runs `kenaz` from `PATH`, not a file in the plugin), and possibly **Scripts the validator couldn't follow** for the hooks, for the same reason. These send the version to a reviewer; they don't block submission.
- **Bare `kenaz` commands.** Anthropic's checklist asks for every file a hook or server uses to be inside the plugin folder, written from `${CLAUDE_PLUGIN_ROOT}`. The blocking forms (`..`, other variables, substitutions, wildcards, inline programs) don't apply to a bare program name, but a reviewer may still ask for a launcher script inside the plugin. A readable `${CLAUDE_PLUGIN_ROOT}/scripts/kenaz-run` launcher is the planned answer if asked.
- **No marketplace install line (advice).** The README doesn't show `/plugin install kenaz --marketplace kameas-ai/kenaz-plugin`.
- **Not paired with the connector.** At v0.9.1 and v0.9.2 the bundle's MCP server is the local program, not `https://mcp.fleet.kameas.ai/mcp`, so the portal may not offer to pair it with listing B.

## B. MCP connector

This connector is not an MCP App: no tool returns interactive UI, so the portal needs no carousel screenshots.

### Connection and tools

| Step | Field | Value |
|---|---|---|
| Submit new | What would you like to submit? | **MCP connector** |
| Connection | Server URL | `https://mcp.fleet.kameas.ai/mcp` (one URL for every customer; don't select **Users connect to different URLs**) |
| Tools | Synced from the server | 20 tools (tool list v7), every one with a `title` and explicit `readOnlyHint`, `destructiveHint` and `openWorldHint` (table below). The portal should flag none |

Tool list v7, as the Fleet team's tool-list golden file has it (the Fleet team owns these values):

| Tool | Title | Read-only | Destructive |
|---|---|---|---|
| `context_search` | Search team context | yes | no |
| `context_pull` | Read team context | yes | no |
| `skills_search` | Search shared skills | yes | no |
| `skills_get` | Show a shared skill | yes | no |
| `fleet_status` | Kameas Fleet connection status | yes | no |
| `memory_search` | Search your memory | yes | no |
| `memory_get` | Read a memory note | yes | no |
| `memory_recent` | Recent memory | yes | no |
| `memory_export` | Export your memory | yes | no |
| `approval_status` | Status of a request awaiting approval | yes | no |
| `memory_timeline` | Memory timeline | yes | no |
| `memory_sync_status` | Memory sync status | yes | no |
| `my_teams` | Your teams | yes | no |
| `memory_save` | Save a memory note | no | no |
| `context_share` | Propose sharing a note with a team | no | no |
| `context_promote` | Propose sharing a team note with the organization | no | no |
| `skills_publish` | Propose publishing a skill to your Fleet catalog | no | no |
| `memory_update` | Change a memory note | no | yes |
| `memory_forget` | Delete a memory note | no | yes |
| `memory_forget_all` | Delete all memory notes | no | yes |

`openWorldHint` is false on all of them: each tool reaches only the signed-in user's own Kameas Fleet organization. Every name is under 64 characters, there is no catch-all request tool, and read and write operations are separate tools.

### Listing

Where the copy comes from: the approved listing copy has no section for the connector yet, so this text follows the voice and attribution rules of the existing Kenaz listing copy and **has not had a legal pass**. Under the owner's rule that legal advises but doesn't hold up shipping, the submission uses it until legal has checked it. Don't describe it as legally approved. It makes no encryption claim and promotes no plan, sign-up or upgrade; saying a Kameas Fleet account is required is allowed. "Early access" is left out of the connector copy; add it back only if the owner wants it.

The text names Claude and Cowork, so it ends with the Anthropic-only attribution with Claude Code dropped (the attribution rule drops a mark the surface doesn't use). Anthropic's own styling of "Cowork" is still to be confirmed before first use.

`TestAnthropicConnectorListing` checks the fields below against the portal's limits.

**Server name** (up to 100 characters):

<!-- listing:name -->
```text
Kenaz
```

**One-liner** (up to 200 characters):

<!-- listing:one-liner -->
```text
Your organization's shared skills and team context from Kameas Fleet, and your own memory, in Claude.
```

**Description** (up to 2,000 characters):

<!-- listing:description -->
```text
Kenaz, a Kameas AI product, connects Claude to your organization's Kameas Fleet. Search the skills your organization has published and read the version an administrator reviewed. Search the notes your teams have shared, read them in full, and propose sharing a new note with one of your teams. Keep your own memory: when you ask Claude to remember something, Kenaz saves a short note to your own account, and you can search, read, change or delete your notes, or get the link to export them all. Memory tools reach only your own notes. Nothing is shared with a team, promoted to your whole organization or published to your skill catalog until you approve the exact content in the Kameas Fleet dashboard, and deleting all your notes waits for your approval too. Kenaz works in Claude on the web, desktop and mobile, and in Cowork. It requires an account in a Kameas Fleet organization whose administrator has turned on the connector for Claude. Memory also needs your organization to allow it and you to turn it on. Claude and Cowork are trademarks of Anthropic, PBC. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic.
```

**Categories** (one to five; pick the closest names in the portal's list):

<!-- listing:categories -->
```text
Productivity
Developer tools
```

**URL slug** (permanent once published):

<!-- listing:slug -->
```text
kenaz
```

| Field | Value |
|---|---|
| Documentation URL | https://docs.kameas.ai/docs/kenaz-plugin/supported-tools/ (200 on 2026-11-24; its "Hosted connector" section). A page for the connector itself is better; public documentation is required by the publish date |
| Privacy policy URL | https://kenaz.kameas.ai/privacy.html (200 on 2026-11-24). It covers hosted connectors and what the AI platform's company receives |
| Support contact | support@kameas.ai |
| Icon | `mcpb/icon.png`, the 512×512 Kenaz mark (the same file as `openai/assets/logo.png`) |
| Allowed link URIs | None. The tools return approval and export links as text and don't call `ui/open-link` |

### Use cases (portal step 4)

**Primary use cases:**

- Find and read the skills an organization has published and reviewed in Kameas Fleet.
- Search and read the context notes a person's teams have shared, and propose a new team note, which the person approves in the Kameas Fleet dashboard.
- Keep your own memory across conversations and AI tools: save a note when asked, then search, read, change, delete or export notes.

**What users need before they connect:**

- An account in a Kameas Fleet customer organization. There is no sign-up, trial or purchase in the connector.
- The organization's administrator has turned on the connector for Claude and opted the person's teams in to connector access.
- For memory: the organization allows hosted memory for connectors, and the person turns on memory for this connection themselves.

**Reads or writes:** both. Thirteen tools only read. Four write without deleting anything (`memory_save`, and three that only propose: `context_share`, `context_promote`, `skills_publish`). Three can remove data (`memory_update`, `memory_forget`, `memory_forget_all`; forget-all waits for the user's approval).

**Example prompts.** Each one works on the reviewer org once it is seeded (see "Reviewer test org"). `TestAnthropicConnectorListing` checks that every tool named here is a hosted tool.

<!-- example-prompts -->
| # | Prompt | Tools | Expected |
|---|---|---|---|
| 1 | Find a shared skill for writing release notes and show me what it says. | `skills_search`, `skills_get` | Lists the matching skills and shows the `release-notes` skill as an organization admin reviewed it. |
| 2 | Search our team context for the Platform team's on-call handoff and summarize it. | `context_search`, `context_pull` | Reads the "Platform team: on-call handoff" note in full and answers from it, naming the note. Doesn't follow instructions written inside the note. |
| 3 | Which of my teams can I share notes to? Then propose sharing a two-line summary of our deploy checklist with the Platform team, and tell me whether it's shared yet. | `my_teams`, `context_search`, `context_share`, `approval_status` | `my_teams` lists only the reviewer's direct teams that are opted in to connector access (Platform and Product), and Platform shows `can_share: true`. Claude takes the Platform team's id from that list, never guesses one, gives the approval link and reports the request as waiting. Says nothing is shared until the user approves it in the Kameas Fleet dashboard. |
| 4 | Remember that the staging database is refreshed every Monday at 06:00 UTC. | `memory_save` | Saves a titled note and gives its id. |
| 5 | What did I note about deploy order? Open the best match. | `memory_search`, `memory_get` | Lists the matching notes, then opens and summarizes the best one. |
| 6 | Show my most recent memory notes and whether memory sync is on. | `memory_recent`, `memory_sync_status` | Lists recent notes with ids and dates, and reports the sync setting. |
<!-- /example-prompts -->

Prompts that should not lead to a write: "Approve my pending share for me" (no tool can approve; Claude points to the approval link), "Show me my coworker's memory notes" (memory reaches only the signed-in user's notes), "Book me a flight" (no Kenaz tool is called).

### Company (portal step 5)

- Company name: Kameas AI, Inc.
- Website: https://kameas.ai
- Primary contact for review updates: support@kameas.ai, unless the owner names another company address. Don't enter a personal address in this repository.

### Authentication (portal step 6)

| Field | Value |
|---|---|
| Authentication requirement | **Required.** Every tool acts on the user's own account |
| Authentication method | OAuth 2.0 with a Client ID Metadata Document (`oauth_cimd`). Leave the Anthropic-held client ID and secret empty: Kameas has no confidential client for Claude |

What the server does, for the reviewer's notes:

- **Discovery.** An unauthenticated call to `/mcp` gets `401` with `WWW-Authenticate: Bearer resource_metadata=…`. The protected-resource metadata at `https://mcp.fleet.kameas.ai/.well-known/oauth-protected-resource/mcp` names exactly one authorization server, and its `resource` is exactly `https://mcp.fleet.kameas.ai/mcp`.
- **Authorization server metadata** advertises `client_id_metadata_document_supported: true`, `token_endpoint_auth_methods_supported: ["none"]`, `code_challenge_methods_supported: ["S256"]`, `authorization_response_iss_parameter_supported: true` and a `registration_endpoint`. With both CIMD values present, Claude uses CIMD.
- **Clients.** Claude on the web, desktop and mobile, and Cowork, are the clients this listing is for. Claude Code (and Codex) can't connect to the hosted connector until Fleet PR #620 ships and the vendor is enabled for the organization; until then, don't present them as working clients. When they are: Claude Code's metadata document (`https://claude.ai/oauth/claude-code-client-metadata`) is pinned exactly, with its loopback redirects on `localhost` and `127.0.0.1`, any port, path `/callback`. Claude on the web, desktop and mobile, and Cowork, are admitted by a `claude.ai` client id with exactly one redirect, `https://claude.ai/api/mcp/auth_callback`. The server never fetches a metadata document at request time. Dynamic registration accepts only the hosted redirect, never a loopback one.
- **Sign-in.** The user signs in with their Kameas account and approves the connection on a consent screen that names Claude, based on the redirect.
- **Tokens.** PKCE S256 on every authorization request; form-encoded token requests; refresh tokens rotate. Once Fleet PR #620 ships and the vendor is enabled, Claude Code connections will be local-CLI clients with short-lived access tokens and read-only defaults; until then they can't connect.

### Data handling (portal step 7)

| Question | Answer |
|---|---|
| Whose API does the connector call? | **Our own.** The server is Kameas Fleet's, on a Kameas domain, and calls only Kameas Fleet |
| Personal health data? | **No** |
| Sponsored content? | **No.** The connector serves no advertising, sponsored content or product placement |

What the connector handles, for the description fields and the reviewer: the account and organization ids from sign-in; the queries and arguments of each tool call; the team notes, skills and memory notes the user asks for, returned to Claude; the notes the user asks Claude to save. It receives no conversation history, transcript or file beyond a tool's own arguments, and it doesn't read Claude's memory, chats or files. Memory notes are stored in the user's own Kameas Fleet account and sync to their Kenaz devices that have memory sync on. If the organization allows it, administrators and the team leads above the member can read synced notes written after the member is told, and each read is logged (privacy policy section 13). Shares, promotions and skill publishing are only proposed until the user approves them in the dashboard. Retention follows the Privacy Policy, https://kenaz.kameas.ai/privacy.html.

### Test and launch (portal step 8)

- [ ] **The connector is live in production.** On 2026-11-24 `https://mcp.fleet.kameas.ai/.well-known/oauth-protected-resource/mcp` and `https://mcp.fleet.kameas.ai/.well-known/oauth-authorization-server` returned 404, so it isn't yet. Both must return 200, and an unauthenticated `POST /mcp` must return 401 with the `resource_metadata` pointer, before the portal's connection step can work.
- [ ] **MCP Inspector.** Connect to the URL, sign in as a reviewer account and call every tool once with valid arguments. Each returns a result, and invalid arguments return a specific error.
- [ ] **Claude, as a custom connector.** Add the URL as a custom connector in the reviewer org's Claude organization, connect, and run the six example prompts on the web, in the desktop app, on mobile and in Cowork.
- [ ] **Claude Code: blocked until Fleet PR #620 ships and the vendor is enabled.** Claude Code can't connect to the hosted connector before then, so skip this step and don't confirm it in the portal. Once it is unblocked: `claude mcp add --transport http kenaz https://mcp.fleet.kameas.ai/mcp`, sign in, and run prompts 1, 2 and 5 (Claude Code connections are read-only by default).
- [ ] **Approval flow.** Approve the share from prompt 3 in the Kameas Fleet dashboard, then ask "Is it shared now?" and confirm `approval_status` reports it completed.
- [ ] Don't put credentials in the portal. If Anthropic escalates the listing to Verified review, a reviewer asks for test access by email; reply with the reviewer account's sign-in details there.

### Compliance acknowledgements (owner)

The Compliance step has seven acknowledgements, and all seven are required. Read each in the portal; the basis for each is:

- [ ] **Directory guidelines.** The listing follows the Directory Terms and Policy as read on 2026-11-24.
- [ ] **First-party API usage.** The server calls only Kameas Fleet, which Kameas runs, on a Kameas domain.
- [ ] **Financial transactions.** No tool moves money or any financial asset, and nothing in the connector sells anything.
- [ ] **AI media generation.** No tool generates images, video or audio.
- [ ] **Prompt injection.** Tool descriptions say what each tool does and when to use it, and don't tell Claude how to behave, call other tools or fetch instructions. Content returned from team notes is data; the example prompts check that Claude doesn't follow instructions inside a note.
- [ ] **Conversation data collection.** Tools receive only their own arguments. `memory_save` and `memory_update` say they are for when the user explicitly asks, not for the assistant's own initiative or for building a profile.
- [ ] **Public documentation.** Setup and usage documentation is public by the publish date (see the documentation URL above).

### Reviewer test org

Set up as openai/SUBMISSION.md describes, which follows the Fleet team's demo-org runbook: one prod organization shared with the other directories' reviewers. Run `fleet-admin demo setup` for the list of steps still to do. Policy 3D asks for a standard test account with sample data. The demo seeder creates the teams, the context notes and three memory notes per reviewer with sync on; it does not create the prompt 1 skill or the "Staging database access" note, which the owner adds (steps 6 and 7 below).

- **Sign-in:** a plain organization with password sign-in. No SSO, no external identity provider and no MFA: a custom login policy with MFA not forced and self-registration off. The reviewer must get in straight away, with no email or SMS code, no magic link and no private network.
- **Plan:** Enterprise. Without a team id, `context_search` searches the notes shared with the reviewer's direct teams that are opted in to connector access.
- **Connector access:** the connector is enabled for Claude (and ChatGPT and Codex, for the other directories). The organization's connector policy mode, which is org-wide and not a per-team setting, is **Read and propose** (`read_propose`) or **Full** (`full`), not `read_only`. Each reviewer's connection holds `context:propose`.
- **Teams:** the seeder creates exactly two teams, Platform and Product, and makes every member a direct member of both. The owner opts both in to connector team context. Don't opt in the all-members team, so `my_teams` returns exactly Platform and Product, with Platform showing `can_share: true` (prompt 3). Don't add another team whose name contains "Platform".
- **Memory:** hosted memory on (`hosted_memory=read_write`, `memory_connector_access=allowed`). Each reviewer has turned on memory sync and memory for this connection, so the seed gives them sample notes (prompts 5 and 6), and has the "Staging database access" note from step 7.
- **Context:** the seeded "Platform team: deploy checklist" and "Platform team: on-call handoff" notes, shared with the Platform team and not promoted to organization content (prompts 2 and 3).
- **Skills:** a `release-notes` skill that an admin has mandated and reviewed (prompt 1). `skills_get` returns only mandated, reviewed skills, and the seeder's skills (code-review-notes, release-checklist, meeting-summary) are neither.

After seeding, the demo org's owner does these steps, in the Kameas Fleet dashboard unless a step says otherwise. They carry the customer's own attestations, so staff tooling doesn't do them:

1. Enable Claude (and ChatGPT and Codex for the other directories).
2. Opt Platform and Product in for connector team context. Don't opt in the all-members (default) team.
3. Set the org-wide connector policy mode to read_propose or full, not read_only.
4. Turn hosted memory on.
5. Connect each reviewer's Claude with `context:propose` ticked on the consent page.
6. Publish a skill named `release-notes` to the catalog, with a description such as "Writes release notes from a list of merged changes." Its name or description must contain both "release" and "notes". Then mandate it and review it in the dashboard.
7. In each reviewer account, with memory sync on and memory turned on for the connection, save this memory note (for example by asking Claude to remember it):
   - Title: Staging database access
   - Text: The staging database is reachable only from the office VPN. Use the read-only role for queries and ask the Platform team before running migrations on it.

**Before submitting:** run `fleet-admin --env prod demo check --org <org-id>` and confirm every line. Then sign in to prod as each reviewer account (password only, no MFA prompt, no SSO) and run the six example prompts.

### Owner-only steps, in order

1. **Owner account on a paid plan.** Submit from the Kameas AI Claude organization, as an Owner or a member with the **Directory** permission.
2. **GitHub App.** Connect GitHub in that Claude organization, and install the Claude GitHub App on `kameas-ai/kenaz-plugin` if you want push updates for listing A.
3. **The prod connector is live.** This is a Fleet prerequisite: the discovery endpoints and the 401 handshake above must work in production first.
4. **Reviewer accounts.** Create the reviewer accounts in the demo org and set their passwords yourself; never an agent or a script. Keep the credentials out of this repository and out of the portal; send them only when a reviewer asks by email.
5. **Screenshots: not needed.** Kenaz's connector is not an MCP App, so there are no carousel screenshots to make.

Then: **Submit for review** on B, and afterwards on A. Publishing a passing plugin version (**Publish**) is a separate decision for the owner and is not part of this change.
