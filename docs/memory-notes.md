# Memory notes: the shared format

Every Kenaz product that writes memory notes follows this: the plugin, the harness, the hosted connector. It is what makes a note written by one readable by another, and it does not assume hooks, a background process or any one AI tool. How a product gets its notes (a background observer, its own agent loop, a tool call) is that product's choice.

## A note

| Field | Rules |
|---|---|
| `id` | `[A-Za-z0-9._-]{1,64}`, minted on the device |
| `title` | One line, at most 200 characters |
| `content` | UTF-8 text, at most 8 KiB. Everything a reader needs is in here, because tags do not sync |
| `content_hash` | SHA-256 of `content` |
| `kind` | `raw` (saved on purpose), `observation`, `session_summary`, and the harness narrative kinds `narrative_extractive`, `narrative_synthesised`, `narrative_extractive_fallback`. A reader treats a kind it does not know like `raw`. The new kinds sit outside Fleet's narrative ranking |
| `scope_kind` | `project`, `global`, `long_term`, and `session` where a product has it. `session` never syncs |
| `source` | Short label of the saving app: `claude-code`, `codex`, `claude-desktop`, `harness`, `cli`, `import` and so on |
| `created_hlc`, `title_hlc`, `scope_hlc`, `pinned_hlc` | Hybrid logical clocks; edits order by them |

## The project a note belongs to

A note with `scope_kind` `project` has a project key, computed the same way everywhere:

1. Find the checkout's remote: `origin`, or the first remote by name. A linked worktree uses its main repository's.
2. Reduce it to `host/path`: no scheme, credentials, port, or `.git` suffix, all lower case. The ssh and https forms of one repository agree.
3. The key is `rk1-` and the first 32 hex characters of the SHA-256 of that text.

So every worktree and clone of a repository has one key on every device, and the key names no host or repository. A repository with no network remote, or a directory that isn't one, gets the first 16 hex characters of the SHA-256 of its main checkout's path. That key is local to one device and never syncs.

Fleet stores no remotes or paths. A product gets one project id per key from Fleet's project registry, with `memproj:` and the key as the `client_request_id`, and sends that id as `scope_id`. A replay returns the row whatever its state, so read `state`: follow `merged_into` for `merged`, and treat `deleted` as final, never asking for that key again. The project's `name` is the name of its folder, never a path or a remote, and one that looks like a credential isn't sent. If `project` goes missing from the settings after this device has seen it, another client turned sync back on: project sync then waits for a fresh acknowledgement of the notice.

## How Fleet treats notes

Fleet dedupes on the same text and kind, within one project, or within `global` and `long_term` together. A scope move onto a group already holding the text comes back `superseded`, and on a merge the source's title and pin are dropped. It may refuse an automatic note with `capture_off` (`retry:true`) when an organization or a global switch has capture off: back off, keep the note local and visible in status, and never count it as refused for good. `consent_upgrade_required` (`retry:true`) can repeat on every push and waits for the member to confirm the newer notice: the same applies. A Fleet that hasn't turned on these kinds answers `invalid_field` on `kind` with `retry:false`: for an automatic note that isn't final, so treat it the same way. While any of these lasts, send at most one such note at a time, not the whole queue. Never send a made-up `consent_version`, and read the response after a settings write rather than assuming it took: with a higher edition stored, a scope drop can be ignored and the 200 still shows `project`.

## Optional sync fields

Fleet also carries `files_read`, `files_modified`, `turn_id`, `tool_name` and `source_turn`. The harness sets them; the plugin does not. They are optional: a note must read correctly without them, so a product that does not set them puts the same facts in `content`.

## Automatic notes

An automatic note has kind `observation` or `session_summary`, and on its device the tag `auto`. It syncs like any other note, under the member's one-time grant for saves; nothing asks about it note by note.

Observation types, in a `type:<name>` tag and a `Type:` line of `content` for readers without tags: `bugfix`, `change`, `decision`, `discovery`, `feature`, `refactor`.

Labels, as `label:<name>` tags: `convention`, `delta`, `mechanism`, `pitfall`, `rationale`, `root-cause`, `tradeoff`.

An observation's `content` holds, in this order and leaving out what is empty: the `Type:` line, one line of context, a paragraph on what happened, `Facts:` with one `- ` line each, `Files read:`, `Files modified:`.

A turn's summary holds lines labelled `Request:`, `Investigated:`, `Learned:`, `Completed:`, `Next steps:`, `Notes:`.

## Before a note is written

A product removes these from anything it sends to a summarising model and from every automatic note:

- `<private>...</private>` blocks, and everything after an unterminated `<private>`.
- Credential shapes: the patterns in `pkg/secretscan` and `internal/memory/capture/scrub.go`, whole PEM blocks, values assigned to names containing `password`, `secret`, `token`, `api_key`, `access_key`, `private_key` or `credential`, command-line passwords, and `Authorization`, `Cookie` and `X-Api-Key` headers. JSON is reduced to its string values first, so that escaping hides nothing.
- Email addresses, replaced with `[email]`.
- Home directories, replaced with `~`.

A model's output that still looks like a credential is dropped, not cleaned. A tool call that touches a key or credentials file (`.env`, `id_rsa`, `*.pem`, `.ssh/`, `.aws/`, a vendor tool's login file and the like), or prints the environment, is not captured at all.

The scan catches common shapes only. It is a safety net, not a guarantee, and no copy may promise more.

## Rules every product keeps

- Off by default, opted in by each member. An organization can turn capture off.
- Raw tool input and output stay on the device, are not kept after processing, and never sync. Only notes sync.
- Notes never enter telemetry and never train a model.
- Recalled automatic notes are labelled as untrusted: tool output can carry injected instructions.
- Notes are not end-to-end encrypted. No copy may say they are.
