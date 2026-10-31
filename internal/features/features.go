// Package features holds build-time feature switches.
package features

// OpenCode reports whether OpenCode support is reachable in this build:
// `kenaz setup opencode` (and --undo), its help, doctor and status lines,
// and relaying opencode client-kind telemetry. It is off in shipped builds
// (OpenCode is not part of the first release); the adapter code stays in
// the repo. Build with -tags kenaz_opencode to turn it on. Tests may set
// it.
var OpenCode = openCodeBuild

// ClaudeDesktopChat reports whether this build ships alongside the Kenaz
// desktop extension (.mcpb) for chat in the Claude desktop app: `kenaz init`
// then points to it, and the Supported Tools hosts include the Claude
// desktop app's chat. The official release compiles with
// -tags kenaz_claude_desktop_chat exactly when it builds the .mcpb; other
// builds say nothing about chat in the Claude desktop app. Tests may set it.
var ClaudeDesktopChat = claudeDesktopChatBuild

// Cowork reports whether this build lists Cowork as a Supported Tools host.
// It is off in shipped builds until the Claude directory listing is live;
// build with -tags kenaz_cowork to turn it on. It is independent of
// ClaudeDesktopChat. Tests may set it.
var Cowork = coworkBuild
