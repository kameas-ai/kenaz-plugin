# Kenaz for Claude Code and Codex

*Kenaz, a Kameas AI product.*

Kenaz for Claude Code and Codex connects your Claude Code and Codex sessions to your organization's Kameas Fleet. You can:

- **Sign in** with your Kameas account, and register this device under a display name you control.
- **Report session metadata** to your organization's Fleet analytics: session counts and durations, tool names, latency and success, error categories, model names, token usage, and cost when the harness itself reports it (Kenaz does not estimate prices). Prompts, responses, code, file paths and tool output are never sent. Nothing is sent until you sign in yourself and opt in.
- **Use your organization's skills**, and share the skills you write.
- **Search and share team context.**
- **Remember things across sessions** with on-device memory: your AI tool can save short notes (facts, decisions, conventions, fixes) and find them again later. See [Memory](#memory).

It never approves, blocks or rewrites what your agent does.

What Kenaz for Claude Code and Codex reports about your sessions is decided by open-source code you can read. The part that connects to Kameas Fleet (sign-in and transport) is proprietary.

Release binaries are official builds that include a proprietary module and are licensed under the Kameas Client Software License. The source in this repository is Apache-2.0 and can be built without that module, but such builds cannot connect to Kameas Fleet.

## Not the Kameas Fleet admin CLI

`kenaz` is the end-user component of Kenaz for Claude Code and Codex: the plugin and the local program it runs. It is not the Kameas Fleet admin CLI (`fleet-admin`), which is a staff-only tool for operating the Kameas Fleet service. `kenaz` never uses AWS credentials. It signs in only with your own Kameas account (`kenaz login`) and talks only to your organization's Kameas Fleet.

## How it works

Each harness exports its own OpenTelemetry to a receiver on `127.0.0.1` inside the local `kenaz` process. The projector (`internal/project`) maps that telemetry onto a closed allowlist (`fleetapi/allowlist`) and drops everything else on the device. It then hands the result to the Fleet connection behind the `fleetapi` interfaces.

- `plugin/` is the plugin package. It's in Claude Code plugin format, which Codex also loads.
- `.claude-plugin/marketplace.json` is the `kenaz` marketplace (install id `kenaz@kenaz`).
- `cmd/kenaz` is the public build. It uses `fleetapi/stub` and cannot connect to Kameas Fleet.

See [ARCHITECTURE.md](ARCHITECTURE.md).

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

## Memory

Memories are stored only on this device and are never sent to Kameas. When your AI tool uses a memory in a session, it becomes part of that session and is sent to your AI provider (Anthropic or OpenAI) like anything else in the conversation.

Kenaz memory gives Claude Code and Codex a small notebook on your device. The agent saves short notes with the `memory_save` tool (durable facts, decisions, conventions and fixes) and finds them again with `memory_search`, `memory_get` and `memory_recent`. At the start of each session, the agent gets a short index of the newest note titles for the current project (titles and ids only, never note bodies).

- **Where:** one SQLite file per OS user, `memory/memory.db` under the Kenaz state directory (`kenaz memory path` prints it). The directory is readable only by you (0700, file 0600). Everyone who shares an OS account sees the same memories.
- **No sign-in needed:** memory works whether or not you are signed in to Kameas Fleet, and telemetry choices don't affect it.
- **Limits:** notes are up to 8 KiB with a 200-character title; the store holds up to 2,000 notes or 16 MiB. When it is full, nothing is deleted automatically: remove notes with `kenaz memory forget <id>` or `kenaz memory purge`.
- **Credentials:** `memory_save` refuses text that obviously contains a credential (for example API keys, access tokens or private keys). This is a basic check, not a scanner: never ask your agent to save secrets or personal data.
- **Your controls:**

  ```sh
  kenaz memory list [--project current|all] [--limit N]
  kenaz memory search <query>
  kenaz memory show <id>
  kenaz memory forget <id>
  kenaz memory purge [--yes]   # delete every memory on this device
  kenaz memory path
  kenaz memory disable         # or enable; memory is on by default
  ```

  Turning memory off stops the tools and the session index and keeps the store; `kenaz memory purge` deletes it. `kenaz status` and `kenaz doctor` show whether memory is on and how many notes it holds.

## Uninstall

`kenaz logout` signs out and deletes queued Fleet data, but it keeps your memory, because memory is local data, not Fleet data. To delete memory, run `kenaz memory purge` (or delete the file `kenaz memory path` prints). The store is per OS user, so it holds the memories of everyone who uses this OS account. `kenaz setup --undo claude|codex` restores your Claude Code or Codex configuration and also leaves memory in place.

## Development

```sh
make check   # fmt, vet, tests, policy and naming gates
make build   # bin/kenaz (unofficial build)
```

## License

Apache-2.0 (see [LICENSE](LICENSE) and [NOTICE](NOTICE)). Official release binaries are licensed as described above.

Claude and Claude Code are trademarks of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz for Claude Code and Codex is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic or OpenAI.
