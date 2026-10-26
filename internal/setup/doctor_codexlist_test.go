package setup

import (
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

func TestCodexListsKenaz(t *testing.T) {
	pl := variant.Current.Plugin
	cases := []struct {
		out  string
		want bool
	}{
		{tID + "  enabled\n", true},
		{"NAME   MARKETPLACE\n" + pl + "  " + tMk + "\n", true},
		{pl + "-other@acme  enabled\n", false},
		{"acme@" + pl + "-tools\n", false},
		{"", false},
	}
	// Another build variant's plugin is not this one.
	for _, v := range variant.Variants {
		if v.Name != variant.Current.Name {
			cases = append(cases, struct {
				out  string
				want bool
			}{v.PluginID() + "  enabled\n", false})
		}
	}
	for _, tc := range cases {
		if got := codexListsKenaz(tc.out); got != tc.want {
			t.Errorf("codexListsKenaz(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}
