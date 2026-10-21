package setup

import "testing"

func TestCodexListsKenaz(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"kenaz@kenaz  enabled\n", true},
		{"NAME   MARKETPLACE\nkenaz  kenaz\n", true},
		{"kenaz-other@acme  enabled\n", false},
		{"acme@kenaz-tools\n", false},
		{"", false},
	} {
		if got := codexListsKenaz(tc.out); got != tc.want {
			t.Errorf("codexListsKenaz(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}
