package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// providerErr shows the whole-plugin refusal with the plan-required text
// and a per-feature tier limit with the per-feature text; never Fleet's
// message, and never one in place of the other.
func TestProviderErrTierRefusals(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"whole plugin": {&fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Message: "fleet text", Details: map[string]string{"capability": fleetapi.PluginCapability}}, fleetapi.PlanRequiredMessage},
		"legacy":       {&fleetapi.APIError{Status: 403, Code: fleetapi.CodeLegacyEarlyAccessRequired, Message: "fleet text"}, fleetapi.PlanRequiredMessage},
		"feature":      {&fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Message: "fleet text", Details: map[string]string{"capability": "org_graph_sharing"}}, fleetapi.TierLimitMessage + " (feature: org_graph_sharing)"},
		"unnamed":      {&fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Message: "fleet text"}, fleetapi.TierLimitMessage},
	}
	for name, c := range cases {
		var errb bytes.Buffer
		if code := providerErr(Env{Stderr: &errb}, c.err); code != ExitError {
			t.Errorf("%s: exit %d", name, code)
		}
		got := strings.TrimSpace(errb.String())
		if !strings.HasSuffix(got, ": "+c.want) || strings.Contains(got, "fleet text") {
			t.Errorf("%s: %q", name, got)
		}
	}
}
