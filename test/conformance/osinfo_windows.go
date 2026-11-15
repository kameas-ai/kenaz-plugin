//go:build windows

package conformance

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// windowsInfo reads the Windows edition and build from the registry. The
// ProductName value still says "Windows 10" on Windows 11 clients, so a
// client's name is derived from the build number (22000 and later is
// Windows 11) and the edition; a Server's ProductName is used as it is.
func windowsInfo() OSInfo {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return OSInfo{}
	}
	defer k.Close()
	str := func(name string) string { s, _, _ := k.GetStringValue(name); return strings.TrimSpace(s) }
	build := str("CurrentBuild")
	if build == "" {
		build = str("CurrentBuildNumber")
	}
	if build == "" {
		return OSInfo{}
	}
	info := OSInfo{Version: str("DisplayVersion"), Build: build}
	if ubr, _, err := k.GetIntegerValue("UBR"); err == nil {
		info.Build += "." + strconv.FormatUint(ubr, 10)
	}
	product := str("ProductName")
	info.Server = strings.EqualFold(str("InstallationType"), "Server") || strings.Contains(product, "Server")
	switch n, _ := strconv.Atoi(build); {
	case info.Server && product != "":
		info.Name = product
	case info.Server:
		info.Name = "Windows Server"
	case n >= 22000:
		info.Name = strings.TrimSpace(fmt.Sprintf("Windows 11 %s", str("EditionID")))
	default:
		info.Name = strings.TrimSpace(fmt.Sprintf("Windows 10 %s", str("EditionID")))
	}
	return info
}
