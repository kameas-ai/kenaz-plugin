package memsync

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

func forgetAllOn(t *testing.T, d *device) {
	t.Helper()
	if _, err := d.st.ForgetAll(context.Background(), memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
}

// The answer was lost: the retry, after a restart, is the same request, and nothing newer goes.
func TestLostForgetAllAnswerErasesOnce(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	d := newDevice(t, f, alice)
	d.enable()
	d.save("t", "old note", true)
	d.sync()
	forgetAllOn(t, d)
	f.Fault = func(call string, after bool) error {
		if call == "forget_all" && after {
			return errors.New("connection lost")
		}
		return nil
	}
	if err := d.w.RunOnce(ctx); err == nil {
		t.Fatal("the lost answer went unnoticed")
	}
	if pending, _ := d.st.ErasePending(ctx); !pending {
		t.Fatal("the barrier came down with no answer")
	}
	// Another device writes in the gap.
	other := newDevice(t, f, alice)
	other.enable()
	other.save("t", "newer note", true)
	other.sync()
	f.Fault = nil
	d.w.release()
	d.st.Close()
	d.open()
	d.sync()
	if f.Erases != 1 || len(f.ForgetAllIDs) != 1 || f.ForgetAllIDs[0] == "" {
		t.Fatalf("%d erases, ids %v", f.Erases, f.ForgetAllIDs)
	}
	if pending, _ := d.st.ErasePending(ctx); pending {
		t.Fatal("the barrier stayed up")
	}
	if got := pulledBodies(t, f, alice); len(got) != 1 {
		t.Fatalf("Fleet holds %d notes, want the newer one", len(got))
	}
}

// Still running, on any status Fleet settles on: not a failure, and it waits as told.
func TestEraseInProgressIsNotAFailure(t *testing.T) {
	for _, status := range []int{409, 503} {
		ctx := context.Background()
		f := fleetapitest.NewMemoryFleet()
		d := newDevice(t, f, alice)
		d.enable()
		forgetAllOn(t, d)
		f.Fault = func(call string, after bool) error {
			if call == "forget_all" && !after {
				return fleetapitest.EraseInProgress(status, 7)
			}
			return nil
		}
		d.w.backoff = time.Minute
		err := d.w.RunOnce(ctx)
		if !errors.Is(err, errEraseRunning) {
			t.Fatalf("%d: %v", status, err)
		}
		if wait := d.w.eraseWait(err); wait != 7*time.Second || d.w.backoff != 0 {
			t.Fatalf("%d: wait %v, backoff %v", status, wait, d.w.backoff)
		}
		st, err := d.st.Status(ctx)
		if err != nil || !st.ErasePending || st.StatusCode != CodeErasePending {
			t.Fatalf("%d: %+v %v", status, st, err)
		}
		f.Fault = nil
		if err := d.w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if pending, _ := d.st.ErasePending(ctx); pending {
			t.Fatalf("%d: the barrier stayed up", status)
		}
	}
}

// With no time from Fleet there's still a wait.
func TestEraseWaitWithoutRetryAfter(t *testing.T) {
	d := newDevice(t, fleetapitest.NewMemoryFleet(), alice)
	err := &fleetapi.APIError{Status: 409, Code: fleetapi.CodeEraseInProgress}
	if got := d.w.eraseWait(err); got != eraseRetryWait {
		t.Fatal(got)
	}
}

// forgetAllAs answers as a Fleet other than the fake's own.
type forgetAllAs struct {
	fleetapi.Memory
	seen []string
	fn   func(m fleetapi.Memory, ctx context.Context, r fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error)
}

func (s *forgetAllAs) ForgetAll(ctx context.Context, r fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error) {
	s.seen = append(s.seen, r.RequestID)
	return s.fn(s.Memory, ctx, r)
}

// Today's Fleet ignores the id. A stricter one that refuses it still gets the erase.
func TestForgetAllAgainstOtherFleets(t *testing.T) {
	ctx := context.Background()
	t.Run("ignores the id", func(t *testing.T) {
		f := fleetapitest.NewMemoryFleet()
		d := newDevice(t, f, alice)
		d.enable()
		forgetAllOn(t, d)
		as := &forgetAllAs{Memory: d.w.o.Memory, fn: func(m fleetapi.Memory, ctx context.Context, r fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error) {
			r.RequestID = ""
			return m.ForgetAll(ctx, r)
		}}
		d.w.o.Memory = as
		if err := d.w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if pending, _ := d.st.ErasePending(ctx); pending || f.Erases != 1 {
			t.Fatalf("pending %v, %d erases", pending, f.Erases)
		}
	})
	t.Run("refuses the id", func(t *testing.T) {
		f := fleetapitest.NewMemoryFleet()
		d := newDevice(t, f, alice)
		d.enable()
		forgetAllOn(t, d)
		as := &forgetAllAs{Memory: d.w.o.Memory, fn: func(m fleetapi.Memory, ctx context.Context, r fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error) {
			if r.RequestID != "" {
				return fleetapi.MemoryForgetAllResponse{}, &fleetapi.APIError{Status: 400, Code: fleetapi.CodeInvalidRequestBody}
			}
			return m.ForgetAll(ctx, r)
		}}
		d.w.o.Memory = as
		if err := d.w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if pending, _ := d.st.ErasePending(ctx); pending || f.Erases != 1 || len(as.seen) != 2 || as.seen[0] == "" || as.seen[1] != "" {
			t.Fatalf("pending %v, %d erases, sent %v", pending, f.Erases, as.seen)
		}
	})
}

func lostAnswer(call string, after bool) error {
	if call == "forget_all" && after {
		return errors.New("connection lost")
	}
	return nil
}

func (d *device) execSQL(t *testing.T, q string) {
	t.Helper()
	db, err := sql.Open("sqlite", d.path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

// A second forget-all while the first's answer is lost: its own id, and it erases.
func TestSecondForgetAllErases(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	d := newDevice(t, f, alice)
	d.enable()
	d.save("t", "old", true)
	d.sync()
	forgetAllOn(t, d)
	f.Fault = lostAnswer
	d.w.RunOnce(ctx)
	other := newDevice(t, f, alice)
	other.enable()
	other.save("t", "newer", true)
	other.sync()
	f.Fault = nil
	forgetAllOn(t, d)
	d.w.release()
	d.open()
	d.sync()
	if f.Erases != 2 || f.ForgetAllIDs[0] == f.ForgetAllIDs[1] {
		t.Fatalf("%d erases, ids %v", f.Erases, f.ForgetAllIDs)
	}
	if got := pulledBodies(t, f, alice); len(got) != 0 {
		t.Fatalf("Fleet still holds %d after the second forget-all", len(got))
	}
}

// After a completed erase, a new one has a new id, and erases.
func TestLaterForgetAllErases(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	d := newDevice(t, f, alice)
	d.enable()
	d.save("t", "a", true)
	d.sync()
	forgetAllOn(t, d)
	d.sync()
	d.sync()
	other := newDevice(t, f, alice)
	other.enable()
	other.save("t", "b", true)
	other.sync()
	forgetAllOn(t, d)
	d.sync()
	if f.Erases != 2 || f.ForgetAllIDs[0] == f.ForgetAllIDs[1] {
		t.Fatalf("%d erases, ids %v", f.Erases, f.ForgetAllIDs)
	}
	if got := pulledBodies(t, f, alice); len(got) != 0 {
		t.Fatalf("Fleet still holds %d", len(got))
	}
}

// An older binary between two runs of this one. The first answer was lost; the older binary
// finishes that erase without clearing the id, then takes a second forget-all, which moves
// the barrier and leaves the id. This one must not deliver the second with the first's id,
// or Fleet answers with the old result and erases nothing.
func TestOlderBinaryDoesntLeaveAnIDToReuse(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	d := newDevice(t, f, alice)
	d.enable()
	d.save("t", "first", true)
	d.sync()
	forgetAllOn(t, d)
	f.Fault = lostAnswer
	d.w.RunOnce(ctx)
	f.Fault = nil
	other := newDevice(t, f, alice)
	other.enable()
	other.save("t", "note the second forget-all must erase", true)
	other.sync()
	d.execSQL(t, `UPDATE sync_state SET erase_pending = 0`)
	d.execSQL(t, `UPDATE sync_state SET erase_pending = 1, erase_barrier_hlc = '9999999999999999:000000:older-binary'`)
	d.w.release()
	d.open()
	d.sync()
	if pending, _ := d.st.ErasePending(ctx); pending {
		t.Fatal("the barrier stayed up")
	}
	if got := pulledBodies(t, f, alice); len(got) != 0 || f.Erases != 2 {
		t.Fatalf("reported erased with %d erases, and Fleet still holds %d notes", f.Erases, len(got))
	}
}
