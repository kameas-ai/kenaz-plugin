//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"
)

func platformLink(target, link string) error { return os.Symlink(target, link) }

// crossDevice reports a rename refused because the two paths are on
// different volumes.
func crossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }
