//go:build windows

package credstore

import "io/fs"

func ownedByCurrentUser(fs.FileInfo) bool { return true }
