//go:build !windows

package config

import "syscall"

// errCrossDevice is the platform's "different volumes" rename error.
var errCrossDevice error = syscall.EXDEV

func syscallBusy() error { return syscall.EBUSY }
