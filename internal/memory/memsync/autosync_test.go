package memsync

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// autoDevice is a signed-in device with sync on and, if granted, the standing grant.
func autoDevice(t *testing.T, granted bool) (*device, *fleetapitest.MemoryFleet) {
	t.Helper()
	f := fleetapitest.NewMemoryFleet()
	d := newDevice(t, f, alice)
	d.enable()
	if granted {
		if err := d.st.GrantSaves(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
	}
	return d, f
}

func (d *device) saveAuto(n int) {
	d.t.Helper()
	for i := 0; i < n; i++ {
		_, _, err := d.st.SaveAuto(context.Background(), memory.AutoNote{ProjectKey: memory.GlobalProject,
			Title: fmt.Sprint("observed ", i), Body: fmt.Sprint("what happened ", i, " at ", time.Now().UnixNano()), Source: memory.SourceCLI,
			Kind: memory.KindObservation, Tags: []string{memory.AutoTag}})
		if err != nil {
			d.t.Fatal(err)
		}
	}
}

func (d *device) at(when time.Time) { d.st.SetClock(func() time.Time { return when }) }

// A Fleet with its flag off refuses these kinds for good, as far as it says. The notes
// stay here, and it hears from this device once a batch at most, never once a note.
func TestRefusedKindsStayLocalWithoutAStorm(t *testing.T) {
	d, f := autoDevice(t, true)
	f.RefuseCaptureKinds = true
	d.saveAuto(5)
	base := time.Now()
	d.at(base)

	for i := 0; i < 20; i++ {
		d.sync()
	}
	st := d.status()
	if got := f.Calls("push"); got != 1 {
		t.Errorf("%d pushes in 20 cycles, want 1", got)
	}
	if st.AutoHeld != "invalid_field" {
		t.Errorf("held: %+v", st)
	}
	if st.Notes[memory.StateRejected] != 0 || len(st.BlockedCodes) != 0 || st.Queue["blocked"] != 0 {
		t.Errorf("a note was refused for good: %+v", st)
	}
	if st.Queue["ready"] != 1 {
		t.Errorf("queue %+v: only the note that was tried waits in the queue", st.Queue)
	}
	if n := st.Notes[memory.StateLocal] + st.Notes[memory.StatePending]; n != 5 {
		t.Errorf("notes lost: %+v", st.Notes)
	}

	// An hour on, one more try; then the wait doubles.
	d.at(base.Add(time.Hour + time.Minute))
	d.sync()
	d.sync()
	if got := f.Calls("push"); got != 2 {
		t.Errorf("%d pushes after the first wait, want 2", got)
	}
	d.at(base.Add(2*time.Hour + 2*time.Minute))
	d.sync()
	if got := f.Calls("push"); got != 2 {
		t.Errorf("%d pushes: the second wait did not double", got)
	}

	// The flag comes on: everything goes, and the first success opens the way.
	f.RefuseCaptureKinds = false
	d.at(base.Add(24 * time.Hour))
	for i := 0; i < 8; i++ {
		d.sync()
	}
	st = d.status()
	if st.Notes[memory.StateSynced] != 5 || st.AutoHeld != "" || st.AutoWaiting != 0 {
		t.Errorf("after the flag: %+v", st)
	}
}

// Until Fleet has taken one automatic note, a batch carries one.
func TestFirstAutomaticNoteGoesAlone(t *testing.T) {
	d, f := autoDevice(t, true)
	d.saveAuto(3)
	saved := d.save("on purpose", "deliberate", true)
	_ = saved
	sizes := []int{}
	f.Fault = func(call string, after bool) error {
		if call == "push" && !after {
			sizes = append(sizes, 0)
		}
		return nil
	}
	d.sync()
	st := d.status()
	if st.Notes[memory.StateSynced] != 4 {
		t.Fatalf("%+v", st)
	}
	if len(sizes) < 2 {
		t.Errorf("%d pushes: the automatic notes all went at once", len(sizes))
	}
}

// Without the grant nothing is queued, nothing is sent, and status says why.
func TestAutomaticNotesWaitForTheGrant(t *testing.T) {
	d, f := autoDevice(t, false)
	d.saveAuto(3)
	d.sync()
	st := d.status()
	if got := f.Calls("push"); got != 0 {
		t.Errorf("%d pushes", got)
	}
	if st.AutoWaiting != 3 || st.Notes[memory.StateLocal] != 3 {
		t.Errorf("%+v", st)
	}
	// Notes saved before the grant go once it is given: the backfill.
	if err := d.st.GrantSaves(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	d.sync()
	if st := d.status(); st.Notes[memory.StateSynced] != 3 || st.AutoWaiting != 0 {
		t.Errorf("after the grant: %+v", st)
	}
}

// Notes saved before sync was turned on are on the list the member approves, after the
// notes saved on purpose, and turning sync on queues exactly that list.
func TestEnablingSyncListsAutomaticNotes(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	d := newDevice(t, f, alice)
	d.saveAuto(2)
	d.save("on purpose", "deliberate", true)
	ctx := context.Background()
	cand, err := d.st.LocalSyncCandidates(ctx)
	if err != nil || len(cand) != 3 {
		t.Fatalf("%+v %v", cand, err)
	}
	if memory.IsAuto(cand[0].Kind) || !memory.IsAuto(cand[1].Kind) || !memory.IsAuto(cand[2].Kind) {
		t.Errorf("order: %+v", cand)
	}
	r := Remote{Memory: f.SessionClient(d.sessionFn), Session: d.sessionFn}
	if err := r.EnableSync(ctx, d.p, consent); err != nil {
		t.Fatal(err)
	}
	res, err := d.st.EnableSync(ctx, consent, memory.MutateOptions{SyncApproved: true, Records: cand[:2]})
	if err != nil || res.Queued != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	d.sync()
	st := d.status()
	if st.Notes[memory.StateSynced] != 2 || st.AutoWaiting != 1 {
		t.Errorf("%+v", st)
	}
}

// The grant's hourly cap holds, and automatic notes leave room for deliberate ones.
func TestBacklogStaysWithinTheHourlyCap(t *testing.T) {
	d, f := autoDevice(t, true)
	d.saveAuto(450)
	base := time.Now().Truncate(time.Hour).Add(time.Minute)
	d.at(base)
	d.sync()
	st := d.status()
	want := memory.MaxGrantedSavesPerHour - 100
	if st.Notes[memory.StateSynced] != want {
		t.Errorf("synced %d in the hour, want %d", st.Notes[memory.StateSynced], want)
	}
	granted, within, err := d.st.SpendSaveGrant(context.Background())
	if err != nil || !granted || !within {
		t.Errorf("a deliberate save lost its room: %v %v %v", granted, within, err)
	}
	d.at(base.Add(time.Hour))
	d.sync()
	if st := d.status(); st.Notes[memory.StateSynced] != 2*want {
		t.Errorf("next hour: %+v", st.Notes)
	}
	_ = f
}

// An organization's switch stops the sending before any note leaves, and lifting it
// starts it again at once.
func TestCaptureSwitchStopsSendingWithoutARefusal(t *testing.T) {
	d, f := autoDevice(t, true)
	off := true
	d.w = New(Options{Store: d.st, Memory: f.SessionClient(d.sessionFn), Session: d.sessionFn, ConsentVersion: consent, Holder: d.holder,
		Log: slog.New(slog.DiscardHandler),
		Capabilities: func(context.Context) (fleetapi.Capabilities, error) {
			return fleetapi.Capabilities{Keys: map[string]bool{fleetapi.CapMemorySync: true, fleetapi.CapMemoryCaptureOff: off}}, nil
		}})
	d.saveAuto(2)
	d.sync()
	if got := f.Calls("push"); got != 0 {
		t.Errorf("%d pushes with capture off", got)
	}
	if st := d.status(); st.AutoHeld != "capture_off" {
		t.Errorf("%+v", st)
	}
	// Deliberate notes are not held up.
	d.save("on purpose", "deliberate", true)
	d.sync()
	if st := d.status(); st.Notes[memory.StateSynced] != 1 {
		t.Errorf("%+v", st.Notes)
	}
	off = false
	d.sync()
	if st := d.status(); st.Notes[memory.StateSynced] != 3 || st.AutoHeld != "" {
		t.Errorf("after lifting: %+v", st)
	}
}

// A push refused with capture_off, before this device heard the switch.
func TestCaptureOffRefusalBacksOff(t *testing.T) {
	d, f := autoDevice(t, true)
	f.CaptureOff = true
	d.saveAuto(3)
	for i := 0; i < 5; i++ {
		d.sync()
	}
	st := d.status()
	if got := f.Calls("push"); got != 1 {
		t.Errorf("%d pushes", got)
	}
	if st.AutoHeld != "capture_off" || st.Notes[memory.StateRejected] != 0 {
		t.Errorf("%+v", st)
	}
}

// Only a refusal of the kind waits: anything else about a note still refuses it, and a
// deliberate note's kind is not ours to wait on.
func TestOtherInvalidFieldsStillRefuse(t *testing.T) {
	d, f := autoDevice(t, true)
	_ = f
	ctx := context.Background()
	id := d.save("on purpose", "deliberate text", true)
	l, err := d.st.AcquireLease(ctx, "t", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.st.LeasePushBatch(ctx, l)
	if err != nil || len(b.Ops) != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	no := false
	if _, err := d.st.ApplyPushResults(ctx, l, b.Ops, []memory.PushResult{{ID: id, Status: "rejected", Code: "invalid_field", Field: "kind", Retry: &no}}); err != nil {
		t.Fatal(err)
	}
	if st := d.status(); st.Notes[memory.StateRejected] != 1 || st.AutoHeld != "" {
		t.Errorf("%+v", st)
	}
}

// Another device gets the note, and keeps its kind, so that it is labelled untrusted.
func TestPulledAutomaticNoteKeepsItsKind(t *testing.T) {
	d, f := autoDevice(t, true)
	d.saveAuto(1)
	d.sync()
	other := newDevice(t, f, alice)
	other.enable()
	other.sync()
	got, err := other.st.Recent(context.Background(), memory.AllProjects, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != memory.KindObservation {
		t.Errorf("%+v", got)
	}
}
