package app

import (
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// With the file credential store (no OS keychain) there is no key outside
// user-writable files: the approvals store gets none, so approvals report
// approval_unavailable instead of trusting files an agent could write.
func TestApprovalsStoreKeyNeedsKeychain(t *testing.T) {
	ks := &spool.Memory{}
	files := Env{KeyStore: ks, cred: &credStore{store: credstore.Files}}
	if k := approvalsStoreKey(files, fleetapi.EnvDev, true); k != nil {
		t.Fatal("file credential store produced an approvals key")
	}
	kc := Env{KeyStore: ks, cred: &credStore{store: credstore.Keychain}}
	k := approvalsStoreKey(kc, fleetapi.EnvDev, true)
	if len(k) != 32 {
		t.Fatalf("keychain key: %d bytes", len(k))
	}
	if again := approvalsStoreKey(kc, fleetapi.EnvDev, false); string(again) != string(k) {
		t.Fatal("key not stable")
	}
	if _, err := ks.Get(string(fleetapi.EnvDev)); err == nil {
		t.Fatal("approvals key stored under the spool key's name")
	}
}
