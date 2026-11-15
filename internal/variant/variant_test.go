package variant

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestIdentityComplete(t *testing.T) {
	for _, id := range []Identity{Kenaz, Current} {
		v := reflect.ValueOf(id)
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.Type.Kind() != reflect.String {
				continue
			}
			// Only the public build has a public marketplace; only a staff
			// build has a banner tag.
			if f.Name == "MarketplaceRepo" || (f.Name == "BannerTag" && !id.StaffOnly) {
				continue
			}
			if v.Field(i).String() == "" {
				t.Errorf("%s: %s is empty", id.Name, f.Name)
			}
		}
		if !id.Allowed(id.DefaultEnv) {
			t.Errorf("%s: default env %q is not allowed", id.Name, id.DefaultEnv)
		}
	}
	s := reflect.ValueOf(Shared)
	for i := 0; i < s.NumField(); i++ {
		if s.Field(i).IsZero() {
			t.Errorf("Shared.%s is empty", s.Type().Field(i).Name)
		}
	}
	if len(Shared.EnvVars) == 0 || Shared.EnvVars[0] != Shared.EnvVar {
		t.Errorf("EnvVars %v must start with EnvVar %q", Shared.EnvVars, Shared.EnvVar)
	}
	if len(Shared.RelayPorts) < 2 || Shared.RelayPorts[len(Shared.RelayPorts)-1] != 0 {
		t.Errorf("RelayPorts %v must end with an ephemeral port", Shared.RelayPorts)
	}
	if !strings.HasPrefix(Current.ApprovalsName(), Current.DisplayName) {
		t.Errorf("approvals name %q", Current.ApprovalsName())
	}
}

func TestPortsIsACopy(t *testing.T) {
	p := Shared.Ports()
	p[0] = 1
	if Shared.RelayPorts[0] == 1 {
		t.Fatal("Ports aliases RelayPorts")
	}
}

// sharedIdentityFields lists the Identity fields the builds may have in
// common. Every other field must differ between kenaz and kenaz-dev: the
// hosts (Claude Code, Codex) have one slot per plugin and marketplace name,
// and a machine can have both builds installed. Values every build uses
// belong in Shared, not in Identity.
var sharedIdentityFields = map[string]bool{
	"AllowedEnvs": true, // both connect to dev
}

// Every identity field differs between the builds, compared by
// reflection, so a field added later cannot be shared by accident.
func TestIdentityFieldsDiffer(t *testing.T) {
	a, b := reflect.ValueOf(Kenaz), reflect.ValueOf(KenazDev)
	for i := 0; i < a.NumField(); i++ {
		f := a.Type().Field(i)
		if sharedIdentityFields[f.Name] {
			continue
		}
		if reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
			t.Errorf("Identity.%s is %v in both builds (a shared value belongs in Shared)", f.Name, a.Field(i).Interface())
		}
	}
}

// The staff dev build never connects to prod and is never published.
func TestKenazDev(t *testing.T) {
	d := KenazDev
	if d.Allowed("prod") || !d.Allowed("dev") || len(d.AllowedEnvs) != 1 || d.DefaultEnv != "dev" {
		t.Fatalf("dev envs: default %q, allowed %v", d.DefaultEnv, d.AllowedEnvs)
	}
	if d.MarketplaceRepo != "" {
		t.Fatal("the dev build has a public marketplace")
	}
	if o, ok := d.For("prod"); !ok || o.Name != Kenaz.Name {
		t.Errorf("For(prod) = %v %v", o.Name, ok)
	}
	if !slices.Contains(Kenaz.AllowedEnvs, "prod") {
		t.Error("kenaz does not connect to prod")
	}
}

// The stage environment was retired: no build connects to it, and each
// build's refusal names only what that build can use.
func TestStageIsRetired(t *testing.T) {
	if !Retired("stage") || Retired("prod") || Retired("dev") || Retired("") {
		t.Fatalf("RetiredEnvs = %v", RetiredEnvs)
	}
	for _, v := range Variants {
		if v.Allowed("stage") {
			t.Errorf("%s still connects to stage", v.Name)
		}
		if Retired(v.DefaultEnv) {
			t.Errorf("%s defaults to a retired env", v.Name)
		}
	}
	if got := Kenaz.RetiredMessage("stage"); got != "The stage environment has been retired. Use dev or prod." {
		t.Errorf("kenaz: %q", got)
	}
	if got := KenazDev.RetiredMessage("stage"); got != "The stage environment has been retired. Use dev." {
		t.Errorf("kenaz-dev: %q", got)
	}
}

func TestNamed(t *testing.T) {
	for in, want := range map[string]string{"": "kenaz", "kenaz": "kenaz", "kenaz-dev": "kenaz-dev"} {
		if v, ok := Named(in); !ok || v.Name != want {
			t.Errorf("Named(%q) = %q %v", in, v.Name, ok)
		}
	}
	if _, ok := Named("other"); ok {
		t.Error("Named(other)")
	}
}

// Prod keeps the cookie name of earlier releases; the relays of other
// environments, which can run at the same time, get their own.
func TestApprovalsCSRFCookieFor(t *testing.T) {
	seen := map[string]string{}
	for _, env := range []string{"prod", "stage", "dev"} {
		c := Shared.ApprovalsCSRFCookieFor(env)
		if o, dup := seen[c]; dup {
			t.Errorf("%s and %s share cookie %s", o, env, c)
		}
		seen[c] = env
	}
	if Shared.ApprovalsCSRFCookieFor("prod") != Shared.ApprovalsCSRFCookie || Shared.ApprovalsCSRFCookieFor("") != Shared.ApprovalsCSRFCookie {
		t.Error("prod cookie changed")
	}
	if got := Shared.ApprovalsCSRFCookieFor("dev"); got != "kenaz_dev_approvals_csrf" {
		t.Errorf("dev cookie %q", got)
	}
}

func TestCommandNamesNonDefaultEnv(t *testing.T) {
	for _, tc := range []struct {
		id       Identity
		sub, env string
		want     string
	}{
		{Kenaz, "init", "prod", "kenaz init"},
		{Kenaz, "init", "dev", "kenaz init --env dev"},
		{Kenaz, "setup opencode", "stage", "kenaz setup opencode"}, // retired
		{Kenaz, "init", "", "kenaz init"},
		{Kenaz, "init", "elsewhere", "kenaz init"},
		{KenazDev, "init", "dev", "kenaz-dev init"},
		{KenazDev, "init", "stage", "kenaz-dev init"}, // retired
		{KenazDev, "init", "prod", "kenaz-dev init"},  // not one of its envs
	} {
		if got := tc.id.Command(tc.sub, tc.env); got != tc.want {
			t.Errorf("%s.Command(%q, %q) = %q, want %q", tc.id.Name, tc.sub, tc.env, got, tc.want)
		}
	}
}
