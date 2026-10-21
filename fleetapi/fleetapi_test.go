package fleetapi_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/allowlist"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
)

func TestEnrollBodyValidate(t *testing.T) {
	ok := fleetapitest.SampleEnrollBody()
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
	cases := map[string]func(*fleetapi.EnrollBody){
		"no node id":      func(b *fleetapi.EnrollBody) { b.NodeID = "" },
		"bad platform":    func(b *fleetapi.EnrollBody) { b.Platform = "plan9" },
		"bad arch":        func(b *fleetapi.EnrollBody) { b.Arch = "386" },
		"harness kind":    func(b *fleetapi.EnrollBody) { b.ClientKind = "codex" },
		"empty name":      func(b *fleetapi.EnrollBody) { b.Hostname = "" },
		"long name":       func(b *fleetapi.EnrollBody) { b.Hostname = strings.Repeat("x", 65) },
		"control in name": func(b *fleetapi.EnrollBody) { b.Hostname = "a\nb" },
		"no version":      func(b *fleetapi.EnrollBody) { b.Version = "" },
	}
	for name, mut := range cases {
		b := fleetapitest.SampleEnrollBody()
		mut(&b)
		if b.Validate() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestEnrollBodyKeys(t *testing.T) {
	got := strings.Join(allowlist.EnrollBodyKeys(), ",")
	want := "arch,client_kind,hostname,in_vm,node_id,os_version,platform,version,vm_id"
	if got != want {
		t.Fatalf("EnrollBodyKeys = %s, want %s (changing this changes what is sent: needs review and a disclosure update)", got, want)
	}
}

func TestOrgPaused(t *testing.T) {
	err := &fleetapi.APIError{Status: 403, Code: fleetapi.CodeOrgPaused, Details: map[string]string{"paused_category": "telemetry"}}
	cat, ok := fleetapi.OrgPaused(errors.Join(errors.New("wrap"), err))
	if !ok || cat != "telemetry" {
		t.Fatalf("OrgPaused = %q, %v", cat, ok)
	}
	if _, ok := fleetapi.OrgPaused(errors.New("other")); ok {
		t.Fatal("non-API error reported as paused")
	}
}

func TestEnvValid(t *testing.T) {
	for _, e := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvStage, fleetapi.EnvDev} {
		if !e.Valid() {
			t.Errorf("%s invalid", e)
		}
	}
	if fleetapi.Env("qa").Valid() {
		t.Error("qa valid")
	}
}

// Early access: the wire code and the approved text.
func TestEarlyAccessRequired(t *testing.T) {
	if fleetapi.CodeEarlyAccessRequired != "fleet_plugin_early_access_required" {
		t.Fatalf("code = %q", fleetapi.CodeEarlyAccessRequired)
	}
	if fleetapi.EarlyAccessMessage != "Kenaz for Claude Code and Codex is in early access. Contact sales@kameas.ai to join." {
		t.Fatalf("message changed without legal: %q", fleetapi.EarlyAccessMessage)
	}
	err := &fleetapi.APIError{Status: 403, Code: fleetapi.CodeEarlyAccessRequired}
	if !fleetapi.EarlyAccessRequired(errors.Join(errors.New("wrap"), err)) {
		t.Fatal("wrapped early-access error not recognised")
	}
	if fleetapi.EarlyAccessRequired(&fleetapi.APIError{Status: 403, Code: fleetapi.CodeOrgPaused}) || fleetapi.EarlyAccessRequired(errors.New("x")) {
		t.Fatal("other errors reported as early access")
	}
}
