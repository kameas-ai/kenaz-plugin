package app

import (
	"runtime"
	"runtime/debug"
	"sort"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// staffOnlyNote is what status and doctor say about a staff-only build.
const staffOnlyNote = "STAFF ONLY: not for customer data"

// Release hooks for the official release pipeline. Official builds set these before calling Run; public builds
// leave them empty. They are configuration, not behavior: the public CLI
// owns the `licenses` and `version --json` surfaces.

// License is one text printed by `kenaz licenses`.
type License struct {
	Name string
	Text string
}

// ExtraLicenses are appended by official builds: LICENSE (Kameas Client
// Software License) and the proprietary module's notices.
var ExtraLicenses []License

// Provenance link-time values (-X github.com/kameas-ai/kenaz-plugin/app.<Name>=...),
// alongside BuildCommit and PrivateModuleVersion in runtime.go.
var (
	PublicModuleVersion = ""
	PrivateCommit       = ""
	BuildDate           = ""
)

// BuildInfo is `version --json` (the schema the official release pipeline's
// checks and scripts/verify-release.sh read).
type BuildInfo struct {
	Version string `json:"version"`
	Flavor  string `json:"flavor"`
	// Variant is the build variant's name (variant.Identity.Name): kenaz,
	// or kenaz-dev for the staff dev build.
	Variant              string   `json:"variant"`
	PublicCommit         string   `json:"public_commit"`
	PublicModuleVersion  string   `json:"public_module_version"`
	PrivateModuleVersion string   `json:"private_module_version"`
	PrivateCommit        string   `json:"private_commit"`
	BuildDate            string   `json:"build_date"`
	GoVersion            string   `json:"go_version"`
	Modules              []string `json:"modules"`
}

// CurrentBuildInfo reports this binary's provenance. Unset values read
// "unknown" so a release gate can reject placeholders.
func CurrentBuildInfo(p fleetapi.Provider) BuildInfo {
	or := func(s string) string {
		if s == "" {
			return "unknown"
		}
		return s
	}
	bi := BuildInfo{
		Version:              Version,
		Flavor:               "unofficial",
		Variant:              variant.Current.Name,
		PublicCommit:         or(BuildCommit),
		PublicModuleVersion:  or(PublicModuleVersion),
		PrivateModuleVersion: or(PrivateModuleVersion),
		PrivateCommit:        or(PrivateCommit),
		BuildDate:            or(BuildDate),
		GoVersion:            runtime.Version(),
	}
	if p != nil && p.Flavor() == fleetapi.FlavorOfficial {
		bi.Flavor = "official"
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, m := range info.Deps {
			v := m.Version
			if m.Replace != nil {
				v += " => " + m.Replace.Path + " " + m.Replace.Version
			}
			bi.Modules = append(bi.Modules, m.Path+" "+v)
		}
		sort.Strings(bi.Modules)
	}
	return bi
}
