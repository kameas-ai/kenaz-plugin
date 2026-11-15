package spool

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/clock"
)

// Every test of this package runs under the Windows rule: removing or
// renaming a file that this process still holds open fails, on every
// platform. A spool that forgets to close a segment before it removes it
// then fails here, not only on a Windows runner.
func TestMain(m *testing.M) {
	enforceOpenRule.Store(true)
	os.Exit(m.Run())
}

// openFiles is how many files of the spool this process holds open.
func openFiles() int {
	openMu.Lock()
	defer openMu.Unlock()
	n := 0
	for _, c := range openCount {
		n += c
	}
	return n
}

// The rule itself: an open file can be neither removed nor renamed, nor
// renamed over, until it is closed.
func TestOpenRuleEmulatesWindows(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.seg"), filepath.Join(dir, "b.seg")
	f, err := openTracked(a, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeFile(a); !errors.Is(err, ErrInUse) {
		t.Fatalf("removing an open file: %v", err)
	}
	if err := renameFile(a, filepath.Join(dir, "c.seg")); !errors.Is(err, ErrInUse) {
		t.Fatalf("renaming an open file: %v", err)
	}
	if err := renameFile(b, a); !errors.Is(err, ErrInUse) {
		t.Fatalf("renaming over an open file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close() // a second close does not uncount another handle
	if inUse(a) {
		t.Fatal("still counted after close")
	}
	if err := renameFile(b, a); err != nil {
		t.Fatal(err)
	}
	if err := removeFile(a); err != nil {
		t.Fatal(err)
	}
}

// Every way the spool removes or renames a file, with the writer open
// whenever the product would have it open: size eviction (including of the
// active segment), age eviction, removal after delivery, quarantine of a
// corrupt segment, AppendAll's rename, the cursor's rename, and purge. None
// may touch a file that is still open, and none may leak a handle.
func TestNoRemovalHitsAnOpenFile(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	s, _ := newSpool(t, Options{MaxBytes: 4096, SegmentBytes: 1024, Clock: clk})
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	payload := bytes.Repeat([]byte{'x'}, 200)
	// Size eviction while the writer is open, many times over.
	for i := 0; i < 80; i++ {
		clk.Advance(time.Millisecond)
		must("append", s.Append("l", payload))
	}
	if st := s.Stats(); st.DroppedSize == 0 {
		t.Fatalf("nothing was evicted for size: %+v", st)
	}
	// A set written whole and renamed into place, with the writer open before.
	must("append all", s.AppendAll("l", "", [][]byte{payload, payload}))
	must("append", s.Append("l", payload))
	// Delivery: each accepted record moves the cursor (a rename over the old
	// cursor) and each finished segment is removed; the callback appends, so a
	// writer is open while older segments go.
	n := 0
	_, err := s.Drain(func(Record) error {
		n++
		if n%3 == 0 {
			clk.Advance(time.Millisecond)
			return s.Append("l", payload)
		}
		return nil
	})
	must("drain", err)
	// A drain that stops part-way keeps its segment and its cursor.
	stop := errors.New("stop")
	if _, err := s.Drain(func(Record) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("drain: %v", err)
	}
	// Quarantine: a segment that does not decrypt is removed.
	must("append", s.Append("l", payload))
	must("close", s.Close())
	names, _ := s.segments()
	must("corrupt", os.WriteFile(filepath.Join(s.Dir(), names[len(names)-1]), []byte("not a segment"), 0o600))
	must("append", s.Append("l", payload)) // a writer is open again
	_, err = s.Drain(func(Record) error { return nil })
	must("drain after corruption", err)
	if st := s.Stats(); st.Quarantined != 1 {
		t.Fatalf("quarantined %d", st.Quarantined)
	}
	// Age eviction of everything, the active segment included.
	must("append", s.Append("l", payload))
	clk.Advance(8 * 24 * time.Hour)
	must("prune", s.Prune())
	if !s.Empty() {
		t.Fatal("expired segments remain")
	}
	// Purge with the writer open.
	must("append", s.Append("l", payload))
	must("purge", s.Purge())
	ents, _ := os.ReadDir(s.Dir())
	if len(ents) != 0 {
		t.Fatalf("%d file(s) left after purge", len(ents))
	}
	if n := openFiles(); n != 0 {
		t.Fatalf("%d file(s) still open after purge", n)
	}
	must("append", s.Append("l", payload))
	must("close", s.Close())
	if n := openFiles(); n != 0 {
		t.Fatalf("%d file(s) still open after close", n)
	}
}

// A segment is never open for reading while a purge removes it: the drain
// reads under the spool's lock, so a purge that arrives mid-read waits for
// the reader to close instead of failing (on Windows) and leaving the
// previous account's queue on disk.
func TestPurgeDuringADrainReadWaitsForTheReader(t *testing.T) {
	s, _ := newSpool(t, Options{})
	for i := 0; i < 3; i++ {
		if err := s.Append("l", bytes.Repeat([]byte{'x'}, 64)); err != nil {
			t.Fatal(err)
		}
	}
	purged := make(chan error, 1)
	started := false
	readingSegment = func(string) {
		if started {
			return
		}
		started = true
		go func() { purged <- s.Purge() }()
		// Give the purge every chance to run while the segment is open.
		select {
		case err := <-purged:
			purged <- err
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Cleanup(func() { readingSegment = nil })
	if _, err := s.Drain(func(Record) error { return nil }); err != nil {
		t.Fatalf("drain: %v", err)
	}
	select {
	case err := <-purged:
		if err != nil {
			t.Fatalf("a purge during the drain's read failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the purge never finished")
	}
	if ents, _ := os.ReadDir(s.Dir()); len(ents) != 0 || !s.Empty() {
		t.Fatalf("%d file(s) left after the purge", len(ents))
	}
}

// What a process must do, stated as a test: a spool object that still holds
// its writer blocks every other remover of that file, in this process or
// another. A relay that stops closes its spool (its process exits); logout
// runs PurgeDir after the relay has purged, which closes the writer.
func TestAnOpenWriterBlocksOtherRemovers(t *testing.T) {
	s, key := newSpool(t, Options{})
	if err := s.Append("l", []byte("queued")); err != nil {
		t.Fatal(err)
	}
	if err := PurgeDir(s.Dir()); !errors.Is(err, ErrInUse) {
		t.Fatalf("PurgeDir with the writer open: %v", err)
	}
	other, err := Open(s.Dir(), key, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Purge(); !errors.Is(err, ErrInUse) {
		t.Fatalf("another spool's purge with the writer open: %v", err)
	}
	if s.Empty() {
		t.Fatal("the segment went although its writer was open")
	}
	// Closed, as when the first relay has exited: both work.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := other.Purge(); err != nil || !other.Empty() {
		t.Fatalf("purge after the writer closed: %v", err)
	}
	if err := PurgeDir(s.Dir()); err != nil {
		t.Fatal(err)
	}
}

// Logout purges right after it asked the relay to stop. A relay that is a
// moment late closing its spool must not leave the signed-out account's
// queue on disk: PurgeDirWait retries until the writer is closed. If it
// never closes, the error names the file that is still there.
func TestPurgeDirWaitRetriesUntilTheWriterCloses(t *testing.T) {
	s, _ := newSpool(t, Options{})
	if err := s.Append("l", []byte("queued")); err != nil {
		t.Fatal(err)
	}
	if err := PurgeDir(s.Dir()); !errors.Is(err, ErrInUse) {
		t.Fatalf("one attempt with the writer open: %v", err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		s.Close()
	}()
	if err := PurgeDirWait(s.Dir(), 5*time.Second); err != nil {
		t.Fatalf("purge while the writer closes: %v", err)
	}
	if ents, _ := os.ReadDir(s.Dir()); len(ents) != 0 {
		t.Fatalf("%d file(s) left", len(ents))
	}

	// A writer that never closes: bounded, and reported.
	if err := s.Append("l", []byte("queued")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := PurgeDirWait(s.Dir(), 200*time.Millisecond)
	if !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), ".seg") {
		t.Fatalf("purge with a writer that stays open: %v", err)
	}
	if took := time.Since(start); took < 200*time.Millisecond || took > 3*time.Second {
		t.Fatalf("waited %s", took)
	}
	if s.Empty() {
		t.Fatal("the open segment was reported as not removed, but it is gone")
	}
}
