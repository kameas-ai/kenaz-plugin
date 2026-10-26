//go:build !kenaz_opencode || kenaz_dev

package features

// OpenCode stays off in the staff dev build (kenaz_dev), even with
// kenaz_opencode.
const openCodeBuild = false
