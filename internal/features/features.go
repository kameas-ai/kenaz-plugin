// Package features holds build-time feature switches.
package features

// OpenCode reports whether OpenCode support is reachable in this build:
// `kenaz setup opencode` (and --undo), its help, doctor and status lines,
// and relaying opencode client-kind telemetry. It is off in shipped builds
// (OpenCode is not part of the first release); the adapter code stays in
// the repo. Build with -tags kenaz_opencode to turn it on. Tests may set
// it.
var OpenCode = openCodeBuild
