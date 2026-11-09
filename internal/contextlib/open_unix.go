//go:build !windows

package contextlib

import "syscall"

const openNonBlock = syscall.O_NONBLOCK
