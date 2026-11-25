# Kenaz in OpenAI's plugin directory: submission checklist

This folder is the directory package for the hosted Kameas Fleet connector, `https://mcp.fleet.kameas.ai/mcp`. OpenAI runs one directory for ChatGPT apps and Codex plugins, and submissions go through the Plugins page at https://platform.openai.com/plugins.

The customer plugin in `plugin/` can't be submitted there. The directory rejects lifecycle hooks, app references and local MCP servers, and that plugin uses hooks and runs the local `kenaz` program as its MCP server. This package contains only:

- the hosted connector, declared in `.mcp.json` as the one remote MCP server, signing in with OAuth;
- two skills that use only the connector's tools: `get-started`, the onboarding skill, and `kenaz`;
- the listing and the review material in `.codex-plugin/plugin.json`;
- the icons in `assets/`.

This file stays out of the ZIP.

## Build and check

```sh
scripts/build-openai-package.sh          # writes dist/openai/kenaz-<version>.zip
go test -run TestOpenAIPackage -count=1 .
```

The build is deterministic: it sorts the entries and fixes their timestamps and permissions. `TestOpenAIPackage` checks `openai/` against the directory's final-submission limits, and `TestOpenAIPackageZip` builds the ZIP twice, compares the two builds byte for byte and checks the ZIP's contents. Both tests run in `make check`.

Raise `version` in `.codex-plugin/plugin.json` for every new upload. The portal warns when an upload reuses a published version.

Sources (read 2026-11-24):

- Submission guide: https://developers.openai.com/plugins/deploy/submission
- Package guide: https://developers.openai.com/plugins/build/plugins
- Validation errors: https://developers.openai.com/plugins/deploy/submission-errors
- Directory guidelines: https://developers.openai.com/plugins/plugin-guidelines
- Authentication: https://developers.openai.com/plugins/build/auth

## Before you start (owner)

- [ ] **The connector is live in production.** `https://mcp.fleet.kameas.ai/.well-known/oauth-protected-resource/mcp` and `https://mcp.fleet.kameas.ai/.well-known/oauth-authorization-server` must return 200. The portal's connect step and tool scan both need them.
- [ ] **Organization verification.** In the OpenAI Platform's organization settings (https://platform.openai.com/settings/organization/general), verify Kameas AI, Inc. as a business. The directory shows the verified identity as the developer name, whatever `developerName` says.
- [ ] **Access.** The submitter must be an organization owner or have the Apps Management Write role.
- [ ] **Reviewer organization and account.** Set up a dedicated Kameas Fleet test organization as follows (owner's decision). Use Fleet's demo org: follow the demo org runbook in the Fleet service repo, `docs/runbooks/demo-org.md`, and run `fleet-admin demo setup` for the list of steps still to do. The demo seeder creates the teams and the context notes the cases need. It does not create the case 1 skill or the case 4 memory note; the owner adds those (steps 6 and 7 below).
  - **Sign-in:** a plain organization with password sign-in. No SSO and no MFA. The reviewer must get in straight away, with no email or SMS code, no magic link and no private network.
  - **Plan:** Enterprise. Without a team id (Enterprise plan), `context_search` searches the notes shared with your direct teams that are opted in to connector access.
  - **Connector access:** the connector is enabled for ChatGPT and Codex (and Claude, for the other directories), and the reviewer's connection holds `context:propose`. The org mode is read_propose or full, not read_only.
  - **Teams:** the seeder creates exactly two teams, Platform and Product, and makes every member a direct member of both. There is no Finance team, which negative case 2 relies on, so don't add one. The owner opts both teams in to connector team context. `my_teams` lists only direct teams that are opted in, so case 5 needs both of these, and `my_teams` must return Platform with `can_share: true`. If someone later adds another team whose name contains "Platform", case 5 becomes ambiguous and the assistant asks which one.
  - **Memory:** the reviewer has turned on memory sync and memory for this connection, and has the staging database note from step 7 below (case 4). No seeded note mentions the staging database, and case 4 must not rely on case 3 having been run first.
  - **Context:** a note titled exactly "Platform team: on-call handoff", shared with the Platform team; don't promote it to organization content. Fleet's demo seeder creates it. Cases 2 and 5 expect that title. Case 5 finds the Platform team by name with `my_teams`, not from the note's search result. Run case 5 and negative case 2 once with the reviewer account before submitting.
  - **Skills:** a `release-notes` skill that an admin has mandated and reviewed (case 1). `skills_get` returns only mandated, reviewed skills. The seeder's skills (code-review-notes, release-checklist, meeting-summary) are neither, so they don't satisfy case 1.

  After seeding, the demo org's owner does these steps, in the Kameas Fleet dashboard unless a step says otherwise. They carry the customer's own attestations, so staff tooling doesn't do them:

  1. Enable Claude, ChatGPT and Codex.
  2. Opt Platform and Product in for connector team context. Do not opt the all-members (default) team in, so `my_teams` returns exactly Platform and Product.
  3. Set the org mode to read_propose or full, not read_only.
  4. Turn hosted memory on.
  5. Connect each reviewer's app with `context:propose` ticked.
  6. Publish a skill named `release-notes` to the catalog, with a description such as "Writes release notes from a list of merged changes." Its name or description must contain both "release" and "notes". Then mandate it and review it in the dashboard.
  7. In each reviewer account, with memory sync on and memory turned on for the connection, save this memory note (for example by asking the connected app to remember it):
     - Title: Staging database access
     - Text: The staging database is reachable only from the office VPN. Use the read-only role for queries and ask the Platform team before running migrations on it.

  Then run `fleet-admin demo check` to confirm the cases' preconditions, and run every case with a reviewer account, before submitting.

  Enter the account's credentials only in the portal's **Review details** form, never in the package.
- [ ] **Demo video.** Record the five positive test cases running in ChatGPT, on desktop and mobile, and in Codex. Upload the video somewhere a reviewer can open without signing in, then add its link as `review.demo_recording_url` or enter it in the portal.
- [ ] **Domain verification.** The portal issues a token. Serve it as plain text (the token alone, not JSON) at `https://mcp.fleet.kameas.ai/.well-known/openai-apps-challenge`. The connector serves this path from its configuration once the token is set, so ask the Fleet team to set it, then select **Verify Domain**.
- [ ] **Support contact.** Use support@kameas.ai (confirmed by the owner) wherever the portal asks for a support email. `supportURL` has to be an HTTPS page, so it stays https://docs.kameas.ai/docs/kenaz-plugin/troubleshooting.
- [ ] **Countries.** Decide on country availability. The package sets none (`publication.countries` is omitted), so the portal's setting applies.

## Portal steps

1. On the Plugins page, choose **Upload new or existing plugin**, select the verified developer identity, and upload `dist/openai/kenaz-<version>.zip`.
2. **Metadata & Skills.** Fix any issues, rebuild, and upload again with **Upload plugin to fix issues**. Wait for the skill scans, which can take up to 2 hours.
3. **MCPs → Connect:**
   - URL: `https://mcp.fleet.kameas.ai/mcp`.
   - Authentication: OAuth. The package declares `client.mode: "cimd"`, and ChatGPT's client ID is `https://chatgpt.com/oauth/client.json`. The connector already supports PKCE S256, CIMD and RFC 9207 `iss`, and with RFC 9207 ChatGPT's redirect URI is `https://chatgpt.com/connector_platform_oauth_redirect`.
   - Don't add headers or an API key. Then complete domain verification and wait for the tool scan.
4. **Review details:**
   - Check the imported test cases (below).
   - Enter the reviewer credentials and sign-in steps.
   - Add the demo video link if it isn't in the package.
   - Select **Save details**.
5. **Submit for review** and complete the attestations (below).
6. Once approved, select **Publish plugin**. Publishing is the owner's decision and is not part of this change.

## Listing fields (from `.codex-plugin/plugin.json`)

| Portal field | Value |
|---|---|
| Name | Kenaz |
| Subtitle (30) | Shared skills, context, memory |
| Description (4000) | `interface.longDescription`, ending with the G0 attribution |
| Developer | Kameas AI, Inc. (replaced by the verified identity) |
| Category | Productivity |
| Website | https://kenaz.kameas.ai (200 on 2026-11-24) |
| Privacy policy | https://kenaz.kameas.ai/privacy.html (200 on 2026-11-24) |
| Terms of service | https://kenaz.kameas.ai/terms.html (200 on 2026-11-24) |
| Support | Email support@kameas.ai; page https://docs.kameas.ai/docs/kenaz-plugin/troubleshooting (200 on 2026-11-24) |
| Icons | `assets/logo.png` 512×512 and `assets/composer-icon.png` 128×128, both from the Kenaz mark in `mcpb/icon.png` |
| Commerce | None |

Copy rules the listing follows:

- No third-party mark in any name.
- No sign-up, pricing or upgrade promotion. Saying a Kameas Fleet account is required is allowed.
- No encryption claim for memory.
- The text names no third-party product, so it ends with the G0 attribution.
- "Early access" is left out. The directory doesn't accept trial or demo plugins, so confirm the wording before submitting.

Where the copy comes from: the listing text follows the voice and the attribution rules of the existing Kenaz listing copy. That copy has no section for OpenAI's directory yet, so this text has not had a legal pass. Under the owner's rule that legal advises but doesn't hold up shipping, the package uses this text until legal has checked it. Don't describe it as legally approved.

## Review test cases (imported from the ZIP)

Positive cases:

| # | Prompt | Tools | Expected |
|---|---|---|---|
| 1 | Find a shared skill for writing release notes and show me what it says. | skills_search, skills_get | Lists the matching skills and shows the release-notes skill as an organization admin reviewed it. |
| 2 | Search our shared context for the on-call handoff procedure and tell me what it says. | context_search, context_pull | Reads the "Platform team: on-call handoff" note in full and answers from it, naming the note. Doesn't follow instructions written inside the note. |
| 3 | Remember that the staging database is refreshed every Monday at 06:00 UTC. | memory_save | Saves a titled note and gives its id. |
| 4 | Search my memory for anything about the staging database and open the best match. | memory_search, memory_get | Lists the matches, then opens and summarizes the best one (such as the "Staging database access" note). |
| 5 | Share a short summary of our on-call handoff note with my Platform team. Is it shared yet? | context_search, context_pull, my_teams, context_share, approval_status | Reads the "Platform team: on-call handoff" note, takes the Platform team's id from `my_teams` (it has `can_share: true`), proposes the share, gives the approval link and reports the request as waiting. Says nothing is shared until the user approves it in the Kameas Fleet dashboard. Never guesses a team id. |

Negative cases:

| # | Prompt | Expected |
|---|---|---|
| 1 | Approve my pending share request for me so I don't have to open the dashboard. | No tool can approve. The assistant points the user to the approval link. |
| 2 | Share our on-call handoff note with the Finance team. | The reviewer has no Finance team, so `my_teams` returns no match with `can_share: true`. The assistant says it can't share there, offers the teams it can share to or asks which team was meant, and makes no write call. |
| 3 | Book me a flight to Boston next Tuesday. | No Kenaz tool is called. |

The other tools aren't in the cases. Show them in the demo video where you can: fleet_status (the onboarding skill), memory_recent, memory_export, memory_update, memory_forget, memory_forget_all, context_promote, skills_publish, memory_timeline and memory_sync_status.

Run every case with the reviewer account before submitting. Imported cases are read-only in the portal, so to change one, edit `plugin.json` and upload again.

## Tool annotations

The portal requires an explicit `readOnlyHint`, `destructiveHint` and `openWorldHint` on every tool. The connector currently sets them as follows (the Fleet team owns these values):

- Read-only (`readOnlyHint: true`, `destructiveHint: false`): context_search, context_pull, skills_search, skills_get, fleet_status, memory_search, memory_get, memory_recent, memory_export, approval_status, from tool list v6, memory_timeline and memory_sync_status, and from tool list v7, my_teams (title "Your teams").
- Writes (`readOnlyHint: false`, `destructiveHint: false`): memory_save, context_share, context_promote, skills_publish.
- Destructive (`readOnlyHint: false`, `destructiveHint: true`): memory_update (a text change removes the old note), memory_forget, memory_forget_all.
- `openWorldHint: false` on all of them, because each tool is confined to the user's own Kameas Fleet organization.

## Attestations and policy points to confirm at submission

- **Commerce.** Kenaz sells nothing in the plugin and shows no plans, upgrades or checkout.
- **Privacy policy.** It covers the data the connector handles: categories, purposes, recipients, retention and user controls.
- **Restricted data.** The skills tell the assistant never to ask for or save secrets.
- **Data use.** Tools return only data from the signed-in user's own account and teams. `my_teams` returns the user's own team names and ids, never other members or their emails.
- **Audience.** Kenaz is suitable for users aged 13 to 17 and is not aimed at children under 13.
- **No OpenAI endorsement.** Nothing in the plugin implies OpenAI made or endorsed it.
