//go:build windows

package contextlib

// Windows has no named pipes in a folder to block on.
const openNonBlock = 0
