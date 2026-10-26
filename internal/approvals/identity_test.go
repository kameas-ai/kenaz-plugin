package approvals

import (
	"context"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// csrfCookie is the CSRF cookie of the test managers (Env "" is prod).
var csrfCookie = variant.Shared.ApprovalsCSRFCookie

var wantIdentities = map[string]map[string]string{
	"kenaz": {
		"csrf cookie":          "kenaz_approvals_csrf",
		"dev csrf cookie":      "kenaz_dev_approvals_csrf",
		"enroll cookie prefix": "kenaz_enroll_",
		"webauthn name":        "Kenaz approvals",
		"webauthn display":     "Kenaz approvals",
		"rp display name":      "Kenaz",
		"cross-site title":     "Kenaz approvals",
	},
	// The cookies belong to the relay's Fleet environment, not the build.
	"kenaz-dev": {
		"csrf cookie":          "kenaz_approvals_csrf",
		"dev csrf cookie":      "kenaz_dev_approvals_csrf",
		"enroll cookie prefix": "kenaz_enroll_",
		"webauthn name":        "Kenaz (dev) approvals",
		"webauthn display":     "Kenaz (dev) approvals",
		"rp display name":      "Kenaz (dev)",
		"cross-site title":     "Kenaz (dev) approvals",
	},
}

func newIdentityManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(Options{Dir: t.TempDir(), StoreKey: testKey, BaseURL: func() string { return testOrigin },
		Identity: func(context.Context) (Identity, error) { return alice, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The approval page's browser-visible identity: cookie names (shared by
// every page on localhost, whatever the port) and what the authenticator
// shows.
func TestIdentityNames(t *testing.T) {
	m := newIdentityManager(t)
	got := map[string]string{
		"csrf cookie":          m.csrfCookie(),
		"dev csrf cookie":      (&Manager{opt: Options{Env: "dev"}}).csrfCookie(),
		"enroll cookie prefix": enrollCookiePrefix,
		"webauthn name":        waUser{}.WebAuthnName(),
		"webauthn display":     waUser{}.WebAuthnDisplayName(),
		"rp display name":      m.opt.RPDisplayName,
		"cross-site title":     strings.SplitN(CrossSiteMessage, ":", 2)[0],
	}
	wantIdentity := wantIdentities[variant.Current.Name]
	if len(wantIdentity) == 0 {
		t.Fatalf("no expected identity for variant %q", variant.Current.Name)
	}
	for k, w := range wantIdentity {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

// Relays of different Fleet environments run side by side on one machine
// and never share a CSRF cookie name: a page on one relay's port would
// overwrite the other's cookie. Enrollment cookies carry a random ceremony
// id.
func TestEnvCookiesDiffer(t *testing.T) {
	seen := map[string]string{}
	for _, env := range []string{"prod", "stage", "dev"} {
		c := (&Manager{opt: Options{Env: env}}).csrfCookie()
		for prev, by := range seen {
			if strings.HasPrefix(c, prev) || strings.HasPrefix(prev, c) {
				t.Errorf("%s cookie %q collides with %s's %q", env, c, by, prev)
			}
		}
		seen[c] = env
	}
}
