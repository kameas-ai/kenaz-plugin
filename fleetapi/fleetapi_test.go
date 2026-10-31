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

// Plan required: the wire codes and the approved text.
func TestPlanRequired(t *testing.T) {
	if fleetapi.CodeCapabilityNotInTier != "capability_not_in_tier" {
		t.Fatalf("code = %q", fleetapi.CodeCapabilityNotInTier)
	}
	if fleetapi.CodeLegacyEarlyAccessRequired != "fleet_plugin_early_access_required" {
		t.Fatalf("legacy code = %q", fleetapi.CodeLegacyEarlyAccessRequired)
	}
	if fleetapi.PlanRequiredMessage != "Kenaz for Claude Code and Codex needs a Kameas Fleet plan that includes it. Ask your organization's Kameas Fleet administrator." {
		t.Fatalf("message changed: %q", fleetapi.PlanRequiredMessage)
	}
	if strings.Contains(strings.ToLower(fleetapi.PlanRequiredMessage), "early access") {
		t.Fatal("the plan-required text must not mention early access")
	}
	if fleetapi.PluginCapability != "fleet_plugin" {
		t.Fatalf("plugin capability = %q", fleetapi.PluginCapability)
	}
	for name, err := range map[string]*fleetapi.APIError{
		"whole plugin": {Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Details: map[string]string{"capability": fleetapi.PluginCapability}},
		"legacy":       {Status: 403, Code: fleetapi.CodeLegacyEarlyAccessRequired},
	} {
		if !fleetapi.PlanRequired(errors.Join(errors.New("wrap"), err)) {
			t.Fatalf("wrapped %s not recognised", name)
		}
		if _, ok := fleetapi.TierLimit(err); ok {
			t.Fatalf("%s reported as a per-feature tier limit", name)
		}
	}
	if fleetapi.PlanRequired(&fleetapi.APIError{Status: 403, Code: fleetapi.CodeOrgPaused}) || fleetapi.PlanRequired(errors.New("x")) {
		t.Fatal("other errors reported as plan required")
	}
}

// Every capability_not_in_tier that does not name the plugin capability is
// a per-feature limit, not the whole-plugin refusal.
func TestTierLimit(t *testing.T) {
	cases := map[string]struct {
		details map[string]string
		want    string
	}{
		"other capability": {map[string]string{"capability": "org_graph_sharing"}, "org_graph_sharing"},
		"no capability":    {nil, ""},
		"empty capability": {map[string]string{"capability": ""}, ""},
	}
	for name, c := range cases {
		err := errors.Join(errors.New("wrap"), &fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Details: c.details})
		if fleetapi.PlanRequired(err) {
			t.Errorf("%s: reported as plan required", name)
		}
		got, ok := fleetapi.TierLimit(err)
		if !ok || got != c.want {
			t.Errorf("%s: TierLimit = %q, %v", name, got, ok)
		}
	}
	if _, ok := fleetapi.TierLimit(&fleetapi.APIError{Status: 403, Code: fleetapi.CodeOrgPaused}); ok {
		t.Error("org pause reported as a tier limit")
	}
	if _, ok := fleetapi.TierLimit(errors.New("x")); ok {
		t.Error("plain error reported as a tier limit")
	}
}

func TestTierLimitText(t *testing.T) {
	const base = "Your organization's Kameas Fleet plan doesn't include this feature."
	if fleetapi.TierLimitMessage != base {
		t.Fatalf("message changed: %q", fleetapi.TierLimitMessage)
	}
	if got := fleetapi.TierLimitText("org_graph_sharing"); got != base+" (feature: org_graph_sharing)" {
		t.Fatalf("named = %q", got)
	}
	for _, bad := range []string{"", "Org", "9abc", "has space", "a-b", "x\x07", strings.Repeat("a", 65), "<b>hi</b>"} {
		if got := fleetapi.TierLimitText(bad); got != base {
			t.Errorf("%q: %q", bad, got)
		}
	}
	if got := fleetapi.TierLimitText(strings.Repeat("a", 64)); got == base {
		t.Error("64-char name must be shown")
	}
}

// The renamed exports stay as deprecated aliases.
func TestDeprecatedAliases(t *testing.T) {
	if fleetapi.CodeEarlyAccessRequired != fleetapi.CodeLegacyEarlyAccessRequired {
		t.Fatal("CodeEarlyAccessRequired alias")
	}
	if fleetapi.EarlyAccessMessage != fleetapi.PlanRequiredMessage {
		t.Fatal("EarlyAccessMessage alias")
	}
	if !fleetapi.EarlyAccessRequired(&fleetapi.APIError{Code: fleetapi.CodeLegacyEarlyAccessRequired}) ||
		fleetapi.EarlyAccessRequired(&fleetapi.APIError{Code: fleetapi.CodeCapabilityNotInTier}) {
		t.Fatal("EarlyAccessRequired alias")
	}
}
