package app

import (
	"context"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// Enrollment accepts only a sign-in completed during the ceremony; a
// reused identity-provider session (old or missing auth_time) is refused.
func TestIdentityVerifierRequiresFreshSignIn(t *testing.T) {
	fake := &fleetapitest.IdentityVerifier{URL: "https://id.example/authorize", Identity: fleetapi.VerifiedIdentity{Env: fleetapi.EnvDev, UserID: "u", OrgID: "o"}}
	iv := identityVerifier{v: fake, env: fleetapi.EnvDev}
	var opened string
	open := func(u string) error { opened = u; return nil }
	const completeURL = "http://localhost:4318/approvals/enroll/complete?c=tok"
	if _, err := iv.VerifyIdentity(context.Background(), open, completeURL); err == nil {
		t.Fatal("missing auth_time accepted")
	}
	fake.Identity.AuthTime = time.Now().Add(-time.Hour)
	if _, err := iv.VerifyIdentity(context.Background(), open, completeURL); err == nil {
		t.Fatal("stale auth_time accepted")
	}
	fake.Identity.AuthTime = time.Now()
	got, err := iv.VerifyIdentity(context.Background(), open, completeURL)
	if err != nil || got.UserID != "u" || got.OrgID != "o" || got.Env != "dev" || opened == "" || fake.CompleteURL != completeURL {
		t.Fatalf("fresh: %+v %v %q", got, err, opened)
	}
	// Public builds never offer enrollment, even though the stub has the method.
	if verifierFor(stub.New(), fleetapi.EnvDev) != nil {
		t.Fatal("stub build offers enrollment")
	}
}

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
