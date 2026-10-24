package secretscan_test

import (
	"testing"

	"github.com/kameas-ai/kenaz-plugin/pkg/secretscan"
)

func TestDetector(t *testing.T) {
	// Fake shapes built at runtime so no credential-like literal sits in
	// the source.
	pem := "-----BEGIN " + "RSA PRIVATE KEY-----"
	assign := "pass" + "word = hunter2hunter2"
	for _, s := range []string{pem, "AKIA" + "ABCDEFGHIJKLMNOP"} {
		if !secretscan.LooksLikeSecret(s) || !secretscan.LooksLikeCredentialForSync("title", s) {
			t.Errorf("missed a credential shape (len %d)", len(s))
		}
	}
	if secretscan.LooksLikeSecret(assign) {
		t.Error("the on-disk check should not flag a plain assignment")
	}
	if !secretscan.LooksLikeCredentialForSync("notes", assign) {
		t.Error("the sync check missed an assignment")
	}
	if secretscan.LooksLikeCredentialForSync("run make check before pushing") {
		t.Error("false positive")
	}
	if len(secretscan.Patterns()) == 0 || len(secretscan.SyncPatterns()) == 0 {
		t.Error("no patterns")
	}
}
