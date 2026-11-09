package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func markSave(t *testing.T, st *Store, key, source, name string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, _, err := st.SaveAuto(context.Background(), AutoNote{ProjectKey: key, Title: fmt.Sprintf("%s %d", name, i), Body: name + fmt.Sprint(i), Source: source, Kind: KindObservation}); err != nil {
			t.Fatal(err)
		}
	}
}

func markDrain(t *testing.T, st *Store, l Lease) int {
	t.Helper()
	ctx := context.Background()
	total := 0
	for {
		n, err := st.QueueAutoBacklog(ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return total
		}
		total += n
		if _, err := st.db.ExecContext(ctx, `DELETE FROM outbox`); err != nil {
			t.Fatal(err)
		}
	}
}

// The mark is a row number. Forget-all deletes the rows, so numbers start over: the mark goes
// too, or notes imported afterward by a command with no terminal would go free.
func TestImportMarkEndsWithForgetAll(t *testing.T) {
	ctx := context.Background()
	const room = 5
	st, key, l := strandedStore(t)
	if err := st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	markSave(t, st, key, SourceImport, "first", room+5)
	if err := st.AllowImportUpload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ForgetAll(ctx, MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.QueueAutoBacklog(ctx, l); err == nil && n != 0 {
		t.Errorf("queued %d right after forget-all", n)
	}
	var v string
	st.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'import_upload_through'`).Scan(&v)
	if v != "" {
		t.Errorf("mark after forget-all: %q", v)
	}
	// Past the erase, sync and the standing grant on again; nobody ran `memory on`.
	if _, err := st.db.ExecContext(ctx, `UPDATE sync_state SET erase_pending = 0, erase_barrier_hlc = ''`); err != nil {
		t.Fatal(err)
	}
	l, err := st.AcquireLease(ctx, "h2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	roomThisHour(t, st, room)
	markSave(t, st, key, SourceImport, "agent", room+4)
	if n := markDrain(t, st, l); n > room {
		t.Errorf("%d went in an hour, past the room of %d, with nobody letting them through", n, room)
	}
}

// Sync off, grant ended, signed out: nothing queues, whatever the mark says.
func TestImportMarkDoesntOutlastStop(t *testing.T) {
	ctx := context.Background()
	st, key, l := strandedStore(t)
	if err := st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	markSave(t, st, key, SourceImport, "i", 20)
	if err := st.AllowImportUpload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StopSync(ctx, "disabled_by_user"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.QueueAutoBacklog(ctx, l); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}

// Ordinary notes go first and don't wait behind a let-through import, and aren't marked.
func TestOrdinaryNotesGoBeforeImports(t *testing.T) {
	ctx := context.Background()
	st, key, l := strandedStore(t)
	if err := st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	markSave(t, st, key, SourceImport, "i", 20)
	if err := st.AllowImportUpload(ctx); err != nil {
		t.Fatal(err)
	}
	markSave(t, st, key, SourceCLI, "o", 10)
	// Until Fleet has taken one, one goes at a time, and it's an ordinary note each time.
	for i := 0; i < 10; i++ {
		if _, err := st.QueueAutoBacklog(ctx, l); err != nil {
			t.Fatal(err)
		}
		var ord, imp int
		st.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox o JOIN records r ON r.id = o.record_id WHERE r.source != 'import'`).Scan(&ord)
		st.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox o JOIN records r ON r.id = o.record_id WHERE r.source = 'import'`).Scan(&imp)
		if ord == 0 || imp != 0 {
			t.Fatalf("pass %d: %d ordinary, %d imported", i, ord, imp)
		}
		if _, err := st.db.ExecContext(ctx, `DELETE FROM outbox`); err != nil {
			t.Fatal(err)
		}
	}
}

// Ending the grant, and a reset, end the mark.
func TestImportMarkEnds(t *testing.T) {
	ctx := context.Background()
	mark := func(st *Store) string {
		var v string
		st.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'import_upload_through'`).Scan(&v)
		return v
	}
	setup := func(t *testing.T) (*Store, Lease) {
		st, key, l := strandedStore(t)
		if err := st.GrantSaves(ctx, ""); err != nil {
			t.Fatal(err)
		}
		markSave(t, st, key, SourceImport, "i", 3)
		if err := st.AllowImportUpload(ctx); err != nil {
			t.Fatal(err)
		}
		if mark(st) == "" {
			t.Fatal("no mark")
		}
		return st, l
	}
	t.Run("grant", func(t *testing.T) {
		st, _ := setup(t)
		if err := st.RevokeSaveGrant(ctx); err != nil {
			t.Fatal(err)
		}
		if v := mark(st); v != "" {
			t.Fatal(v)
		}
	})
	t.Run("reset", func(t *testing.T) {
		st, l := setup(t)
		gen, err := st.BeginReset(ctx, l, "cursor_expired", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CommitReset(ctx, l, gen, ""); err != nil {
			t.Fatal(err)
		}
		if v := mark(st); v != "" {
			t.Fatal(v)
		}
	})
}
