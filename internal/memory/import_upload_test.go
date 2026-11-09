package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// roomThisHour uses up the hour's cap except for leaving, so a test sees it bind without
// writing hundreds of notes, one commit each.
func roomThisHour(t *testing.T, st *Store, leaving int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	st.SetClock(func() time.Time { return now })
	used := MaxGrantedSavesPerHour - grantReservedForSaves - leaving
	err := st.tx(ctx, func(tx *sql.Tx) error {
		if err := setMeta(ctx, tx, metaSaveGrantHour, strconv.FormatInt(now.Unix()/3600, 10)); err != nil {
			return err
		}
		return setMeta(ctx, tx, metaSaveGrantCount, strconv.Itoa(used))
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The hourly cap holds for ordinary automatic notes and for imports nobody let through;
// imports a person let through go at the queue's pace, and leave the cap alone.
func TestImportUploadAndTheCap(t *testing.T) {
	ctx := context.Background()
	const room = 5
	save := func(t *testing.T, st *Store, key, source, name string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, _, err := st.SaveAuto(ctx, AutoNote{ProjectKey: key, Title: fmt.Sprintf("%s %d", name, i), Body: name, Source: source, Kind: KindObservation}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// drain queues and clears the queue until nothing more goes, and returns the count.
	drain := func(t *testing.T, st *Store, l Lease) int {
		t.Helper()
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
	setup := func(t *testing.T) (*Store, string, Lease) {
		st, key, l := strandedStore(t)
		if err := st.GrantSaves(ctx, ""); err != nil {
			t.Fatal(err)
		}
		roomThisHour(t, st, room)
		return st, key, l
	}

	t.Run("a whole hour's room is the cap less the reserve", func(t *testing.T) {
		st, _, _ := strandedStore(t)
		if err := st.GrantSaves(ctx, ""); err != nil {
			t.Fatal(err)
		}
		var got int
		err := st.tx(ctx, func(tx *sql.Tx) error {
			set, err := loadSettings(ctx, tx)
			if err != nil {
				return err
			}
			got, _, _, err = st.grantRoom(ctx, tx, set)
			return err
		})
		if err != nil || got != MaxGrantedSavesPerHour-grantReservedForSaves {
			t.Fatalf("%d %v", got, err)
		}
	})
	t.Run("ordinary notes keep the cap", func(t *testing.T) {
		st, key, l := setup(t)
		save(t, st, key, SourceCLI, "ordinary", room+3)
		if err := st.AllowImportUpload(ctx); err != nil {
			t.Fatal(err)
		}
		if n := drain(t, st, l); n != room {
			t.Fatalf("%d went, room %d", n, room)
		}
	})
	t.Run("imports nobody let through keep the cap", func(t *testing.T) {
		st, key, l := setup(t)
		save(t, st, key, SourceImport, "imported", room+3)
		if n := drain(t, st, l); n != room {
			t.Fatalf("%d went, room %d", n, room)
		}
	})
	t.Run("imports let through pass it and leave it alone", func(t *testing.T) {
		st, key, l := setup(t)
		// More than one pass of the queue, with the hour's room well below that.
		over := autoQueueDepth + 5
		save(t, st, key, SourceImport, "imported", over)
		if err := st.AllowImportUpload(ctx); err != nil {
			t.Fatal(err)
		}
		if n := drain(t, st, l); n != over {
			t.Fatalf("%d went", n)
		}
		// Ordinary notes still have what the hour had left.
		save(t, st, key, SourceCLI, "ordinary", room+3)
		if n := drain(t, st, l); n != room {
			t.Fatalf("%d ordinary went, room %d", n, room)
		}
	})
	t.Run("notes imported later aren't covered", func(t *testing.T) {
		st, key, l := setup(t)
		save(t, st, key, SourceImport, "first", 3)
		if err := st.AllowImportUpload(ctx); err != nil {
			t.Fatal(err)
		}
		save(t, st, key, SourceImport, "later", room+3)
		if n := drain(t, st, l); n != 3+room {
			t.Fatalf("%d went, want %d", n, 3+room)
		}
	})
}
