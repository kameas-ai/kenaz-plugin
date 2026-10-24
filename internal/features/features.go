// Package features holds build-time feature switches.
package features

// OpenCode reports whether OpenCode support is reachable in this build:
// `kenaz setup opencode` (and --undo), its help, doctor and status lines,
// and relaying opencode client-kind telemetry. It is off in shipped builds
// (OpenCode is not part of the first release); the adapter code stays in
// the repo. Build with -tags kenaz_opencode to turn it on. Tests may set
// it.
var OpenCode = openCodeBuild

// ClaudeDesktopChat reports whether `kenaz init` mentions the Claude
// Desktop chat extension (.mcpb) at all. It is off in shipped builds: the
// extension is not available yet and needs legal coverage before it is
// advertised, so shipped builds say nothing about Claude Desktop chat.
// Build with -tags kenaz_claude_desktop_chat to turn it on. Tests may set
// it.
var ClaudeDesktopChat = claudeDesktopChatBuild
