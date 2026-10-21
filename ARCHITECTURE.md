# Architecture and ownership

## The seam (`fleetapi/`)

All Kameas Fleet I/O goes through `fleetapi.Provider`. Public code builds every payload. The provider adds only transport and auth, and may not add, rename or enrich payload fields. There are two implementations:

- `fleetapi/stub`: used here. Every call returns `ErrUnofficialBuild`.
- the official adapter: in the proprietary Fleet connection module. Only official builds link it, by calling `app.Main(adapter.New(...))`.

Both must pass `fleetapi/fleetapitest`. `fleetapi/allowlist` is the closed set of keys the public layer may emit. Official builds' CI fails on anything else. Extending it changes what is sent, so it needs review by Kameas and an update to the published telemetry disclosure.

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
| `internal/mcp/tools/<group>/` | MCP tools: `status`, `skills`, `context`, `memory` | One subpackage per tool group. Declare tool names as `Name: "..."`, because the name lint scans that. |
| `internal/otlprx/`, `internal/decode/{attr,claude,codex}/`, `internal/canon/`, `internal/session/`, `internal/project/`, `internal/gate/`, `internal/forward/`, `internal/pipeline/`, `internal/fleetschema/`, `internal/fixtures/` | telemetry | receiver → decode → aggregate → project → gate → encode (`pipeline` wires them; the daemon owns queueing/spool and drives `forward`). `gate.Decider` is implemented by `internal/consent`. `fixtures` are synthetic test exports. The device enforces the projector's closed allowlist; Kameas Fleet re-checks every record on arrival. |
| `internal/consent/` | consent | opt-ins, notice, holds for pending Fleet items, pause and lockdown |
| `internal/skills/` | skills | materializer (`.kameas-managed` marker) |
| `internal/setup/` | setup | Harness config writers (Claude `env` merge preserving order and all other keys; Codex format-preserving `[otel]` line editor), diff+confirm, backups, `--undo` (byte-identical when the file is unchanged since setup; otherwise reverts only the owned keys), `print-managed`, `setup doctor` checks. It never writes outside the owned keys. |
| `internal/memory/` | memory | On-device memory store: one SQLite file (pure Go, FTS5) per OS user at `$STATE_ROOT/memory/memory.db`, not per Fleet env. It imports nothing that talks to Kameas Fleet (test-enforced); the `memory_*` tools, `kenaz memory` and the SessionStart index read and write it locally. |
| `internal/hook/` | plugin | SessionStart/SessionEnd. It must never emit permission decisions. |
| `internal/daemonstate/` | core (read by setup and hooks) | Read-only view of `$STATE/daemon.json`, per-env state paths, and the proposed loopback control contract (`/healthz`, `/control/session-end`, `X-Kameas-Nonce`). `internal/daemon` is the only writer and adopts or amends the contract documented in the package. |
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
- **Consent gate:** `consentDecider` adapts `consent.Manager` to `gate.Decider` from a cached `OrgState`, so the gate does no I/O. The send rule: the member is signed in and has personally opted in under the current notice.
- **Spool:** held records (tag `R`) and unsent batches (tag `B`) share the encrypted spool lane `telemetry`. Drain re-gates held records and resends batches.
- **Hooks:** they use the daemon client (HMAC challenge-response). Setup resolves `$STATE` through the config package so setup, hook and daemon agree on `daemon.json`.
- **Release hooks:** official builds set `app.ExtraLicenses` and the provenance link-time variables. `version --json` and `licenses` are public CLI surfaces. `scripts/verify-release.sh` checks a binary against a public tag.

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
That is the user's explicit choice, so the tools also keep their own
`user_confirmed` guard.

The Codex `.codex-mcp.json` drops the Claude `${user_config.*}` env entries
(Codex would pass them literally). The relay then uses `KENAZ_ENV`
from the user's environment, or `prod`.

`make check-no-policy` checks the overlay. The manifests must match, the
server launch must match, the only allowed tool setting is
`approval_mode = "prompt"`, and only for tools that exist. It rejects tool
allow and deny lists. `scripts/test-checks.sh` seeds each violation.
