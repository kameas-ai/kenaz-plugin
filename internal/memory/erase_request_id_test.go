package memory

import (
	"context"
	"testing"
	"time"
)

func eraseMeta(t *testing.T, st *Store) string {
	t.Helper()
	var v string
	st.db.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = ?`, metaEraseRequestID).Scan(&v)
	return v
}

// One id per confirmed forget-all, kept until Fleet answers.
func TestEraseRequestIDLifecycle(t *testing.T) {
	ctx := context.Background()
	st, _, _ := strandedStore(t)
	if _, err := st.ForgetAll(ctx, MutateOptions{SyncApproved: true, RequestKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	first := eraseMeta(t, st)
	if len(first) != 32 {
		t.Fatalf("%q", first)
	}
	// The barrier ends leases, so this takes a new one, as a worker does.
	l, err := st.AcquireLease(ctx, "h2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.EraseRequestID(ctx, l); err != nil || got != first {
		t.Fatalf("%q %v, want %q", got, err, first)
	}
	// The same approval replayed is the same forget-all.
	if _, err := st.ForgetAll(ctx, MutateOptions{SyncApproved: true, RequestKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	if got := eraseMeta(t, st); got != first {
		t.Fatalf("replay changed the id: %q", got)
	}
	// A new confirmation is a new forget-all, even with the last one unanswered.
	if _, err := st.ForgetAll(ctx, MutateOptions{SyncApproved: true, RequestKey: "k2"}); err != nil {
		t.Fatal(err)
	}
	second := eraseMeta(t, st)
	if second == first || second == "" {
		t.Fatalf("%q then %q", first, second)
	}
	if l, err = st.AcquireLease(ctx, "h3", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteErase(ctx, l, "before"); err != nil {
		t.Fatal(err)
	}
	if got := eraseMeta(t, st); got != "" {
		t.Fatalf("still here after Fleet answered: %q", got)
	}
}

// A store whose barrier went up before ids existed gets one, and keeps it.
func TestEraseRequestIDForAnOlderBarrier(t *testing.T) {
	ctx := context.Background()
	st, _, l := strandedStore(t)
	if _, err := st.db.ExecContext(ctx, `UPDATE sync_state SET erase_pending = 1, erase_barrier_hlc = 'old'`); err != nil {
		t.Fatal(err)
	}
	if eraseMeta(t, st) != "" {
		t.Fatal("this store should have no id")
	}
	id, err := st.EraseRequestID(ctx, l)
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	if again, err := st.EraseRequestID(ctx, l); err != nil || again != id {
		t.Fatalf("%q %v, want %q", again, err, id)
	}
	if eraseMeta(t, st) != id {
		t.Fatal("not stored")
	}
}

// A replay of one approval keeps the id; a new approval gets its own, and a new state token.
func TestEraseRequestIDFollowsTheApproval(t *testing.T) {
	ctx := context.Background()
	st, _, _ := strandedStore(t)
	tok1, _ := st.StateToken(ctx)
	if _, err := st.ForgetAll(ctx, MutateOptions{RequestKey: "k1", SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	a := eraseMeta(t, st)
	if _, err := st.ForgetAll(ctx, MutateOptions{RequestKey: "k1", SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	if eraseMeta(t, st) != a || a == "" {
		t.Fatalf("a replay changed the id: %q %q", a, eraseMeta(t, st))
	}
	tok2, _ := st.StateToken(ctx)
	if tok1 == tok2 {
		t.Error("state token unchanged by forget-all, so a second request would reuse the key and erase nothing")
	}
	if _, err := st.ForgetAll(ctx, MutateOptions{RequestKey: "k2", SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	if eraseMeta(t, st) == a {
		t.Fatal("a new forget-all kept the id")
	}
	if tok3, _ := st.StateToken(ctx); tok3 == tok2 {
		t.Error("state token unchanged by the second forget-all on an empty store")
	}
}

// An id belongs to the barrier it was minted for.
func TestEraseRequestIDBelongsToItsBarrier(t *testing.T) {
	ctx := context.Background()
	st, _, _ := strandedStore(t)
	if _, err := st.ForgetAll(ctx, MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	l, err := st.AcquireLease(ctx, "h2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.EraseRequestID(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	// An older binary's forget-all moves the barrier and leaves the id where it is.
	if _, err := st.db.ExecContext(ctx, `UPDATE sync_state SET erase_barrier_hlc = 'a later barrier'`); err != nil {
		t.Fatal(err)
	}
	second, err := st.EraseRequestID(ctx, l)
	if err != nil || second == first || second == "" {
		t.Fatalf("%q then %q, %v", first, second, err)
	}
	// Its retries keep the new one.
	if again, err := st.EraseRequestID(ctx, l); err != nil || again != second {
		t.Fatalf("%q then %q, %v", second, again, err)
	}
}
