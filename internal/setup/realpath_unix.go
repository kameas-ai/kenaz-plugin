//go:build !windows

package setup

import "path/filepath"

// realPath resolves every link in p.
func realPath(p string) (string, error) { return filepath.EvalSymlinks(p) }
