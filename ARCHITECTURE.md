# Architecture and ownership

## The seam (`fleetapi/`)

All Kameas Fleet I/O goes through `fleetapi.Provider`. Public code builds every payload. The provider adds only transport and auth, and may not add, rename or enrich payload fields. There are two implementations:

- `fleetapi/stub`: used here. Every call returns `ErrUnofficialBuild`.
- the official adapter: in the proprietary Fleet connection module. Only official builds link it, by calling `app.Main(adapter.New(...))`.

Both must pass `fleetapi/fleetapitest`. `fleetapi/allowlist` is the closed set of keys the public layer may emit. Official builds' CI fails on anything else. Extending it changes what is sent, so it needs review by Kameas and an update to the published telemetry disclosure.

### Resource attributes on every telemetry request

The relay stamps these on each resource group of an OTLP logs or traces request (`internal/project/encode.go`); none is copied from a harness. The list is `fleetschema.ResourceAttrs` and `ClientResourceAttrs`, exported as `allowlist.Telemetry().ResourceAttributes` and pinned in `fleetapi/allowlist/allowlist_test.go`.

| Attribute | Value |
|---|---|
| `kameas.user.id`, `kameas.org.id` | the signed-in account and its organization |
| `kameas.machine.id` | the relay's node id |
| `kameas.tier` | the plan tier, when known |
| `service.name`, `service.version` | `project.ServiceName` and the relay's version |
| `kameas.client.kind`, `kameas.client.version` | the harness, from `telemetry_schema` 2 on |
| `kameas.batch.id` | a random id for the request, see below |

`kameas.vm.id` is allowlisted but not emitted.

**`kameas.batch.id`** is 32 lowercase hex characters from 128 bits of `crypto/rand`, drawn once when a request body is serialized. Two distinct batches can hold identical records (two agent processes that each export the same tool call at the same instant), and log records carry no id of their own; the batch id keeps their bytes apart, so that Fleet can treat a byte-identical body as a replay.

- **Random per batch.** Every `project.Encode` call draws a new id for each request it returns; when it splits records into several requests, each gets its own. All resource groups of one request carry the same id.
- **Stable across retries.** A batch that could not be sent is spooled as its serialized body and sent again unchanged, so a retry carries the same id. Nothing encodes a stored body again. Records that were held before they were ever encoded get their id when they are encoded.
- **Not an identifier of the user or device.** It is not derived from the account, device, session, time or content, and it says nothing about them. Fleet does not store it.
- **No fallback.** If the random source fails, encoding panics, as it does for span ids. An empty, constant or all-zero id is never sent.

**Never** import the proprietary module from this repo, add a build tag that references it, or add policy (permission decisions, Cedar, allow/deny lists) anywhere. `make check-no-policy` enforces the last of these.

## Package layout and owners

| Path | Area | Notes |
|---|---|---|
| `app/`, `cmd/kenaz/` | core | CLI dispatch (stdlib `flag`). Extend the `commands` table in `app/main.go`. |
| `internal/config/` | core | env, paths, `$STATE` |
| `internal/daemon/` | core | singleton on-demand relay daemon: started by `kenaz mcp` and the hooks, exits when idle. Lanes implement Start/Stop/Hold/Purge. |
| `internal/enroll/`, `internal/device/` | core | enroll/heartbeat body; member-controlled device display name |
| `internal/spool/` | core | encrypted bounded queue |
| `internal/credstore/` | core | owner-only credential files and the `STORE` marker that records keychain or files for a device with no OS keychain |
| `internal/clock/`, `internal/logx/` | core | injectable clock; redacting slog and rotating `relay.log` |
| `internal/mcp/` | core | stdio MCP server and registry |
| `internal/approvals/` | approvals | Human-approved writes: pending-write store (CAS state machine), the loopback approval page (`web/`, embedded) with Host/Origin/Sec-Fetch/CSRF guards, `kenaz approve` through those same routes, and the nonce-authenticated control API (prepare/status/list only). One exception to a person approving each write: an operation can register an `AutoApprover`, which the relay asks about a newly prepared request before `Prepare` answers; on a yes the relay claims and runs the request as an approval does. Only the context share grant registers one (see `internal/contextlib/`). The record keeps `approved_by: "grant"`. |
| `internal/mcp/tools/<group>/` | MCP tools: `status`, `skills`, `context`, `memory`, `approvals` (read-only `approval_status`) | One subpackage per tool group. Declare tool names as `Name: "..."`, because the name lint scans that. |
| `internal/otlprx/`, `internal/decode/{attr,claude,codex,opencode}/`, `internal/canon/`, `internal/session/`, `internal/project/`, `internal/gate/`, `internal/forward/`, `internal/pipeline/`, `internal/fleetschema/`, `internal/fixtures/` | telemetry | receiver → decode → aggregate → project → gate → encode (`pipeline` wires them; the daemon owns queueing/spool and drives `forward`). `gate.Decider` is implemented by `internal/consent`. `fixtures` are synthetic test exports. The device enforces the projector's closed allowlist; Kameas Fleet re-checks every record on arrival. |
| `internal/consent/` | consent | opt-ins, notice, holds for pending Fleet items, pause and lockdown |
| `internal/skills/` | skills | materializer (`.kameas-managed` marker) |
| `internal/contextlib/` | context files | Local context file library behind the `kenaz_context_library` build tag: `~/.kenaz/contexts/` and `<repo>/.kenaz/context/`, scanned on demand. Same file rules as the harness library. It imports nothing that talks to Kameas Fleet (test-enforced). Project and team files are untrusted and never loaded eagerly; `context_list`, `context_read`, `kenaz context list` and the SessionStart index read it; `context_write` saves to your own files, never a repository's. `context_share_file` (in `internal/mcp/tools/context`) shares one file with a team through the approval page; a ledger under the state root ties each file to its Fleet node, per account, so a file never reaches another account's teams. **The context share grant** (`grant.go`; `kenaz context grant on|off|status` in `app/contextgrant.go`) changes the rule that a person approves the exact text of every context share an agent prepares: while a member has it on, the relay runs a prepared `context_share_file` request without the page, through the approval's own claim and the same executor. It is off by default. Only the terminal command gives it (a real terminal and a typed `y`); no MCP tool, control route, hook, environment variable or config key does (test-enforced). The grant is a file under `$STATE/context-share-grant/`, bound to one account and sealed with an HMAC keyed from three keychain items: the approvals key, a random id made at `grant on` and deleted when the grant ends, and the environment's spool key, which every release's logout deletes. So a grant file written without the keychain, or copied and put back after `grant off` or a sign-out, is not a grant. That stops an agent limited to the plugin's tools or to writing files; it does not stop a program running as the user that can read the keychain or drive a terminal, which can already approve a single request. Logout, a relay purge and another account signing in end the grant; logout and purge also delete its history. The relay decides inside `Prepare`, before it answers (`AutoApproveShareFile` in `internal/mcp/tools/context/grant.go`), so a `pending` reply always means nothing will run without the person; only the executor runs after the decision, and a write still running when `Prepare` answers is reported as `executing`. The decision is no when the text is marked as drafted from memory (the mark is the agent's own; unmarked memory-derived text is not detected), when the member isn't in the payload's team, when the payload's path, short name, node, team, version or base text aren't what the ledger holds for the file and account (for an update, a link this member authored), when the relay's own scan finds a credential or hidden text, or past 30 shares in a clock hour; it writes the history entry (short name, team, SHA-256, time) before the share runs. The relay does not re-read the file, so it can't check that the text is the file's. The history is a local, erasable record, not an audit trail. It covers every team the member is in, including the organization's all-members team, so it can publish to the whole organization. `context_share`, `context_promote`, `skills_publish` and memory writes are not covered. `OrgAllowsShareGrant` is where a Fleet setting will be able to forbid it; today it allows and calls nothing. `context_pull_files` saves team files as working copies under `~/.kenaz/team-contexts/`, a third library labelled untrusted. |
| `internal/setup/` | setup | Harness config writers (Claude `env` merge preserving order and all other keys; Codex format-preserving `[otel]` line editor; OpenCode `plugin` entry and `mcp.kenaz` in `opencode.json`), diff+confirm, backups, `--undo` (byte-identical when the file is unchanged since setup; otherwise reverts only the owned keys), `print-managed`, `setup doctor` checks. It never writes outside the owned keys. |
| `internal/memory/` | memory | On-device memory store: one SQLite file (pure Go, FTS5) per OS user at `$STATE_ROOT/memory/memory.db`, not per Fleet env. It imports nothing that talks to Kameas Fleet (test-enforced); the `memory_*` tools, `kenaz memory` and the SessionStart index read and write it locally. |
| `internal/memory/capture/` | memory | Automatic capture (only in a build with the tag `kenaz_memory_capture`, which the official release sets, and then off until a member turns it on): hook handlers queue scrubbed, truncated events on disk; a detached worker batches them through a writer the member picks (there is no default) into typed notes in the local store. It never reads, stores, copies or forwards a vendor's credentials or session tokens (test-enforced). It imports nothing that talks to Fleet or telemetry (test-enforced). The plugin registers its hooks, which return before reading the session unless the member turned capture on (test-enforced); `make check-no-policy` pins which events run what. See `docs/memory-staff.md`. |
| `internal/hook/` | plugin | SessionStart/SessionEnd. It must never emit permission decisions. |
| `internal/daemonstate/` | core (read by setup and hooks) | Read-only view of `$STATE/daemon.json`, per-env state paths, and the proposed loopback control contract (`/healthz`, `/control/session-end`, `X-Kameas-Nonce`). `internal/daemon` is the only writer and adopts or amends the contract documented in the package. |
| `opencode/` | OpenCode adapter | `@kameas/kenaz-opencode`: dependency-free OpenCode server plugin (plain ES modules, no build step). Observe-only: it maps OpenCode bus events to metadata records and posts OTLP/HTTP JSON to the relay. `make test-opencode` runs its tests (needs Node 18+; not part of `make check`). |
| `plugin/`, `.claude-plugin/` | plugin | Versions in both manifests and the marketplace entry must match. The MCP server and hooks run the installed `kenaz` binary from PATH, never npx (test-enforced). |

## Extension points

- **Daemon:** append an `app.DaemonHook` to `daemonHooks` (in `app/daemoncmd.go`). The hook may add `Lanes` and `Contributors` to `*daemon.Options` and return a mount func. Ingest handlers must be wrapped: `d.Handle("POST /v1/logs", d.RequireSession(h))`. This answers 503 while signed out and counts as activity for idle exit.
- **Lanes** implement `daemon.Lane` (Start/Stop/Hold/Resume/Purge/Status). The consent controller drives Hold/Resume. Logout and node removal call Stop+Purge.
- **Status** is `daemon.Status` (schema_version 1), shared by `status --json` and `fleet_status`. Add fields through a `daemon.StatusContributor`, never by logging.
- **MCP tools:** append a `mcp.Group` to `mcpGroups` (in `app/mcpcmd.go`). The MCP process is thin. State lives in the daemon, which the MCP process reaches through the nonce-authenticated control client.
- **Spool:** `daemon.Options.Spool` is the daemon's single `*spool.Spool`. Lanes append already-projected payloads under their lane name.

## Gates (`make check`)

`fmt-check`, `vet`, `test`, `check-no-policy`, `check-names`, `check-privacy-logs` and `test-checks` (self-test of the gates). Root-package tests enforce the trademark disclaimer and the approved open-core wording.


## Integration wiring

- **`app/telemetry.go` is the only place the daemon, the telemetry pipeline and consent meet.** The daemon hosts the OTLP receiver on the daemon's chosen port.
  - `POST /v1/{logs,traces,metrics}` sits behind `RequireSession`.
  - `POST /control/session-end` sits behind the nonce bearer and is hash-only (`canon.LocalSessionHash`). The receiver's own `/internal/session-end` stays disabled.
- **Session changes reach a running relay.** The provider may load the stored session once per process, so a relay that started signed out would not see a sign-in made by another process.
  - `kenaz login` calls `POST /control/session-changed` (nonce bearer). The relay reloads the stored session through the optional `fleetapi.SessionReloader`, then enrolls and re-runs the consent refresh at once. A relay that cannot reload in place (a provider without the interface, a release without the endpoint, a removed node) answers or fails so that login restarts it instead.
  - As a safety net the relay re-reads the stored session every minute and does the same refresh when it changed, so a sign-in or sign-out whose notice never arrived is still picked up.
  - `kenaz logout` stops a running relay with a purge (`POST /control/shutdown?purge=1`). The reload itself never sends anything: uploads still need sign-in and the member's own opt-in.
- **Consent gate:** `consentDecider` adapts `consent.Manager` to `gate.Decider` from a cached `OrgState`, so the gate does no I/O. The send rule: the member is signed in and has personally opted in under the current notice.
- **Spool:** held records (tag `r`) and unsent batches (tag `o`, stamped with their owning account) share the encrypted spool lane `telemetry`. Records with the tags of earlier builds (`B`, `R`, `b`) are dropped unsent.
  - **Draining never re-posts a delivered batch.** A batch posted twice would be counted twice unless Fleet recognizes the repeat (see below), so the relay does not rely on that. After each record the lane accepts, the spool writes a cursor beside the segment (`<segment>.cur`: a count of delivered records, bound to the segment by its first record's nonce; written to a temp file, fsynced and renamed). A drain that stops part-way (429, 5xx, timeout, a hold, shutdown) and the next relay process both resume after the last accepted record. The segment format is unchanged, and a segment with no cursor drains from the start.
  - **When a batch can still be posted twice.** (1) A crash after Fleet accepted a batch and before its cursor was written: that single batch is sent again, and nothing before it. (2) A post Fleet accepted whose response never arrived (a timeout, or the relay stopping mid-post): the relay cannot tell it from a failure and posts it again. Both re-send the same stored bytes, with the same `kameas.batch.id`, so Fleet's ingest dedup recognizes them: since 2026-11-14 Fleet answers 200 to a request body it has already stored from the same member on the same signal within about seven days, and does not store it again. A Fleet without that dedup counts a batch re-posted in these two windows twice.
  - **A held record is re-gated and encoded once.** Drain runs it through the gates again. What consent no longer allows is dropped. A record that is held again stays where it is and the drain stops. What may be sent is encoded and written back to the spool as `o` batches before the held record is marked done, and every later attempt sends those bytes. This matters because each encoding draws a new `kameas.batch.id` (and new span ids): a second encoding of the same events would look like new data to Fleet.
  - **The write-back is all or nothing.** The batches one held record becomes are written as one new segment under a temporary name (`*.seg.tmp`) and renamed into place (`spool.AppendAll`). If the write fails (a full disk), nothing is queued, the held record is kept and a later drain tries again, so a failure neither loses the record nor queues part of it twice. The segment's name carries the held record's id (`<nanos>-<seq>.w<record id>.seg`), and while it is queued a second write-back for the same record writes nothing. So after a crash between the rename and the cursor, the held record is re-gated again but the bytes queued the first time are the only ones sent. The segment is also named to sort after every segment already queued, even if the wall clock went back, so it is never sent before the held record it came from is met again.
  - **What is durable.** A written-back set is durable before its held record's cursor is written: the temporary file is fsynced, renamed, and the directory is fsynced (not on Windows, where a directory cannot be fsynced), and only then does the drain write the cursor, which is fsynced the same way. Ordinary queued appends are still not fsynced, as before: after power loss a recently queued ordinary batch can be missing.
  - **Order is kept:** a failing batch blocks the batches behind it until it is accepted or dropped.
  - **Telemetry queued for more than 7 days is dropped,** for example after the laptop was off or the organization was paused that long. The limit is applied on every append and at the start of every drain, with the 50 MiB size bound, and what it discards is included in the lane's `dropped` count in `kenaz status` (counted per spool record).
  - Purge, eviction and quarantine remove a segment's cursor with it. A drain sweeps cursors whose segment is gone and half-written `*.seg.tmp` files.
- **Hooks:** they use the daemon client (HMAC challenge-response). Setup resolves `$STATE` through the config package so setup, hook and daemon agree on `daemon.json`.
- **Release hooks:** official builds set `app.ExtraLicenses` and the provenance link-time variables. `version --json` and `licenses` are public CLI surfaces. `scripts/verify-release.sh` checks a binary against a public tag.

## Release-archive installs

`internal/setup/bundle.go` handles the self-contained archive (`bin/`,
`plugin/`, `.claude-plugin/`, `opencode/`, `bundle.json` from the release
packaging). Setup verifies every listed digest and that the plugin,
marketplace and adapter carry the bundle's version, then renders a per-user
plugin copy under `$STATE/setup/bundle/<version>_<os>_<arch>/` with absolute
MCP and hook commands and registers it through the host CLI. The state
record keeps the rendered path and digest; `--undo` runs the host's
uninstall and marketplace removal before restoring the config file (byte
for byte when setup's own write was untouched). The repository's `plugin/`
keeps bare `kenaz` launch commands for PATH installs.

**Release pinning** (`internal/setup/pin.go`): the plugin is pinned to a
signed release, never auto-updated from a branch. The
app passes setup its release (`app.Version`, `app.BuildCommit`). Setup adopts
a GitHub `kenaz` marketplace only when it is pinned to that exact commit
(Codex `ref = "<commit>"`; Claude Code's inline `settings` catalog with a
plugin `sha`); one on the default branch, a branch or a tag is replaced by the
verified bundle copy with a warning, and doctor flags an adopted one that is
not pinned. Codex runs the plugin from the copy it made at install time
(`$CODEX_HOME/plugins/cache/kenaz/kenaz/<version>`), which outlives a
marketplace re-added with `--ref`, so for a pinned Codex marketplace setup
also compares that copy with the clone's `plugin/` and reinstalls the plugin
when they differ; doctor warns. `print-managed` pins to the release too: Claude Code gets an
inline catalog whose `git-subdir` plugin source carries `ref` (the tag) and
`sha` (the commit), with `autoUpdate: false`, because github/git marketplace
sources take a ref but no commit; Codex gets a `[marketplaces.kenaz]` block
with `ref = "<commit>"`. A build with no release commit refuses
`print-managed` unless given `--release` and `--commit`.

## OpenCode adapter

OpenCode does not load Claude Code plugin manifests and the relay does not
rely on an OpenCode-native OTLP exporter. Instead:

- `kenaz setup opencode` adds two owned entries to the user's global
  `opencode.json`: a `plugin` entry `["<adapter dir>", {"state_dir": ...}]`
  and `mcp.kenaz` (the `kenaz mcp` server by absolute path, so the status,
  context, skills and memory tools are available). Nothing else is written.
- The adapter (`opencode/src`) uses only the server-plugin `event` and
  `dispose` hooks. `map.js` is the only module that knows OpenCode's event
  shapes; it reads named fields of `session.created/deleted`,
  `message.updated` (completed assistant messages: model, provider, tokens,
  cost, API error status) and `message.part.updated` (finished tool parts:
  tool id, status, timing). Subagent sessions report under their root
  session.
- `relay.js` reads `daemon.json` from `state_dir`, proves the listener is
  the relay with the `/healthz` challenge-response, and posts to `/v1/logs`.
  The queue is bounded and in memory; a 503 (signed out) drops the batch.
- The relay decodes the records in `internal/decode/opencode` (named
  attributes only), deduplicates retried model/tool calls by a keyed hash of
  OpenCode's ids, and closes the session on `opencode.session.end`.
- Fleet drops resource groups whose `kameas.client.kind` it does not know,
  so OpenCode records are sent only when Fleet advertises
  `telemetry_schema >= fleetschema.SchemaRevOpenCode`; before that the
  pipeline drops them on the device (`dropped_client_unsupported`).

`make check-no-policy` keeps the adapter observe-only (no permission, tool,
chat or command hooks) and dependency-free, and keeps OpenCode tool and
permission keys out of setup.

## One plugin, two manifests

Codex picks the first manifest it finds in this order: root `plugin.json`
(Agent Plugins), then `.codex-plugin/plugin.json`, then
`.claude-plugin/plugin.json` (codex-rs `DISCOVERABLE_PLUGIN_MANIFEST_PATHS`).
Claude Code reads only `.claude-plugin/`. So:

- `plugin/.claude-plugin/plugin.json` + `plugin/.mcp.json`: what Claude Code
  loads. Its write tools force a prompt with
  `_meta["anthropic/requiresUserInteraction"]`.
- `plugin/.codex-plugin/plugin.json`: what Codex loads instead. It mirrors
  the Claude manifest (name, version, description, license) and points
  `mcpServers` at `plugin/.codex-mcp.json`. `skills/` and `hooks/hooks.json`
  are shared through Codex's defaults.
- `plugin/.codex-mcp.json`: the same server launch, plus
  `tools.<name>.approval_mode = "prompt"` for `skills_publish`,
  `context_share` and `context_promote`. Claude never sees these keys.

**Use `prompt`, never `approve`.** In Codex, `approve` means *auto-approve*
(codex-rs `core/src/mcp_tool_call.rs` `requires_mcp_tool_approval_for_mode`
returns false for `Approve`). `prompt` asks on every call, and user policy
can only tighten it (`AppToolApproval::restrict_to`). Caveat: with
`approval_policy = never` and a full-access permission profile, Codex
auto-approves even `prompt` (`codex-mcp` `mcp_permission_prompt_is_auto_approved`).
Host prompts are therefore never treated as proof of confirmation: the
write tools only prepare a request, and the write runs after the user
approves it on the relay's local page or with `kenaz approve` (`internal/approvals`; see
"Approving writes" in the README). The one exception is `context_share_file`
while the member has the context share grant on. A model-supplied `user_confirmed` is
ignored.

The Codex `.codex-mcp.json` drops the Claude `${user_config.*}` env entries
(Codex would pass them literally). The relay then uses `KENAZ_ENV`
from the user's environment, or `prod`.

`make check-no-policy` checks the overlay. The manifests must match, the
server launch must match, the only allowed tool setting is
`approval_mode = "prompt"`, and only for tools that exist. It rejects tool
allow and deny lists. `scripts/test-checks.sh` seeds each violation.
