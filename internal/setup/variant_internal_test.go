package setup

import (
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// The build variant's names, as tests expect to see them.
var (
	tBin = variant.Current.Binary
	tMk  = variant.Current.Marketplace
	tID  = variant.Current.PluginID()
)

// skipWithoutPublicMarketplace skips tests of adopting the public GitHub
// marketplace in a build that has none (the staff dev build).
func skipWithoutPublicMarketplace(t *testing.T) {
	t.Helper()
	if OfficialRepo == "" {
		t.Skip("this build variant has no public marketplace")
	}
}

// skipOpenCode skips OpenCode tests in a build variant where OpenCode is
// always off.
func skipOpenCode(t *testing.T) {
	t.Helper()
	if variant.Current.Name != variant.Kenaz.Name {
		t.Skip("OpenCode is off in this build variant")
	}
}
