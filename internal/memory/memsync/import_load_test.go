package memsync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

func (d *device) importNotes(n int) {
	d.t.Helper()
	for i := 0; i < n; i++ {
		if _, _, err := d.st.SaveAuto(context.Background(), memory.AutoNote{ProjectKey: memory.GlobalProject, Title: fmt.Sprint("imported ", i), Body: fmt.Sprint("old work ", i), Source: memory.SourceImport, Kind: memory.KindObservation, Tags: []string{memory.AutoTag}}); err != nil {
			d.t.Fatal(err)
		}
	}
}

func TestImportOverQuotaWaits(t *testing.T) {
	d, f := autoDevice(t, true)
	f.MaxRecords = 50
	d.importNotes(150)
	ctx := context.Background()
	if err := d.st.AllowImportUpload(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := d.w.RunOnce(ctx); err != nil {
			t.Logf("run %d: %v", i, err)
		}
	}
	first := f.Calls("push")
	for i := 0; i < 50; i++ {
		if err := d.w.RunOnce(ctx); err != nil {
			t.Logf("run: %v", err)
		}
	}
	st := d.status()
	t.Logf("pushes first %d, then 50 more runs added %d; notes %v quota=%v", first, f.Calls("push")-first, st.Notes, st.QuotaReached)
	if f.Calls("push")-first > 0 {
		t.Errorf("hot loop while over quota")
	}
	if st.Notes[memory.StateRejected] != 0 || !st.QuotaReached {
		t.Errorf("%+v", st)
	}
	// Past the limit for days, more than the attempts that end a note. Then the limit rises.
	later := time.Now()
	for i := 0; i < memory.MaxOpAttempts+3; i++ {
		later = later.Add(2 * time.Hour)
		d.at(later)
		d.w.RunOnce(ctx)
	}
	if st = d.status(); st.Notes[memory.StateRejected] != 0 {
		t.Fatalf("rejected after waiting: %v", st.Notes)
	}
	f.MaxRecords = 20000
	for i := 0; i < 20; i++ {
		later = later.Add(2 * time.Hour)
		d.at(later)
		d.w.RunOnce(ctx)
	}
	st = d.status()
	t.Logf("after the limit rose: %v quota=%v", st.Notes, st.QuotaReached)
	if st.Notes[memory.StateSynced] != 150 {
		t.Errorf("%v", st.Notes)
	}
}

func TestImportRateLimitedWaits(t *testing.T) {
	d, f := autoDevice(t, true)
	d.importNotes(120)
	ctx := context.Background()
	if err := d.st.AllowImportUpload(ctx); err != nil {
		t.Fatal(err)
	}
	f.Fault = func(call string, after bool) error {
		if call == "push" && !after {
			return fleetapitest.RateLimited(60)
		}
		return nil
	}
	later := time.Now()
	for i := 0; i < memory.MaxOpAttempts*4; i++ {
		d.w.RunOnce(ctx)
		later = later.Add(time.Hour)
		d.at(later)
	}
	st := d.status()
	t.Logf("after %d rounds of 429: %v", memory.MaxOpAttempts*4, st.Notes)
	if st.Notes[memory.StateRejected] != 0 {
		t.Errorf("429 turned into a rejection: %v", st.Notes)
	}
	f.Fault = nil
	for i := 0; i < 20; i++ {
		later = later.Add(time.Hour)
		d.at(later)
		d.w.RunOnce(ctx)
	}
	st = d.status()
	t.Logf("after: %v", st.Notes)
	if st.Notes[memory.StateSynced] != 120 {
		t.Errorf("%v", st.Notes)
	}
}
