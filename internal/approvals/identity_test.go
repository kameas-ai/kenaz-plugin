package approvals

import (
	"context"
	"strings"
	"testing"
)

var wantIdentity = map[string]string{
	"csrf cookie":          "kenaz_approvals_csrf",
	"enroll cookie prefix": "kenaz_enroll_",
	"webauthn name":        "Kenaz approvals",
	"webauthn display":     "Kenaz approvals",
	"rp display name":      "Kenaz",
	"cross-site title":     "Kenaz approvals",
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
		"csrf cookie":          csrfCookie,
		"enroll cookie prefix": enrollCookiePrefix,
		"webauthn name":        waUser{}.WebAuthnName(),
		"webauthn display":     waUser{}.WebAuthnDisplayName(),
		"rp display name":      m.opt.RPDisplayName,
		"cross-site title":     strings.SplitN(CrossSiteMessage, ":", 2)[0],
	}
	for k, w := range wantIdentity {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}
