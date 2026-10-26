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
	"AllowedEnvs": true, // both connect to dev and stage
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
	if d.Allowed("prod") || !d.Allowed("dev") || !d.Allowed("stage") || d.DefaultEnv != "dev" {
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
