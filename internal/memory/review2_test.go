package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Second security review regression (reviewer's repro): a pull page that
// was in flight when the user ran forget-all cannot bring notes back,
// with the old lease or a fresh one, until Fleet confirmed the erase.
func TestPullRefusedWhileErasePending(t *testing.T) {
	ctx := context.Background()
	s, err := OpenProfile(filepath.Join(t.TempDir(), "a.db"), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.EnableSync(ctx, "v1", MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	l, err := s.AcquireLease(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := s.ForgetAll(ctx, MutateOptions{}); err != nil || !res.FleetErasePending {
		t.Fatalf("forget-all = %+v %v", res, err)
	}
	c := "remembered secret plan"
	page := PullPage{Records: []PulledRecord{{ID: "mem-1", Seq: "5", State: "live", Content: c, ContentHash: ContentHash(c), Kind: "raw", ScopeKind: "global", Title: "t"}}, Cursor: "5"}
	if _, err := s.ApplyPullPage(ctx, l, page); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old lease applied a page: %v", err)
	}
	l2, err := s.AcquireLease(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPullPage(ctx, l2, page); !errors.Is(err, ErrErasePending) {
		t.Fatalf("page applied during erase: %v", err)
	}
	if _, err := s.BeginReset(ctx, l2, "cursor_expired", ""); !errors.Is(err, ErrErasePending) {
		t.Fatalf("reset staged during erase: %v", err)
	}
	if n, _ := s.Count(ctx); n != 0 {
		t.Fatal("forget-all resurrected by a pull")
	}
}
