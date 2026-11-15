package conformance

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// OSInfo is the operating system a runner actually is, as the system
// itself reports it. CurrentOS only maps a platform to the matrix's OS id;
// this is what the evidence records, so that a cell run on macOS 26 or on
// Windows Server is never labelled with the target's name alone.
type OSInfo struct {
	Name    string // "macOS", "Ubuntu 24.04.3 LTS", "Windows Server 2025 Datacenter"
	Version string // "26.2", "24.04", "24H2"
	Build   string // "25C56", "26100.4349"; "" on Linux
	// Server is true for a Windows Server installation.
	Server bool
}

// Known reports whether anything was observed.
func (i OSInfo) Known() bool { return i.Name != "" }

// describe is the evidence's os_observed: the OS as observed, then the Go
// platform, for example "macOS 26.2 (build 25C56) darwin/arm64".
func (i OSInfo) describe(goos, goarch string) string {
	platform := goos + "/" + goarch
	if !i.Known() {
		return platform
	}
	return i.describeShort() + " " + platform
}

// observeOS asks the system what it is. On a platform other than the one
// the options name (tests), only the given os-release content is used.
func (o *RunOptions) observeOS(ctx context.Context) OSInfo {
	switch o.GOOS {
	case "linux":
		kv := parseOSRelease(o.OSRelease)
		if kv["PRETTY_NAME"] == "" && kv["NAME"] == "" {
			return OSInfo{}
		}
		name := kv["PRETTY_NAME"]
		if name == "" {
			name = kv["NAME"]
		}
		return OSInfo{Name: name, Version: kv["VERSION_ID"]}
	case "darwin":
		if runtime.GOOS != "darwin" {
			return OSInfo{}
		}
		get := func(flag string) string {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out, code, err := o.exec(cctx, "", nil, "sw_vers", flag)
			if err != nil || code != 0 {
				return ""
			}
			return strings.TrimSpace(string(out))
		}
		v := get("-productVersion")
		if v == "" {
			return OSInfo{}
		}
		name := get("-productName")
		if name == "" {
			name = "macOS"
		}
		return OSInfo{Name: name, Version: v, Build: get("-buildVersion")}
	case "windows":
		if runtime.GOOS != "windows" {
			return OSInfo{}
		}
		return windowsInfo()
	}
	return OSInfo{}
}

// OSTargetMatch compares the runner with a cell's OS target, for the
// installed tier's evidence. It decides nothing about which cells run
// (CurrentOS does, for both tiers) and nothing about the live gate: it only
// says, truthfully, whether the runner is the target release.
//
//   - macos-14-arm64: exact on macOS 14; any other macOS release is family.
//   - windows-11-amd64: exact on a Windows 11 client (build 22000 or later,
//     not Server); Windows Server or an older build is family.
//   - ubuntu-24.04-amd64: CurrentOS already requires Ubuntu 24.04.
//
// note is empty exactly when match is exact.
func OSTargetMatch(target string, info OSInfo) (match, note string) {
	if !info.Known() {
		return OSMatchUnknown, "the runner's OS release could not be observed, so it is not known to be the target " + target
	}
	is := func(what string) (string, string) {
		return OSMatchFamily, fmt.Sprintf("this runner is %s, not the target %s (%s): the cell ran on the same OS family and architecture only", info.describeShort(), what, target)
	}
	switch target {
	case "macos-14-arm64":
		if major, _, _ := strings.Cut(info.Version, "."); major != "14" {
			return is("macOS 14")
		}
	case "windows-11-amd64":
		build, _, _ := strings.Cut(info.Build, ".")
		n, err := strconv.Atoi(build)
		if err != nil {
			return OSMatchUnknown, "the runner's Windows build could not be observed, so it is not known to be the target " + target
		}
		if info.Server || n < 22000 {
			return is("Windows 11")
		}
	case "ubuntu-24.04-amd64":
		if info.Version != "24.04" {
			return is("Ubuntu 24.04")
		}
	default:
		return OSMatchUnknown, "no rule says which OS release is the target " + target
	}
	return OSMatchExact, ""
}

// describeShort is the OS without the Go platform.
func (i OSInfo) describeShort() string {
	s := i.Name
	if i.Version != "" && !strings.Contains(i.Name, i.Version) {
		s += " " + i.Version
	}
	if i.Build != "" {
		s += " (build " + i.Build + ")"
	}
	return s
}
