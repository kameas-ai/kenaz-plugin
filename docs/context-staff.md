# Context files: staff use and go-live

Local context files, `context_write`, the team share and pull tools and the session index are behind the build tag `kenaz_context_library`, and no release sets it. This is how staff run them without a customer release, and what has to happen before customers get them.

## Turning it on

`scripts/staff-build.sh` includes it, so build, wrap and register as `docs/memory-staff.md` describes. The other member of the org does the same. Stop any older relay first: `pkill -f 'kenaz daemon'`. An older relay can't prepare a file share and reports `approval_failed`.

Your account needs a team on a plan with team sharing: `/kenaz:status` and `team_members` show it. Sign in with `kenaz login`. Signed out, `context_write`, `context_list` and `context_read` work only for your own files and a repository's: a team's folder needs you signed in, and the tools say so. The exception is reading a folder with no owner on record, which stays readable until it's claimed.

Team copies are separated by account: each team's folder belongs to one environment, organization, account and team, and another account signed in on this device can't list, read or write it. A folder with no owner on record, such as one made by hand before your first sync, becomes yours and that team's on your first sync, and its files are listed in the one prompt, which names the organization. If you answer no and nothing has come down into it, the folder goes back as it was. If a file in it is linked to another account, the folder stays as it is: your own files move to a new folder, the rest aren't sent and are reported, and other accounts can still read that folder until those files are moved or deleted. If a folder's owner record can't be read, sync says which folder.

## How two people use it

Ask your AI tool, in your own words. These are the steps it takes:

1. **Write.** `context_write` saves `personal/release-process.md` under `~/.kenaz/contexts/`. Or edit that folder by hand. Repository files in `.kenaz/context/` are edited as normal files and can be shared too.
2. **Draft from memory, if you want to.** `/kenaz:draft-context what the file should cover` runs only when you ask, and shows you the exact text before saving it. Only the staff bundle carries it.
3. **Share.** `context_share_file` returns a link and a command. Open the link and press Approve, or run `kenaz approve`, after checking the exact text, the team and the short name. Nothing leaves your device before that. Fleet scans the text again and can still refuse it. The page warns about private notes only when your AI tool marked the text as drawn from memory. No warning doesn't mean the text has none, so read it either way.
4. **Find.** Your colleague uses `context_search` to get the file's id.
5. **Pull.** `context_pull_files` with that id saves a working copy under `~/.kenaz/team-contexts/<team>/`. Copies are labelled untrusted, because a teammate can write anything, and they are listed at session start but never loaded whole.
6. **Update.** The author edits their file and shares it again: same id, next version. Your colleague pulls again. A copy with its own changes is left alone unless they set `overwrite`, and the replaced text goes to the folder's `.trash` for 30 days.
7. **Removal.** Deleting a shared file from Kameas Fleet doesn't delete copies on people's devices. A pull reports a copy whose file is gone and leaves it in place.

## Sharing a team's folder

Edit Markdown in `~/.kenaz/team-contexts/<team>/`, or have your AI tool do it, and run `kenaz context sync [--team name]` in a terminal. It brings your team's changes down, lists what would go up, and asks once. Press `v` to read the exact text. If you both changed a file, your team's text stays at the file's name and yours is saved beside it as `<name> (your version).md`.

Your own files in `~/.kenaz/contexts/` stay on the device: move one into a team folder to share it. A repository's `.kenaz/context/` files aren't part of sync; `context_share_file` still shares them one at a time. Only files directly in the team's folder sync, not subfolders. A file deleted here stays on Kameas Fleet for now, and the command lists it. If a file of the same name was removed from Kameas Fleet, sharing that name is refused: rename the file and run it again.

When you share a folder, check the list first: Kenaz scans for credentials, but not for everything you may not want your team to read. Deleting a shared file doesn't remove copies people already have.

## What it can't do

- **Edit a teammate's file.** Fleet refuses a push over another member's file. `context_share_file` says so before asking for an approval, and `as_new` shares your version as your own file instead.
- **Update your own file from a second device, or pull everything at once, on an older build.** Both depend on the Fleet client built into your program. With one that passes on version, author, short name and a listing, leave out the ids to pull all the team and organization files you can see, and pass `next_cursor` as `cursor` to continue a long list; your own file can be updated from any device, and copies keep their short names. A pull of everything can miss nodes Fleet wrote in one batch, until Fleet's cursor is fixed; pull those by id. With an older client, a pull with no ids returns `list_unavailable`, a copy can only be shared as new, and copies are named from their titles.
- **Say why a file is gone.** Kameas Fleet doesn't report a reason for a removal, so a pull can't show its message for a file removed because it may hold a secret or unsafe content.
- **History.** Fleet has no revisions, so an update replaces the text there. The device's `.trash` holds what this device replaced.
- **Update a file an admin promoted to the whole organization.** It's refused as `node_promoted`.
- **Personal files on Fleet.** Fleet has no private scope, so your own files stay on your device unless you share one with a team.

## Removing the build tag

Deleting `kenaz_context_library` means making `features.ContextLibrary` true in every build. `plugin/` is what customers install, so what only staff have stays out of it until then: `scripts/staff-bundle.py` lays `staff/plugin/` over `plugin/` and adds the Codex prompts in `STAFF_CODEX_PROMPT_TOOLS` to its copy of `plugin/.codex-mcp.json`. These must hold first:

- `staff/plugin/skills/draft-context` moves to `plugin/skills/`, `context_share_file` moves into `plugin/.codex-mcp.json`, and the setup goldens are regenerated.
- The README and `plugin/README.md` carry the text in the next section, and `plugin/skills/kenaz/SKILL.md` and the `.mcpb` manifest list the tools.
- The next edition of both memory notices carries the session-index sentence below.
- An administrator can turn file sharing off.
- Nothing customers read describes teammates' edits or file history unless they exist.
- A pull gives the reason when a file was removed because it may hold a secret or unsafe content. The message is built, but it needs Kameas Fleet to report the reason and the Fleet client to pass it on.
- Removing a file over a secret or unsafe content can't be undone by a retry. When Kameas Fleet builds that removal, it has to raise the version or refuse to bring the file back, because a share retried after a lost reply could otherwise restore it. The Fleet client change that shows deleted files also has to land first.
- People writing in Hebrew, Arabic or Persian can share normal text. These are refused today and need to be right before customers:
  - a right-to-left mark next to ASCII, an Arabic letter mark, and a Persian zero-width non-joiner before a space
  - a pasted zero-width space, soft hyphen, line separator, form feed, and a byte order mark in the middle of a file
  - words that look like a note id, on any share: `mem-20261104`, `mem-16384000`, `--mem-deadbeef`
- The notices and Privacy Policy carry the lines about publishing text from a note to a team.
- `README.md`'s memory sync section no longer says a note "is never shared with a team" without saying that a person can choose to publish text from one.

## Text for the READMEs and docs

- Shared context files are stored in Fleet and aren't end-to-end encrypted.
- Deleting a shared file from Kameas Fleet doesn't delete copies on people's devices.
- A file you share is stored with a short name you can see before you approve it, never its full path on your device.
- When a session starts, Kenaz gives your AI tool a list of your memory notes and your team's context files. When your tool reads a file or note, its full text, including text your teammates wrote, goes to that tool's provider like the rest of the conversation.
- For admins: What a provider does with this text depends on each member's plan with that provider, including whether the plan allows training on conversations.
