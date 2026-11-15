# Baseline conformance

The matrix (`matrix.json`) is 24 cells: four features (OTel export, context, private memory, skills) on two hosts (Claude Code CLI, Codex CLI) on three OSes. The 12 OpenCode cells wait in `later/opencode.json`. `compatibility.json` is the lock: the exact host releases, OS builds and artifact digests a candidate is qualified against. Every value in it is still `UNVERIFIED`.

`scripts/conformance.sh` runs the harness (`validate | run | verdict | check-verdict | report`). `make check` runs `validate` and the harness's own tests.

## Two tiers

Each cell is reported in one of four states, by `scripts/conformance.sh report`: **not run**, **installed-tier pass (reduced)**, **installed-tier pass**, or **live pass**.

An installed-tier pass is **reduced** when the journey recorded any step as not run (no host CLI on the runner, a build without the context file tools) or when the runner is not the cell's target OS release. Every step that ran passed, but it is not a full installed-tier pass, and it is never shown as one: the report, `installed-summary.json` (`reduced`, `not_run_steps`, `os_match`, `os_note`, and `summary.pass_reduced`) and the CI job summary all name the steps not run and the runner's OS. `summary.pass` and `-min-pass` count both kinds.

| | Live tier | Installed tier |
|---|---|---|
| What runs | The official, signed candidate on a locked host release, signed in to the development Fleet, driven through the host with a model | This checkout's unofficial `kenaz`, set up for the host in throwaway directories. No sign-in, no model call, no Kameas Fleet |
| Needs | Everything under "What a live pass still needs" | Go, and nothing else. The real host CLI is used if it happens to be on the runner |
| Journey script | `journeys/<feature>.<host>.sh` (`installed_journey` in the matrix). None is written yet | `installed/<feature>.<host>.sh` (`installed_tier_journey`). All eight are written |
| Evidence | `evidence/<cell>.json`, no `tier` field (or `"live"`) | `evidence/installed/<cell>.json`, `"tier": "installed"` and the journey's steps under `installed` |
| Roll-up | `verdict`: the digest-bound release verdict | `verdict -tier installed`: an installed-tier summary, `release_allowed` always false |
| Release gate | The only thing `check-verdict` accepts | Never. See below |

The two tiers do not mix:

- `BuildVerdict` counts live evidence only. An installed-tier record handed to it is an error and its cell is missing, even if the lock is fully locked.
- `check-verdict` first says which tier it found. It accepts a live verdict and refuses an installed-tier summary (`tier found: installed`) or anything else (`tier found: unknown`) before reading the rest.
- A release verdict's bytes are unchanged by the installed tier: it declares no tier, so every released `check-verdict` still reads it.
- The installed-tier scripts live outside `journeys/` on purpose. Writing a file under `journeys/` unblocks the live tier for that cell, so a file there must be a recorded live journey. A test fails if one appears by accident or if an installed-tier journey is named there.

No live cell has passed. Nothing in this directory is a release verdict, and `test/conformance/release-verdict.json` does not exist.

## What the installed tier does

Every journey (`installed/journey.go`), for its feature and host:

1. **Sandbox.** Creates one temporary directory and points `HOME`, `USERPROFILE`, `KENAZ_HOME`, `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, the XDG directories, `APPDATA`, `LOCALAPPDATA` and the temp directory inside it. It refuses to start unless every path it will write is inside that directory and the directory is not your real home, `~/.kenaz`, `~/.claude` or `~/.codex`. Children get no credential or host setting from your environment (`*_API_KEY`, tokens, `ANTHROPIC_*`, `OPENAI_*`, `CLAUDE*`, `CODEX_*`, `KENAZ_*` and the like are dropped). Before `kenaz` runs for the first time it records the file credential store inside the sandbox, so the OS keychain (on a Linux desktop, the Secret Service) is never probed; the journey refuses to run `kenaz` without that record.
2. **Build.** Builds `kenaz` from this checkout with `KENAZ_CONFORMANCE_TAGS` (default: none) and checks `kenaz version --json` says `unofficial`. It refuses any other flavor, and the `fleet` and `kenaz_dev` tags.
3. **Bundle.** Lays this checkout's `plugin/` around the binary the way a release archive does (unsigned), verifies it with the product's bundle loader, and renders the per-user plugin copy: one MCP server (`kenaz`) and the plugin's hooks, all launching the bundle's binary by absolute path, nothing added. This runs on every OS whether or not a host CLI is present.
4. **Relay.** Starts `kenaz daemon` in its own process group. No relay outlives the journey (see "Relays never outlive a journey").
5. **Setup.** The host's config exists first, with a user's own content: unrelated keys, an unrelated key inside the container setup writes to, and one key setup owns holding a value of the user's. Then `kenaz setup <host> --dry-run` (shows the change, leaves the file as it was), then `kenaz setup <host> --yes`. With the host's CLI on the runner, setup registers the plugin through it from the local bundle. Without it, setup runs with `--no-bundle` and writes telemetry keys only, which is what a user without the host gets.
6. **Host config.** The whole file is compared with the file from before setup. The only differences allowed are the telemetry keys setup owns, each with its one exact value (pointing at this relay, content logging off), and, with the host CLI, the plugin's registration. Any other key or table added, changed or removed fails the step. With the host CLI: the marketplace and plugin are registered and enabled from the rendered copy.
7. **Doctor.** `kenaz setup doctor` reports the relay listening and, with the host CLI, the host exporting to it, the plugin installed and the bundle registration, with no warning or failure for the host.
8. **Host visibility**, only with the real host CLI and only with listing commands: `claude plugin list --json`, `claude plugin marketplace list`, `claude plugin details kenaz@kenaz`, `claude mcp list` (the host connects to the server; no model); `codex plugin list`, `codex plugin marketplace list`, `codex mcp list --json`. The journey never signs in and never starts a session. A listing that exits non-zero and asks for sign-in is recorded as "host listing needs sign-in": it does not fail the cell and does not count as verified. Any other non-zero exit fails the step, because a host that cannot list its plugins has not shown that it sees this one.
9. **MCP.** Speaks MCP to the installed binary over stdio with a hand-written client: `initialize`, then `tools/list`. The feature's tools are present for this build's tags, every input schema parses, and tools this build or sign-in state must not serve are absent.
10. **The feature**, signed out (below).
11. **Undo**, twice, because undo has two ways to put the file back. `setup-undo`: the file is as setup left it, so `kenaz setup --undo <host> --yes` restores its snapshot, and the file must be byte-identical to the one from before setup. `setup-undo-after-edit`: setup runs again, the journey edits a key of the user's own, and undo must then restore each key setup owns (the user's earlier value, or its absence) and leave the edit alone, so the file must be byte-identical to the pre-setup file with that edit. Both times the config must no longer point at the relay or register the plugin, and with the host CLI the host's own `plugin list` must no longer show it. One allowance, in the second undo only and only with a host CLI: the host's own writer may have rewritten the file while unregistering the plugin (Claude Code reorders keys and leaves empty plugin maps), so there the file may hold the same content instead of the same bytes, and the step says which it was.

Per feature:

| Feature | Asserted through the installed binary | Left unproven |
|---|---|---|
| OTel export | The relay's loopback receiver answers and refuses the host's synthetic export (logs, metrics, traces; `internal/fixtures`, every content field a canary) with `503 not signed in`. Nothing is queued and no file under the relay's state holds planted content. `fleet_status` reports unofficial, relay running, signed out. **As a Go test, not the installed binary:** `TestConformanceOTelJourney/<host>` sends the same export through the real receiver and pipeline to an in-process fake Fleet and checks the allowlist. The unofficial build's stub provider can never be signed in, so its relay never projects anything outside Go tests | That a host session exports anything. Receipt by Fleet, opt-in and opt-out, retry and deduplication. The projection of a live export by the installed binary |
| Context | `context_search`, `context_pull`, `context_share`, `context_promote` and `team_members` refuse with `unofficial_build` and return no data. With `kenaz_context_library`: a personal file is written, listed as yours, read back with the same digest, stays on the device, and a team file is refused with `not_signed_in`. Without the tag the file tools must be absent and that step is not run | Team resolution, search, read, share, promotion, limits and cross-team refusals. The approval page. Team context files and the share grant, which Fleet must offer to a signed-in member |
| Private memory | A note is saved on this device only, found, listed, read, edited and forgotten, then gone. Sync is reported unavailable, a tool cannot turn it on (`needs_terminal`), forget-all is refused (`approval_unavailable`) | Sync, cross-host and two-device recall. Automatic capture. Recall inside a session with a model |
| Skills | `skills_search` and `skills_install` refuse with the unofficial-build message. Uninstalling a skill that is not installed removes nothing. `kenaz skills` lists nothing installed. With the host CLI: the registered plugin copy carries the plugin's own `kenaz` skill byte for byte | Browse, install, update, mandated materialization, publish and removal. Discovery and invocation of a Fleet skill in a session |

The OTel projection Go test runs on Windows too. It was recorded as not run there until the telemetry spool stopped fsyncing its directory on Windows: that fsync is refused ("Access is denied"), the consent refresh of a signed-in member failed at its first purge, the member was never admitted, and `TestConformanceOTelJourney` stopped at "member never signed in and admitted" (CI run 43561361213 without the guard, 43561363249 with it).

For every cell the installed tier also leaves unproven: the official signed artifact (an official build links the proprietary Fleet client, answers Fleet-backed tools with its sign-in remedy rather than `unofficial_build`, accepts telemetry once a member is signed in and opted in, and uses the OS keychain); the locked host release (the host CLI on the runner, if any, is whatever is installed); and anything that needs a model call, a Fleet sign-in or a person's approval. Each evidence record repeats this under `installed.unproven` and `installed.build.official_differences`.

CI runners have neither host CLI, so there the registration with the host and the host-visibility step are recorded as not run. They run on a developer machine that has the hosts.

## Run the installed tier locally

```sh
make conformance-installed                         # the 8 cells of this machine's OS, default build
make conformance-installed TAGS="$(make -s print-official-public-tags)"   # the official tags without fleet
scripts/conformance.sh run -tier installed -cell 'context/*' -out /tmp/ev   # some cells, another directory
sh test/conformance/installed/context.codex-cli.sh  # one journey; prints its result as JSON
scripts/conformance.sh report                       # not run / installed-tier pass (reduced) / installed-tier pass / live pass, per cell
```

It needs Go and takes about half a minute. It never reads or writes your real `~/.kenaz`, `~/.claude` or `~/.codex`. A run with other tags overwrites the evidence of the previous run unless you give `-out`.

A cell runs only on its own OS target's platform: macOS on arm64, Ubuntu 24.04 on amd64, Windows on amd64. On any other machine (an Intel Mac, another Linux release) every cell is blocked. `make conformance-installed` and `run -tier installed -min-pass 8` fail then, so that "nothing ran" never looks like success.

The platform is not the target release. The matrix's targets are macOS 14 and Windows 11, and a cell also runs on any other macOS on arm64 or Windows on amd64. So every record names what the runner actually is in `os_observed` (for example `macOS 26.2 (build 25C56) darwin/arm64` or `Windows Server 2025 Datacenter 24H2 (build 26100.4349) windows/amd64`), and installed-tier evidence adds `os_match`: `exact` on the target release, `family` on another release or edition (`os_note` says which), `unknown` when the release could not be observed. Anything but `exact` makes the pass a reduced one. This is about truthful installed-tier evidence only: which platform may run a cell, and what the live gate accepts, are unchanged, and a live pass on a runner that is not the locked OS build is still for the live tier's lock (`compatibility.json`) to refuse.

In CI (`.github/workflows/ci.yml`, dispatch-only) the journeys run in `conformance-installed-linux` (pinned to `ubuntu-24.04`, default build and the official tags without `fleet`), `macos` (pinned to `macos-26`) and `windows (kenaz)` (`windows-latest`, a Windows Server image). The macOS and Windows runners are not the target releases, so their cells are reduced passes. Each job's summary lists the cells, the runner's OS, the reduced passes with their reasons, and every step that failed or was not run.

## Relays never outlive a journey

A relay left behind keeps a port and the temporary directory until its 15-minute idle exit. A journey stops every relay that runs from its sandbox, on every way out:

- **A normal return, or SIGINT, SIGTERM or SIGHUP.** The journey turns the signal into a cancelled run and still cleans up. It asks each relay recorded in the sandbox's `daemon.json` to shut down with the relay's own authenticated request, kills the process group of the relay it started, and kills any recorded relay that is still running. That covers a relay `kenaz mcp` spawned detached after the first one died.
- **The journey killed outright** (SIGKILL, `TerminateProcess`), when it runs no code at all. On macOS and Linux a small watchdog in its own session waits on a pipe the journey holds; when the pipe closes it does the same clean-up and removes the directory. On Windows the relay the journey started is in a job object that the system closes with the journey, which kills it; a relay `kenaz mcp` spawned detached, and the temporary directory, are left behind in that one case.

Nothing outside the sandbox is ever signalled: a recorded pid is stopped only while the operating system reports that its executable is the sandbox's own `kenaz`. A stale `daemon.json` whose pid now belongs to something else, such as your real relay, is left alone. `installed/abort_test.go` interrupts and kills real journeys and checks the process list.

`kenaz init --yes --no-path --no-browser` is the non-interactive form of `kenaz init`. The journeys use `kenaz setup <host>` instead, because a cell is one host and `init` sets up every app it finds on the machine.

## What a live pass still needs

None of this exists in CI today, and the installed tier does not replace any of it:

- **Model credentials for both hosts**: a way for Claude Code and for Codex to run a real session on each runner.
- **An entitled development Fleet test identity**: an official build signed in to the development Fleet as a member of an organization whose plan includes each feature (`KENAZ_CONFORMANCE_DEV` tells the harness one is prepared; it never reads the credentials).
- **A person, or a designed mechanism, for approvals**: sharing, promoting, publishing and forget-all go through the approval page, and telemetry and memory sync need a member's choice.
- **A locked `compatibility.json`**: the exact host releases and their artifact digests on each OS, the OS builds, and the signed candidate's version, commit and digests, each recorded from an observation.
- **Runners for the three OS targets** with those hosts installed, and the eight `journeys/*.sh` scripts that drive a session and observe the Fleet-side outcome.

## Component journeys

Each cell also names a component journey: Go tests that replay fixtures against in-process fake Fleet providers. They are supporting evidence for the live tier (a failure fails the cell, a pass does not pass it). A journey may name tests in several packages (`also`). `TestComponentJourneyTestsExist` fails when a test a journey lists no longer exists, so the lists cannot rot silently.
