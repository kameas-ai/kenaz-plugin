package consent

import (
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// dashboardOrigins are the Kameas Fleet dashboard (SPA) origins per
// environment: the "SPA host" column of Fleet's API host table and the
// fleet-admin --env mapping. The API hosts (api.<env>...) are the
// provider implementation's; these are only ever shown to the member as
// links to open in a browser.
var dashboardOrigins = map[fleetapi.Env]string{
	fleetapi.EnvProd:  "https://fleet.kameas.ai",
	fleetapi.EnvStage: "https://stage.fleet.kameas.ai",
	fleetapi.EnvDev:   "https://dev.fleet.kameas.ai",
}

// DashboardBase returns the dashboard origin for env (no trailing slash).
// An unknown env returns "" so a caller never sends a member of one
// environment to another environment's dashboard.
func DashboardBase(env fleetapi.Env) string { return dashboardOrigins[env] }

// DashboardURL resolves a dashboard-relative approval link against env's
// dashboard. Fleet's contract makes `link` a path that starts with a single
// "/"; anything else (absolute URLs, "//host", backslashes, control
// characters) is ignored and the dashboard root is returned instead, where
// the pending-approvals banner lists every item. It returns "" for an
// unknown env.
func DashboardURL(env fleetapi.Env, link string) string {
	base := DashboardBase(env)
	if base == "" {
		return ""
	}
	if !dashboardRelative(link) {
		return base + "/"
	}
	return base + link
}

func dashboardRelative(link string) bool {
	if len(link) < 1 || link[0] != '/' || strings.HasPrefix(link, "//") || len(link) > 512 {
		return false
	}
	for _, r := range link {
		if r <= ' ' || r == 0x7f || r == '\\' {
			return false
		}
	}
	return true
}

// ApprovalURL is where the member reviews approval a in env.
func ApprovalURL(env fleetapi.Env, a fleetapi.Approval) string {
	return DashboardURL(env, a.Link)
}
