package variant

import (
	"strings"
	"testing"
)

func TestIdentityComplete(t *testing.T) {
	for _, id := range []Identity{Kenaz, Current} {
		for k, v := range map[string]string{
			"Name": id.Name, "Binary": id.Binary, "DisplayName": id.DisplayName, "BundleName": id.BundleName,
			"Marketplace": id.Marketplace, "Plugin": id.Plugin, "MCPServer": id.MCPServer, "ConfigDir": id.ConfigDir,
			"KeychainService": id.KeychainService, "CaskToken": id.CaskToken, "EnvVar": id.EnvVar, "HomeVar": id.HomeVar,
			"StateDirVar": id.StateDirVar, "MCPHostVar": id.MCPHostVar, "DefaultEnv": id.DefaultEnv,
			"ApprovalsCSRFCookie": id.ApprovalsCSRFCookie, "EnrollCookiePrefix": id.EnrollCookiePrefix,
		} {
			if v == "" {
				t.Errorf("%s: %s is empty", id.Name, k)
			}
		}
		if !id.Allowed(id.DefaultEnv) {
			t.Errorf("%s: default env %q is not allowed", id.Name, id.DefaultEnv)
		}
		if len(id.EnvVars) == 0 || id.EnvVars[0] != id.EnvVar {
			t.Errorf("%s: EnvVars %v must start with EnvVar %q", id.Name, id.EnvVars, id.EnvVar)
		}
		if len(id.RelayPorts) < 2 || id.RelayPorts[len(id.RelayPorts)-1] != 0 {
			t.Errorf("%s: RelayPorts %v must end with an ephemeral port", id.Name, id.RelayPorts)
		}
		if !strings.HasPrefix(id.ApprovalsName(), id.DisplayName) {
			t.Errorf("%s: approvals name %q", id.Name, id.ApprovalsName())
		}
	}
}

func TestPortsIsACopy(t *testing.T) {
	p := Current.Ports()
	p[0] = 1
	if Current.RelayPorts[0] == 1 {
		t.Fatal("Ports aliases RelayPorts")
	}
}
