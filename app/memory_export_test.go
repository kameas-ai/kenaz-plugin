package app

import "io"

// SetMemoryStdin replaces the purge prompt's terminal check and input for
// a test and returns a restore func.
func SetMemoryStdin(terminal bool, r io.Reader) func() {
	oldT, oldR := memoryStdinTerminal, memoryStdin
	memoryStdinTerminal = func() bool { return terminal }
	memoryStdin = r
	return func() { memoryStdinTerminal, memoryStdin = oldT, oldR }
}

// SetupAction exposes setupAction.
var SetupAction = setupAction
