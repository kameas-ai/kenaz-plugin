package contextlib

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var grantKey = bytes.Repeat([]byte{3}, 32)

// testBinding stands in for the keychain.
type testBinding struct {
	cur []byte
	n   byte
}

func (b *testBinding) Current() []byte { return b.cur }
func (b *testBinding) Renew() error {
	b.n++
	b.cur = bytes.Repeat([]byte{b.n}, 32)
	return nil
}
func (b *testBinding) Forget() error { b.cur = nil; return nil }

func newTestGrant(dir string, key []byte) ShareGrant {
	return NewShareGrant(dir, key, &testBinding{})
}

// A copy of the grant's file put back after the grant ended is no grant: turning it
// off forgets what the file was bound to, and turning it on again binds a new file.
func TestShareGrantRestoredFileIsNoGrant(t *testing.T) {
	dir := t.TempDir()
	bind := &testBinding{}
	g := NewShareGrant(dir, grantKey, bind)
	a := Account{Env: "dev", OrgID: "o1", UserID: "u1"}
	if err := g.Give(a, time.Now()); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, grantFile))
	if err != nil {
		t.Fatal(err)
	}
	restore := func() { os.WriteFile(filepath.Join(dir, grantFile), saved, 0o600) }
	if err := g.Revoke(); err != nil || bind.cur != nil {
		t.Fatalf("%v %v", err, bind.cur)
	}
	restore()
	if _, on := g.On(a); on {
		t.Fatal("a restored file was read as a grant after off")
	}
	// On again, then off again: the first file is still not a grant, at any point.
	g.Give(a, time.Now())
	restore()
	if _, on := g.On(a); on {
		t.Fatal("an earlier grant's file was read as the new grant")
	}
	// The session ending without this build's help (the binding gone) ends it too.
	g.Give(a, time.Now())
	bind.cur = nil
	if _, on := g.On(a); on {
		t.Fatal("a grant outlived what it was bound to")
	}
	// Wipe takes the history with it.
	g.Give(a, time.Now())
	g.Record(ShareAudit{Account: a, Slug: "a.md", At: time.Now()})
	if err := g.Wipe(); err != nil {
		t.Fatal(err)
	}
	if _, on := g.On(a); on || len(g.History(a)) != 0 {
		t.Fatal("something survived Wipe")
	}
	if _, err := os.Stat(filepath.Join(dir, grantAuditFile)); !os.IsNotExist(err) {
		t.Fatalf("history file: %v", err)
	}
}

// The grant is off until given, belongs to one account, and ends when revoked.
func TestShareGrantGiveAndRevoke(t *testing.T) {
	g := newTestGrant(t.TempDir(), grantKey)
	a := Account{Env: "dev", OrgID: "o1", UserID: "u1"}
	now := time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)
	if _, on := g.On(a); on {
		t.Fatal("on by default")
	}
	if err := g.Give(Account{Env: "dev"}, now); err == nil {
		t.Fatal("given with no account")
	}
	if err := g.Give(a, now); err != nil {
		t.Fatal(err)
	}
	if at, on := g.On(a); !on || !at.Equal(now) {
		t.Fatal("not on after Give")
	}
	if err := g.Revoke(); err != nil {
		t.Fatal(err)
	}
	if _, on := g.On(a); on {
		t.Fatal("on after Revoke")
	}
	if err := g.Revoke(); err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
}

// Seeing another account ends the grant, so it can't come back when the first
// account returns.
func TestShareGrantEndsOnAccountChange(t *testing.T) {
	g := newTestGrant(t.TempDir(), grantKey)
	a, b := Account{Env: "dev", OrgID: "o1", UserID: "u1"}, Account{Env: "dev", OrgID: "o1", UserID: "u2"}
	g.Give(a, time.Now())
	if _, on := g.On(b); on {
		t.Fatal("carried to another account")
	}
	if _, on := g.On(a); on {
		t.Fatal("came back after another account was seen")
	}
	g.Give(a, time.Now())
	if _, on := g.On(Account{}); on {
		t.Fatal("on while signed out")
	}
	if _, on := g.On(a); on {
		t.Fatal("came back after sign-out")
	}
}

// A grant file written without the keychain key is no grant: by hand, with another
// key, or on a device with no keychain at all.
func TestShareGrantNeedsTheKey(t *testing.T) {
	dir := t.TempDir()
	a := Account{Env: "dev", OrgID: "o1", UserID: "u1"}
	bind := &testBinding{}
	g := NewShareGrant(dir, grantKey, bind)
	g.Give(a, time.Now())
	g.Revoke()
	bind.Renew() // a binding is in force, so only the seal stands between a file and a grant
	for name, write := range map[string]func(){
		"plain": func() {
			os.MkdirAll(dir, 0o700)
			os.WriteFile(filepath.Join(dir, grantFile), []byte(`{"account":{"env":"dev","org_id":"o1","user_id":"u1"},"granted_at":"2026-11-13T10:00:00Z"}`), 0o600)
		},
		"unsigned": func() {
			os.WriteFile(filepath.Join(dir, grantFile), []byte(`{"body":{"account":{"env":"dev","org_id":"o1","user_id":"u1"},"granted_at":"2026-11-13T10:00:00Z"},"mac":""}`), 0o600)
		},
		"another key": func() { NewShareGrant(dir, bytes.Repeat([]byte{4}, 32), &testBinding{}).Give(a, time.Now()) },
		"history's seal": func() {
			g.Record(ShareAudit{Account: a, At: time.Now()})
			b, _ := os.ReadFile(filepath.Join(dir, grantAuditFile))
			os.WriteFile(filepath.Join(dir, grantFile), b, 0o600)
		},
	} {
		write()
		if _, on := g.On(a); on {
			t.Errorf("%s: read as a grant", name)
		}
	}
	g.Give(a, time.Now())
	for _, none := range []ShareGrant{NewShareGrant(dir, nil, bind), NewShareGrant(dir, grantKey, nil)} {
		if _, on := none.On(a); on {
			t.Fatal("a grant was read with no key or no keychain binding")
		}
		if none.Available() || none.Give(a, time.Now()) != ErrGrantUnavailable {
			t.Fatal("a grant was given with no key or no keychain binding")
		}
	}
}

// The history counts an account's shares by clock hour and keeps no more than its limit.
func TestShareGrantHistory(t *testing.T) {
	g := newTestGrant(t.TempDir(), grantKey)
	a, b := Account{Env: "dev", OrgID: "o1", UserID: "u1"}, Account{Env: "dev", OrgID: "o1", UserID: "u2"}
	now := time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)
	for _, e := range []ShareAudit{
		{Account: a, Slug: "a.md", At: now.Add(-45 * time.Minute), RequestID: "r1", Status: "executing"},
		{Account: a, Slug: "b.md", At: now.Add(-10 * time.Minute), RequestID: "r2", Status: "executing"},
		{Account: b, Slug: "c.md", At: now, RequestID: "r3", Status: "executing"},
	} {
		if err := g.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Settle("r2", "done", "", false); err != nil {
		t.Fatal(err)
	}
	h := g.History(a)
	if len(h) != 2 || h[0].Slug != "a.md" || h[1].Status != "done" || h[0].Status != "executing" {
		t.Fatalf("%+v", h)
	}
	if n := g.UsedThisHour(a, now); n != 1 {
		t.Fatalf("%d used this hour", n)
	}
	// Every attempt in the hour counts, whatever became of it, but one refused
	// because the member isn't in the team.
	for i, e := range []ShareAudit{
		{Status: "failed", Reason: "node_changed"},
		{Status: "failed"},
		{Status: "canceled"},
		{Status: "reconciling"},
		{Status: "failed", Reason: ReasonNotTeamMember},
		// The reason alone, on a share that didn't fail, changes nothing.
		{Status: "done", Reason: ReasonNotTeamMember},
	} {
		id := "s" + string(rune('0'+i))
		if err := g.Record(ShareAudit{Account: a, Slug: "s.md", At: now, RequestID: id, Status: "executing"}); err != nil {
			t.Fatal(err)
		}
		if n := g.UsedThisHour(a, now); n != i+2 && i < 5 {
			t.Fatalf("a share in flight: %d used", n)
		}
		if err := g.Settle(id, e.Status, e.Reason, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := g.UsedThisHour(a, now); n != 6 {
		t.Fatalf("%d used this hour, want 6: every attempt but the not_team_member refusal", n)
	}
	h = g.History(a)
	if last := h[len(h)-2]; last.Reason != ReasonNotTeamMember || last.Status != "failed" || last.Counts() {
		t.Fatalf("%+v", last)
	}
	for i := 0; i < maxShareAudit+5; i++ {
		g.Record(ShareAudit{Account: a, Slug: "x.md", At: now})
	}
	if n := len(g.History(a)) + len(g.History(b)); n != maxShareAudit {
		t.Fatalf("%d kept", n)
	}
}

// The history's limit never pushes out a line that counts toward this hour's cap:
// lines that don't count go first, however many of them arrive.
func TestShareGrantHistoryKeepsThisHoursCountedLines(t *testing.T) {
	g := newTestGrant(t.TempDir(), grantKey)
	a, b := Account{Env: "dev", OrgID: "o1", UserID: "u1"}, Account{Env: "dev", OrgID: "o1", UserID: "u2"}
	now := time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)
	record := func(e ShareAudit) {
		t.Helper()
		if err := g.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	// An older hour's lines, then this hour's whole cap.
	for i := 0; i < 100; i++ {
		record(ShareAudit{Account: a, Slug: "old.md", At: now.Add(-3 * time.Hour), Status: "done"})
	}
	for i := 0; i < MaxGrantedSharesPerHour; i++ {
		record(ShareAudit{Account: a, Slug: "x.md", At: now, Status: "done"})
	}
	// Then far more lines than the history holds, none of which count for a now:
	// refused shares, and another account's.
	for i := 0; i < maxShareAudit+200; i++ {
		e := ShareAudit{Account: a, Slug: "r.md", At: now, Status: "failed", Reason: ReasonNotTeamMember}
		if i%2 == 1 {
			e = ShareAudit{Account: b, Slug: "theirs.md", At: now, Status: "done"}
		}
		record(e)
	}
	if n := g.UsedThisHour(a, now); n != MaxGrantedSharesPerHour {
		t.Fatalf("%d of this hour's %d counted lines are left", n, MaxGrantedSharesPerHour)
	}
	if n := len(g.History(a)) + len(g.History(b)); n != maxShareAudit {
		t.Fatalf("%d kept", n)
	}
	for _, e := range g.History(a) {
		if e.Slug == "old.md" {
			t.Fatal("an older hour's line outlived this hour's")
		}
	}
	// What is kept is the newest of what could go.
	h := g.History(b)
	if len(h) == 0 || !h[len(h)-1].At.Equal(now) {
		t.Fatalf("%+v", h)
	}
	if n := g.RefusedThisHour(a, now); n == 0 || n > maxShareAudit {
		t.Fatalf("%d refused", n)
	}
}
