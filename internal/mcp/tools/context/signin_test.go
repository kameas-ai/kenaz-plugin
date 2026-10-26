package contexttools

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Claude Desktop chat has no /kenaz:login: a signed-out tool call there
// says how to run `kenaz login` instead.
func TestNotSignedInRemedyPerHost(t *testing.T) {
	t.Setenv(mcp.HostEnvVar, "")
	errs := []error{fleetapi.ErrNotSignedIn, &fleetapi.APIError{Status: http.StatusUnauthorized}}
	for _, err := range errs {
		if r := MapError(err).Remedy; r != "Run /"+variant.Current.Plugin+":login." {
			t.Fatalf("Claude Code remedy for %v = %q", err, r)
		}
	}
	t.Setenv(mcp.HostEnvVar, mcp.HostClaudeDesktop)
	t.Setenv(variant.Shared.EnvVar, variant.Current.DefaultEnv)
	for _, err := range errs {
		te := MapError(err)
		if te.Code != CodeNotSignedIn || !strings.Contains(te.Remedy, " login`") || strings.Contains(te.Remedy, "/"+variant.Current.Plugin+":login") {
			t.Fatalf("Claude Desktop error for %v = %+v", err, te)
		}
	}
}
