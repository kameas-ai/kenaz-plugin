# Automatic memory: staff use and go-live

Run `kenaz memory on`. One notice, one Enter: capture, sync and your earlier notes are set up. `kenaz memory off` stops capture and sync. The rest of this page is what's underneath.

Automatic capture is behind the build tag `kenaz_memory_capture`, and no release sets it. This is how staff run it without a customer release, and what has to happen before customers get it.

## Turning it on

1. Build and wrap it: `scripts/staff-build.sh <release checkout> <Fleet client checkout> <new directory>`, with the client ids in `FLEET_DEV_CLIENT_ID` and `FLEET_PROD_CLIENT_ID`. It builds the official flavor with capture, project notes and context files, unsigned, from copies of those checkouts. To keep notes on this device, use `-tags fleet,kenaz_memory_capture` yourself and wrap it with `scripts/staff-bundle.py`.
2. Check it: `<new directory>/bin/kenaz --version`.
3. Put `<directory>/bin` first on your `PATH`, and sign in if needed.
4. Close your AI tools, and stop any relay still running from another build: `pkill -f 'kenaz daemon'`. A relay stays up across upgrades, and an older one would send queued notes it doesn't understand.
5. Register it with every host that uses Kenaz, so that no host starts an older relay: `kenaz setup claude --bundle <directory>`, and the same with `codex`. Start your AI tool again.
6. Turn it on: `kenaz memory capture enable --writer host-cli`. Read the notice, then press Enter.
7. Check: `kenaz memory capture status`.
8. To sync notes, run `kenaz memory sync enable`, then `kenaz memory sync auto on`, each in a real terminal. Read the notice, then press Enter. An agent can't do this for you: `memory_sync_settings` only turns sync off.

`--writer host-cli` needs a signed-in account, and it runs only on Fleet's answer that the writer isn't switched off.

## Turning it off

- **Stop capturing:** `kenaz memory capture disable`. The hooks stay registered and return at once, without reading the session. Saved notes stay.
- **Back to the released plugin:** `kenaz setup --undo claude`, and the same with `codex`, then add the released marketplace again as it says. Take `<directory>/bin` off your `PATH` and stop the relay as above.

## Next to another memory plugin

Both run, so a session gets two indexes and pays for two background models. `kenaz memory capture compare` shows both sides' notes and work, reading only counts from the other's database. Turn this one off as above.

## Bringing notes over

`kenaz memory import claude-mem --dry-run` shows what would come over, by project, and writes nothing. It takes decisions of any age plus fixes and summaries from the last 30 days, up to half the room your Fleet limit leaves, so that the rest stays free. `--yes` brings them over, and needs capture on. It reads that database read-only and never reads its prompts or its queue. Notes that look like they hold a credential or an email address stay behind, and imported notes are labelled automatic and untrusted. A project needs a checkout under `--root` with a network remote to sync.

## Which hosts

The plugin's hooks file is shared. A capture hook runs only under Claude Code, which it recognizes from the environment the host gives a plugin hook; under Codex or an unknown host it returns at once without reading the session.

Codex asks people to trust a plugin's hooks again whenever the plugin's version changes, and asks about these new ones too, even though capture does nothing there yet.

## Removing the build tag

Deleting `kenaz_memory_capture` means making `features.MemoryCapture` true in every build. That's a one-line code change, and the rest is conditions, all of which must hold first:

- SA 1.5 and Privacy 2.5 are live, with the 14-day notice done.
- Notices `memory-capture/v1` and, for project notes, `memory-sync/v4` are shipped, with the public wording changes that go with them.
- The independent review of the rule that Kenaz never reads, stores, copies or forwards a vendor's credentials has passed, before any customer gets the `host-cli` writer.
- The notice's model line matches what ships.
- Fleet's capture switches are live, because a Fleet without them counts as allowing `host-cli`.
- Fleet's quotas have been raised: steady use fills the old defaults within months. Kenaz reads each account's limits from Fleet.
- A real Codex run shows the hooks registered, trusted and doing nothing.

A release that carries this hooks file reaches every customer even with the tag still in place. Each prompt, tool call and stop then starts the program, which returns in about 12 ms without reading the session, and Codex users get the trust prompt. Decide that on purpose.
