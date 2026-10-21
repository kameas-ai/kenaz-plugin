---
name: kenaz
description: Use when the user asks about Kameas Fleet — signing in, telemetry/consent status, finding or sharing skills from their organization's catalog, or searching and sharing team context — or about Kenaz's on-device memory (saving and finding notes across sessions). Explains which kenaz MCP tools to call and what they never do.
---

# Kenaz for Claude Code and Codex

Kenaz connects this session to the user's organization's Kameas Fleet. Use the `kenaz` MCP tools:

- **Status** — `fleet_status`: sign-in, telemetry opt-ins, pending notices, org pause or lockdown. Call it first when something seems off.
- **Skills** — `skills_search`, `skills_install`, `skills_uninstall`; `skills_publish` shares a local skill and always asks the user first.
- **Team context** — `context_search`, `context_pull`; `context_share` / `context_promote` share content and always ask the user first.
- **Memory (on this device only)** — `memory_save`, `memory_search`, `memory_get`, `memory_recent`, `memory_forget`. Memory never goes to Kameas Fleet and works signed out. A session may start with a short "Kenaz memory index" of note titles and ids; use `memory_search` / `memory_get` for details.
- **Notices** — pending notices are reviewed and accepted by the user in Kameas Fleet. Never summarize or accept them on the user's behalf.

When to save a memory:

- Save durable learnings that will help in a later session: facts about the project, decisions and their reasons, conventions, and fixes for problems that took effort. Keep notes short, with a clear title.
- Never save secrets, credentials, tokens or personal data (names, emails, customer data). If something might be sensitive, ask the user before saving it. `memory_save` refuses text that obviously contains a credential, but that check is not a guarantee.
- Don't save what is already in the repository or is only true for this session.
- If `memory_save` returns a notice, show it to the user. If memory is turned off, tell the user they can turn it on with `kenaz memory enable`; don't retry.
- Memories you read become part of this conversation like anything else you read.

Ground rules:

- Kenaz reports session metadata only to Kameas Fleet (counts, timings, model names, token usage). It never sends prompts, responses, code, file paths or tool output.
- It never blocks or approves tool calls. If the user asks to restrict what the agent may do, point them to their organization's Kameas Fleet administrator.
- Not signed in? Ask the user to run `kenaz login` in a terminal (Codex) or `/kenaz:login` (Claude Code).
