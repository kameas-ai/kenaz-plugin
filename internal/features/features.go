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

// ContextLibrary reports whether the local context file library is reachable
// in this build: `kenaz context`, the context_list, context_read, context_write, context_share_file and
// context_pull_files tools, sharing and pulling files, and
// the session-start context index. It is off in shipped builds until it is
// ready to announce; build with -tags kenaz_context_library to try it. Tests
// may set it.
var ContextLibrary = contextLibraryBuild

// Cowork reports whether this build lists Cowork as a Supported Tools host.
// It is off in shipped builds until the Claude directory listing is live;
// build with -tags kenaz_cowork to turn it on. It is independent of
// ClaudeDesktopChat. Tests may set it.
var Cowork = coworkBuild

// MemoryCapture reports whether automatic memory capture is reachable in this
// build: `kenaz memory capture`, the capture hook events and the background
// worker. It is off in shipped builds until a legal decision and new public
// wording land; build with -tags kenaz_memory_capture to try it. Even then it
// stays off until the user turns it on. Tests may set it.
var MemoryCapture = memoryCaptureBuild

// MemoryProjectScope marks the release in which Kameas Fleet syncs project notes.
// It turns on memory-sync/v4 and makes forget-all erase project notes on an
// account's other devices. Off until that release; build with
// -tags kenaz_memory_project_scope to try it. Tests may set it.
var MemoryProjectScope = memoryProjectScopeBuild

// MemoryCaptureLocalTrial lets a build that cannot reach Fleet run the host CLI
// capture writer without Fleet's confirmation that it is switched on. It is for
// trying capture locally: an official provider never honours it. Build with
// -tags kenaz_memory_capture_local. Tests may set it.
var MemoryCaptureLocalTrial = memoryCaptureLocalBuild
