package secretscan

import (
	"strings"
	"testing"
)

// Every pattern has a rule name.
func TestRulesMatchPatterns(t *testing.T) {
	if len(credentialRules) != len(credentialPatterns) || len(syncCredentialRules) != len(syncCredentialPatterns) {
		t.Fatalf("rules %d/%d, patterns %d/%d", len(credentialRules), len(syncCredentialRules), len(credentialPatterns), len(syncCredentialPatterns))
	}
}

// A finding names the kind and the line, in line order, and has no field that
// could hold the text.
func TestFind(t *testing.T) {
	const gh = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	got := Find("ok\n" + gh + "\nok\nAKIAABCDEFGHIJKLMNOP " + gh + "\n")
	want := []Finding{{"github_token", 2}, {"cloud_access_key", 4}, {"github_token", 4}}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%+v", got)
		}
	}
	if Find("plain text\npassword: hunter22222") != nil {
		t.Fatal("the on-device scan should not flag an assignment")
	}
	sync := FindForSync("plain text\npassword: hunter22222")
	if len(sync) != 1 || sync[0] != (Finding{"assigned_secret", 2}) {
		t.Fatalf("%+v", sync)
	}
	if !LooksLikeCredentialForSync("password: hunter22222") {
		t.Fatal("Find and LooksLike disagree")
	}
}

func TestFindIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("AKIAABCDEFGHIJKLMNOP\n")
	}
	if got := Find(b.String()); len(got) != maxFindings {
		t.Fatalf("%d findings", len(got))
	}
}
