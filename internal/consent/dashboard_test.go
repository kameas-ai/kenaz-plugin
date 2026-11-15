package consent

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Approval links open the selected environment's
// dashboard in prod and dev.
func TestDashboardURLPerEnvironment(t *testing.T) {
	want := map[fleetapi.Env]string{
		fleetapi.EnvProd: "https://fleet.kameas.ai",
		fleetapi.EnvDev:  "https://dev.fleet.kameas.ai",
	}
	for env, base := range want {
		if got := DashboardBase(env); got != base {
			t.Errorf("%s: base %q, want %q", env, got, base)
		}
		if got := DashboardURL(env, "/settings#hosted-inference"); got != base+"/settings#hosted-inference" {
			t.Errorf("%s: link %q", env, got)
		}
		if got := DashboardURL(env, ""); got != base+"/" {
			t.Errorf("%s: no link → %q, want dashboard root", env, got)
		}
	}
	if DashboardBase("qa") != "" || DashboardURL("", "/x") != "" {
		t.Error("an unknown env must not resolve to another environment's dashboard")
	}
}

// A retired environment has no dashboard: no link to one is ever built.
// The origins are exactly the environments some build still connects to,
// so no entry can outlive its environment unnoticed.
func TestNoDashboardForARetiredEnvironment(t *testing.T) {
	for _, env := range variant.RetiredEnvs {
		if got := DashboardBase(fleetapi.Env(env)); got != "" {
			t.Errorf("%s: dashboard %q", env, got)
		}
		if got := DashboardURL(fleetapi.Env(env), "/settings"); got != "" {
			t.Errorf("%s: link %q", env, got)
		}
		if got := ApprovalURL(fleetapi.Env(env), fleetapi.Approval{Link: "/x"}); got != "" {
			t.Errorf("%s: approval link %q", env, got)
		}
	}
	connected := map[string]bool{}
	for _, v := range variant.Variants {
		for _, env := range v.AllowedEnvs {
			connected[env] = true
			if DashboardBase(fleetapi.Env(env)) == "" {
				t.Errorf("%s has no dashboard", env)
			}
		}
	}
	for env, origin := range dashboardOrigins {
		if !connected[string(env)] || variant.Retired(string(env)) {
			t.Errorf("%s (%s) is an environment no build connects to", env, origin)
		}
	}
}

// Only dashboard-relative links are followed; anything that could leave the
// selected environment's origin falls back to its dashboard root.
func TestDashboardURLRejectsForeignLinks(t *testing.T) {
	for _, link := range []string{
		"https://evil.example/approve", "//evil.example/x", "/\\evil.example", "settings",
		"/a b", "/x\x00", "javascript:alert(1)", "/" + strings.Repeat("a", 600),
	} {
		if got := DashboardURL(fleetapi.EnvDev, link); got != "https://dev.fleet.kameas.ai/" {
			t.Errorf("link %q resolved to %q", link, got)
		}
	}
}

// The status model, `kenaz status` text and the SessionStart banner carry
// the selected environment's link, never another environment's.
func TestPendingApprovalLinksFollowEnv(t *testing.T) {
	for _, env := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvDev} {
		f := newFake(allClasses()...)
		f.approvals = []fleetapi.Approval{
			{ID: "a1", Kind: "legal_acceptance", Title: "Terms", Blocking: true, Required: true},
			{ID: "a2", Kind: fleetapi.ApprovalKindMLOptInAvailable, Title: "Hosted inference", Link: "/settings#hosted-inference"},
		}
		m := NewManager(Options{Provider: f, Env: env, ProjectionVersion: 1, Store: NewStore(filepath.Join(t.TempDir(), "c.json"))})
		if err := m.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		st := m.State()
		base := DashboardBase(env)
		if st.DashboardURL != base+"/" || st.Pending[0].URL != base+"/" || st.Pending[1].URL != base+"/settings#hosted-inference" {
			t.Fatalf("%s: %+v", env, st)
		}
		var b bytes.Buffer
		WriteText(&b, st)
		if !strings.Contains(b.String(), base+"/settings#hosted-inference") {
			t.Fatalf("%s: status text has no link:\n%s", env, b.String())
		}
		if banner := SessionStartContext(st); !strings.Contains(banner, base+"/") {
			t.Fatalf("%s: banner has no link: %s", env, banner)
		}
		for other, ob := range dashboardOrigins {
			if other != env && strings.Contains(b.String()+SessionStartContext(st), ob+"/") {
				t.Fatalf("%s output links to %s", env, other)
			}
		}
	}
}
