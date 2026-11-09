package approvals

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A store from the passkey build: credentials, a proof on each approved row, every status.
func TestOpenStoreWrittenByPasskeyBuild(t *testing.T) {
	h := newHarness(t)
	h.m.RegisterReconciler("context.share", func(context.Context, []byte, string) ([]byte, bool, error) { return nil, false, nil })
	exp := h.clk.Now().Add(time.Minute)
	mk := func(id string, st Status) map[string]any {
		p := []byte(`{"x":"` + id + `"}`)
		return map[string]any{
			"id": id, "operation": "context.share", "identity": alice, "payload": p, "payload_sha256": payloadHash(p), "idempotency_key": "k" + id,
			"summary": "s", "status": st, "profile_gen": uint64(1), "created_at": h.clk.Now(), "updated_at": h.clk.Now(), "expires_at": exp,
			"receipt_sha256": "abcd", "proof": map[string]any{"nonce": []byte("n"), "origin": "http://localhost:1", "assertion": []byte("{}")},
		}
	}
	ids := map[string]Status{
		"apr_pendingaaaaaaaaaaaaaaaaaaaa": StatusPending, "apr_approvedaaaaaaaaaaaaaaaaaaa": StatusApproved,
		"apr_executingaaaaaaaaaaaaaaaaaa": StatusExecuting, "apr_reconcilingaaaaaaaaaaaaaaaa": StatusReconciling,
		"apr_deniedaaaaaaaaaaaaaaaaaaaaa": StatusDenied, "apr_doneaaaaaaaaaaaaaaaaaaaaaaa": StatusDone,
	}
	recs := map[string]any{}
	for id, st := range ids {
		recs[id] = mk(id, st)
	}
	inner, _ := json.Marshal(map[string]any{"version": storeVersion, "profile_gen": 1, "bound": alice, "cred_gen": 3,
		"user_handles": map[string][]byte{"x": []byte("h")}, "credentials": []any{map[string]any{"identity": alice, "credential": map[string]any{"id": "AAAA"}}}, "records": recs})
	s := &store{key: testKey}
	b, _ := json.Marshal(sealedFile{Version: sealedVersion, State: inner, MAC: s.mac(inner)})
	if err := os.WriteFile(filepath.Join(h.dir, StateFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	h.who.Store(alice)
	h.m = h.open()
	execs := 0
	h.execHook = func(context.Context, []byte, string) ([]byte, error) { execs++; return []byte(`{}`), nil }
	for id, was := range ids {
		st := h.status(id)
		if was == StatusApproved && st != StatusCanceled {
			t.Errorf("approved row: %s", st)
		}
		if was == StatusPending && st != StatusPending {
			t.Errorf("pending row: %s", st)
		}
	}
	if execs != 0 {
		t.Fatalf("%d ran at open", execs)
	}
	h.m.RegisterReconciler("context.share", func(context.Context, []byte, string) ([]byte, bool, error) { return nil, false, nil })
	h.m.sweep(context.Background())
	if st := h.status("apr_pendingaaaaaaaaaaaaaaaaaaaa"); st != StatusPending {
		t.Fatalf("pending ran on its own: %s", st)
	}
	raw, _ := os.ReadFile(filepath.Join(h.dir, StateFile))
	if strings.Contains(string(raw), "credentials") || strings.Contains(string(raw), "proof") {
		t.Errorf("old fields kept")
	}
	// An old pending row can still be pressed, once.
	if st, err := h.m.Approve(context.Background(), "apr_pendingaaaaaaaaaaaaaaaaaaaa"); err != nil || st != StatusDone {
		t.Fatalf("%s %v", st, err)
	}
	if st, _ := h.m.Approve(context.Background(), "apr_pendingaaaaaaaaaaaaaaaaaaaa"); st != StatusDone {
		t.Fatalf("second press: %s", st)
	}
	if st, err := h.m.Approve(context.Background(), "apr_deniedaaaaaaaaaaaaaaaaaaaaa"); err == nil && st == StatusDone {
		t.Fatalf("denied row approved")
	}
}

// Press twice at once; a key reused with other content; a press after expiry.
func TestPressRunsOnce(t *testing.T) {
	h := newHarness(t)
	id := h.prepare(req(alice, "k1", "x"))
	block := make(chan struct{})
	h.execHook = func(context.Context, []byte, string) ([]byte, error) { <-block; return []byte(`{}`), nil }
	res := make(chan Status, 8)
	for i := 0; i < 8; i++ {
		go func() { st, _ := h.m.Approve(context.Background(), id); res <- st }()
	}
	time.Sleep(50 * time.Millisecond)
	close(block)
	for i := 0; i < 8; i++ {
		<-res
	}
	if n := h.execs.Load(); n != 1 {
		t.Fatalf("ran %d times", n)
	}
	id2 := h.prepare(req(alice, "k2", "y"))
	h.clk.Advance(DefaultTTL + time.Second)
	if st, err := h.m.Approve(context.Background(), id2); err == nil && st == StatusDone {
		t.Fatal("ran after expiry")
	}
	id3 := h.prepare(req(alice, "k3", "z"))
	h.who.Store(bob)
	if st, err := h.m.Approve(context.Background(), id3); err == nil && st == StatusDone {
		t.Fatal("ran for another account")
	}
}
