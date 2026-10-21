// Package skills materializes Kameas Fleet Agent Skills into the harness
// user skill roots: ~/.claude/skills/kameas-<slug>
// for Claude Code and ~/.agents/skills/kameas-<slug> for Codex.
//
// Ownership: the relay only creates, updates or deletes a directory that
// carries a valid .kameas-managed marker; an unmarked directory at the
// target name is reported name_conflict and left byte-identical.
// Writes are built in a temp sibling and swapped in by rename.
// Payloads are re-validated on the device before anything is written;
// executable bits are never set.
//
// Permissions: allowed-tools and hooks frontmatter is
// stripped from every skill except a mandated one whose payload bytes match
// the admin-reviewed sha256. Mandated does not mean enforced: a user
// may delete a mandated skill; it is re-materialized on the next poll, and
// nothing is locked.
//
// The daemon runs Lane (poll, ack, revocation sweep); the MCP tools in
// internal/mcp/tools/skills share Store's cross-process lock.
package skills
