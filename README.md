# Kenaz for Claude Code and Codex

*Kenaz, a Kameas AI product.*

Kenaz for Claude Code and Codex connects your Claude Code and Codex sessions to your organization's Kameas Fleet. You can:

- **Sign in** with your Kameas account, and register this device under a display name you control.
- **Report session metadata** to your organization's Fleet analytics: session counts and durations, tool kinds (built-in tools by name; tools and servers you added are reported only as "mcp" or "other"), latency and success, error categories, model names, token usage, and cost when the harness itself reports it (Kenaz does not estimate prices). Prompts, responses, code, file paths and tool output are never sent. Nothing is sent until you sign in yourself and opt in.
- **Use your organization's skills**, and share the skills you write.
- **Search and share team context.**
- **Remember things across sessions** with memory stored on your device: your AI tool can save short notes (facts, decisions, conventions, fixes) and find them again later, with optional sync to your own Kameas Fleet account. See [Memory](#memory).

It never approves, blocks or rewrites what your agent does.

What Kenaz for Claude Code and Codex reports about your sessions is decided by open-source code you can read. The part that connects to Kameas Fleet (sign-in and transport) is proprietary.

Release binaries are official builds that include a proprietary module and are licensed under the Kameas Client Software License. The source in this repository is Apache-2.0 and can be built without that module, but such builds cannot connect to Kameas Fleet.

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

1. **PATH.** If `kenaz` is not on your `PATH` yet, it shows the change and asks `Add <dir> to PATH in <profile>? [y/N]`. On macOS and Linux it adds one marked block (`# >>> kenaz PATH >>>`) to your login shell's profile: `~/.zshrc` (or `$ZDOTDIR/.zshrc`); for bash on macOS the first of `~/.bash_profile`, `~/.bash_login` and `~/.profile` that exists (else a new `~/.bash_profile`), and `~/.bashrc` on Linux; or `~/.config/fish/conf.d/kenaz.fish`. Running it again changes nothing, and a new install directory replaces the block. The profile as it was before kenaz first changed it is kept as `<profile>.kenaz-backup`. A profile that is a symbolic link (dotfiles managers) is edited where the link points, and the link is kept. A profile that is read-only, a broken link, or has a kenaz block with no end marker is left alone, and init prints the line to add yourself. On Windows it adds the folder to the front of your user `Path` (`HKCU\Environment`) unless it is already there, also when written with variables such as `%LOCALAPPDATA%`. For an installer-managed install it adds `install/current/bin`, so upgrades keep working. With `--no-path` it changes nothing and prints the line to add. Open a new terminal afterwards, and quit and reopen Claude Desktop and ChatGPT so they see it.
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

**Which plugin is used.** If you already registered the `kenaz` marketplace from GitHub (`claude plugin marketplace add kameas-ai/kenaz-plugin`, or the same with `codex`), setup uses it instead of registering the archive's copy, and you don't need `--no-bundle`. If its plugin is not installed yet, setup installs it (`claude plugin install kenaz@kenaz` or `codex plugin add kenaz@kenaz`), and `--undo` uninstalls only what setup installed; the marketplace stays yours. If you installed the plugin and turned it off, setup leaves it off (and installed on `--undo`) and tells you how to turn it on. The GitHub plugin starts `kenaz` from `PATH`, so setup warns when `kenaz` is not on `PATH` (`kenaz init` fixes that). Two flags override this:

- `--bundle <dir>` registers that archive's plugin and replaces the GitHub marketplace. `--undo` removes it and prints the command that adds the GitHub marketplace back.
- `--no-bundle` never registers an archive and uses `kenaz` from `PATH`.

Any other `kenaz` marketplace that setup did not register (a different repository or a local directory) is refused, and setup asks you to remove it, so the plugin is never loaded twice.

Paths with spaces work. Re-running setup changes nothing; running it from a newer archive re-registers that one. `kenaz setup --undo <host>` removes the registration and restores your configuration, and `kenaz setup doctor` checks it.

### Where Kenaz keeps its files

| | Configuration (`config.toml`) | State (relay, queue, memory, credentials files) |
|---|---|---|
| macOS | `~/Library/Application Support/kameas/kenaz` | `~/Library/Application Support/kameas/kenaz/state` |
| Linux | `$XDG_CONFIG_HOME/kameas/kenaz` | `$XDG_STATE_HOME/kameas/kenaz` |
| Windows | `%AppData%\kameas\kenaz` | `%LocalAppData%\kameas\kenaz\state` |

`KENAZ_HOME` overrides both. On Windows the state stays on this machine (`%LocalAppData%` never roams with a roaming profile). Releases up to 0.4.x kept it in `%AppData%\kameas\kenaz\state`; the first `kenaz` command of a newer release moves it there. If two `kenaz` processes start at once, both end up on the new location. If another `kenaz` process (such as a running relay) still has files open in the old location, `kenaz` keeps using it and tries again next time. Run `kenaz setup claude` and `kenaz setup codex` once after the move if you installed from an archive, so the hosts point at the plugin copy's new location.

## OpenCode (preview)

**OpenCode is not part of the first release.** Released builds leave it out: `kenaz setup opencode` isn't offered, doctor and status don't mention OpenCode, and the relay drops any OpenCode activity on the device. The adapter stays in this repository; build with `go build -tags kenaz_opencode ./cmd/kenaz` to try it.

OpenCode support is new and not yet qualified on every platform. OpenCode does not load the Claude Code plugin format, so Kenaz connects to it in two pieces, which `kenaz setup opencode` registers in your global `opencode.json` (with a diff, a backup, and `kenaz setup --undo opencode` to restore it):

- the `kenaz` MCP server, for the status, context, skills and memory tools;
- the Kenaz OpenCode adapter (`opencode/`), a small OpenCode plugin with no dependencies. It observes OpenCode's session, message and tool events and reports only metadata (the same closed allowlist as for the other harnesses) to the local `kenaz` relay. It never changes what OpenCode does.

Pass `--adapter <dir>` if the adapter is not in the `opencode/` directory next to the `kenaz` binary's `bin/`. Setup edits only `opencode.json`, which must be plain JSON, and needs OpenCode 1.19.4 or later. The adapter was checked against events recorded from OpenCode 1.19.4. Skills synced from your organization appear in OpenCode through `~/.agents/skills`.

Differences from Claude Code and Codex: there is no session-start banner or memory index (use the `fleet_status` and `memory_*` tools), and OpenCode activity is sent only once your organization's Kameas Fleet accepts OpenCode as a client. Until then it is dropped on the device.

## Signing in and where credentials are kept

`kenaz login` opens your browser to sign in. Sign-in redirects back to a one-time listener on `127.0.0.1` (a random port) in the `kenaz` process. If no browser can be opened (for example on a machine with no display), or with `kenaz login --no-browser`, `kenaz` prints the sign-in link instead and keeps waiting for the redirect.

Sign-in tokens and the key that encrypts queued uploads are kept in the OS keychain: the macOS Keychain, the Windows Credential Manager, or the Secret Service on Linux. On a machine with no usable OS keychain (such as a Linux VM with no Secret Service), `kenaz` keeps them as files instead, in a `credentials` directory under its state directory (`~/.local/state/kameas/kenaz/credentials` by default on Linux, or `$KENAZ_HOME/state/credentials`). The directory is created with mode 0700 and each file with mode 0600, and `kenaz` refuses to read a credentials file that other users can read. `kenaz logout` deletes these files. `kenaz doctor` and `kenaz status` show which store is in use (`credential store: OS keychain` or `credential store: files (no OS keychain)`).

The first sign-in or relay start records the choice in `credentials/STORE`, and `kenaz` uses the recorded store from then on, without checking again. If the recorded store is the OS keychain and it can't be reached, commands stop with `OS keychain unavailable; try again or run kenaz logout to reset`; they never switch to files on their own, which would sign you out. `kenaz logout` removes the record, so the next sign-in chooses again. `kenaz doctor` shows the recorded store and whether a fresh check agrees with it. The harness hooks and `kenaz mcp` never check for a keychain (they have to start quickly). Until a store is recorded, they treat the device as signed out.

**Without an OS keychain, sign-in tokens and the spool key are protected only by file permissions in your home directory.** Anyone who can read your files as you, or as an administrator of the machine, can use them.

### Signing in from inside a VM (such as a Kenaz Workbench)

The same `kenaz` runs inside or outside a VM; there is no separate mode. Run `kenaz login` (or `kenaz login --no-browser`) in the VM and open the printed link in a browser:

- **A browser inside the VM** completes sign-in directly, because the redirect goes to `127.0.0.1` in the VM.
- **A browser on the host** completes sign-in only if the host's `127.0.0.1:<port>` reaches the VM's `127.0.0.1:<port>`, for the port in the printed link's `redirect_uri`. Some VMs forward localhost ports to the guest automatically; others need a forward set up after the link is printed, for example `ssh -L <port>:127.0.0.1:<port> <vm>`. Without a forward, the host browser shows a connection error after you sign in.
- If the host browser ends on that connection error, the address bar holds the full redirect (`http://127.0.0.1:<port>/callback?code=…&state=…`). Running `curl '<that address>'` inside the VM, while `kenaz login` is still waiting, delivers it and completes sign-in. The code is single-use and only works with the waiting `kenaz login`.

## Approving writes

Tools that write to Kameas Fleet (`skills_publish`, `context_share`, `context_promote`) and memory writes for a signed-in account that reach Kameas Fleet (forget-all, turning on memory sync, sending held changes, moving notebook notes into your account, and a save, edit or delete while sync is on or of a note Fleet already holds) don't write when your agent, or a `kenaz memory` command, asks for them. They prepare a request and return a `request_id`, `status: "pending"` and an `approve_url`. Nothing is uploaded until you open that link, check the exact content and destination shown there, and approve with a security key or your device's unlock (Touch ID, Windows Hello or a PIN, through WebAuthn with user verification). A harness permission prompt, an "always allow" setting or a `user_confirmed` argument from the agent doesn't count as approval.

- **Where:** the page is served by the local `kenaz` relay at `http://localhost:<port>/approvals/`, on both `127.0.0.1` and `[::1]`. It accepts requests only from that origin. The text the page shows is written by the relay from the exact stored content, not by the agent. The pending request is kept in `approvals/approvals.json` under the state directory (0600). That file and the enrolled keys are signed with a key kept in the OS keychain, so an agent that can only write files can't plant or alter a request (see the threat model below). A file changed by anything other than the relay is set aside (`approvals.json.rejected`) and never used, so keys you set up must then be set up again.
- **Set up once:** `kenaz approvals enroll` opens the setup page. The first setup asks you to sign in to Fleet again in the browser, and only an account that matches your `kenaz login` is accepted. Setup then continues only in the browser that completed that sign-in. Adding another key needs approval from a key you already set up. If you lost every key, use "Replace lost authenticators", which signs you in again and revokes the old keys and any open requests.
- **Status:** `kenaz approvals` lists requests and their status, `kenaz approvals open <request_id>` opens one, and the `approval_status` tool lets the agent check. Neither the CLI nor any tool can approve, cancel or set up a key.
- **Expiry:** requests expire 5 minutes after the agent prepares them. `kenaz logout` or switching accounts cancels open requests and deletes their content. If the relay stops in the middle of a write, the request shows `reconciling`. Kenaz then checks whether the write arrived and never sends it again blindly. Signing out closes a `reconciling` request as `outcome_unknown`; it is not retried after you sign back in. A request still `reconciling` after 7 days is closed as `failed` (`outcome_unknown`) too.
- **Many notes:** one approval lists at most 64 notes, each with its full text (the page shows a preview and a "Show full text" control). With more, the command approves the first 64 and says how many remain; run it again after approving to review the next batch.
- **Opened from another site:** an approval link opened from a web page, chat or email is refused. Copy it from your terminal (or `kenaz approvals`) into the address bar.
- **No authenticator, no write:** if no key is set up and none can be, the write tools return `approval_unavailable` and nothing is written. Public builds (`cmd/kenaz`) can't do the Fleet sign-in that setup needs, so they always report `approval_unavailable`. Official builds can set up a key only once their Fleet connection supports browser identity verification (`fleetapi.IdentityVerifier`). Without an OS keychain (`credential store: files`), approvals are always `approval_unavailable`, because files your agent can rewrite can't be trusted to hold approvals. Which browsers, operating systems and authenticators work hasn't been tested yet.
- **Threat model:** approvals protect against an agent that can act only through the `kenaz` MCP tools and commands (it can ask for writes, but it can't approve them or forge an approved request). They don't protect against an agent that can run shell commands as your OS user. The key that signs `approvals.json` is in the OS keychain, but that doesn't stop such an agent: on macOS the item is created through `/usr/bin/security`, so any process running as you can read it without a prompt, and an unlocked Secret Service or Credential Manager behaves the same way. A same-user shell agent can already read your Fleet sign-in tokens the same way and call Kameas Fleet directly, so it's out of scope. If your agent can run arbitrary shell commands, approvals don't contain it.
- **Limits:** approval protects the writes `kenaz` makes on your behalf. An agent that runs as your OS user can still reach anything your user account can. It can read Fleet sign-in tokens kept as files (no OS keychain), and often items in an unlocked OS keychain, and then call Kameas Fleet directly. Kenaz accepts any authenticator that reports user verification. It doesn't require attestation, so it can't tell a hardware key from a software one that an agent registered in a setup you completed. Only complete a Fleet sign-in for setup that you started yourself.

## Memory

Memories are stored on this device. They are sent to Kameas only if you turn on memory sync for your Kameas Fleet account (off by default), and never as telemetry. When your AI tool uses a memory in a session, it becomes part of that session and is sent to your AI provider (Anthropic or OpenAI) like anything else in the conversation.

Kenaz memory gives Claude Code and Codex a small notebook on your device. The agent saves short notes with the `memory_save` tool (durable facts, decisions, conventions and fixes), finds them again with `memory_search`, `memory_get` and `memory_recent` (keyword search on this device; no embeddings), and can change or delete them with `memory_update`, `memory_forget` and `memory_forget_all`. At the start of each session, the agent gets a short index of the newest note titles for the current project (titles and ids only, never note bodies). Every result says where a change took effect: on this device, waiting for your approval, queued for or synced to your account, or refused.

- **Where:** SQLite files under the Kenaz state directory, readable only by you (directory 0700, files 0600). Notes saved while signed out go to the device notebook, `memory/memory.db` (`kenaz memory path` prints it); everyone who shares an OS account sees those. Notes saved while signed in go to a separate store for that Kameas Fleet account (`memory/profiles/`), which is shown only while that account is signed in: after you sign out or switch accounts, the previous account's notes leave your agent's results and stay on disk.
- **No sign-in needed:** memory works whether or not you are signed in to Kameas Fleet, and telemetry choices don't affect it.
- **Limits:** notes are up to 8 KiB with a 200-character title; each store holds up to 2,000 notes or 16 MiB. When it is full, nothing is deleted automatically: remove notes with `kenaz memory forget <id>`, or empty the device notebook with `kenaz memory purge`.
- **Credentials:** `memory_save` refuses text that obviously contains a credential (for example API keys, access tokens or private keys), and a stricter check keeps credential-like notes from ever being synced. These are basic checks, not a scanner: never ask your agent to save secrets or personal data.
- **Your approval:** while you are signed in, every memory change for your account waits for your approval on the Kenaz approval page (see [Approving writes](#approving-writes)); `kenaz memory` commands print and open that page, they never approve anything themselves, and neither does a terminal answer or a flag your agent sets. If no approval is possible (no authenticator set up, or the relay is not running), saves and edits still apply on this device but their upload waits until you approve it (`kenaz memory sync approve`); a new text never reuses an approval you gave for an earlier one. A deletion only hides the note: it is deleted when you approve, and `kenaz memory revert <id>` shows it again. Deleting everything, turning on sync and moving notebook notes into your account are refused (`approval_unavailable`); turning on sync uploads exactly the notes listed on the approval page. Signed out, the device notebook works as before: save, edit, delete and `kenaz memory purge` need no approval, because nothing leaves the device.
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

With an official build, a plan that includes memory sync, and your sign-in, you can sync the notes of your Kameas Fleet account to your other enrolled devices. It is off until you turn it on with `kenaz memory sync enable`, which shows the Memory Sync Notice and opens the approval page, where the notice is shown again and you approve with your authenticator. This consent is separate from telemetry consent: turning one on or off never changes the other.

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
