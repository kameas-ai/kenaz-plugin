//go:build !windows

package conformance

// windowsInfo is the registry read on Windows.
func windowsInfo() OSInfo { return OSInfo{} }
