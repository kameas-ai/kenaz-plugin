//go:build !windows

package credstore

import (
	"io/fs"
	"os"
	"syscall"
)

func ownedByCurrentUser(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Getuid()
}
