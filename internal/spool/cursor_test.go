package spool

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/clock"
)

// fill appends n records "rec-00".."rec-(n-1)".
func fill(t *testing.T, s *Spool, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.Append("l", []byte(fmt.Sprintf("rec-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
}

// sidecars lists the cursor files (and cursor temp files) in the spool.
func sidecars(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".seg") {
			out = append(out, e.Name())
		}
	}
	return out
}

// deliverer records every payload handed out and fails where told to.
type deliverer struct {
	got    map[string]int
	order  []string
	failAt string // fail before accepting this payload
	err    error
}

func (d *deliverer) fn(r Record) error {
	p := string(r.Payload)
	if p == d.failAt {
		return d.err
	}
	if d.got == nil {
		d.got = map[string]int{}
	}
	d.got[p]++
	d.order = append(d.order, p)
	return nil
}

var errUpload = errors.New("upload failed")

// A record that fails part-way through a segment keeps itself and what
// follows; the records accepted before it are never handed out again.
func TestDrainResumesAfterFailedRecord(t *testing.T) {
	for _, segBytes := range []int64{0, 128} { // one segment, then several
		s, _ := newSpool(t, Options{SegmentBytes: segBytes})
		fill(t, s, 8)
		d := &deliverer{failAt: "rec-03", err: errUpload}
		n, err := s.Drain(d.fn)
		if !errors.Is(err, errUpload) || n != 3 {
			t.Fatalf("first drain n=%d err=%v", n, err)
		}
		if got := s.Stats().Records; got != 5 {
			t.Fatalf("queued after partial drain = %d, want 5", got)
		}
		// Fails again at the same record: still nothing earlier is replayed.
		if n, _ := s.Drain(d.fn); n != 0 {
			t.Fatalf("second drain delivered %d", n)
		}
		d.failAt = ""
		n, err = s.Drain(d.fn)
		if err != nil || n != 5 {
			t.Fatalf("final drain n=%d err=%v", n, err)
		}
		for i := 0; i < 8; i++ {
			p := fmt.Sprintf("rec-%02d", i)
			if d.got[p] != 1 || d.order[i] != p {
				t.Fatalf("segBytes=%d: %s delivered %d times; order %v", segBytes, p, d.got[p], d.order)
			}
		}
		if !s.Empty() || len(sidecars(t, s.Dir())) != 0 {
			t.Fatalf("left behind: %v", sidecars(t, s.Dir()))
		}
	}
}

// Progress survives a restart: a new process resumes after the last
// accepted record.
func TestDrainResumesAcrossRestart(t *testing.T) {
	s, key := newSpool(t, Options{})
	fill(t, s, 6)
	d := &deliverer{failAt: "rec-04", err: errUpload}
	s.Drain(d.fn)
	s.Close()

	re, err := Open(s.Dir(), key, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := re.Stats().Records; got != 2 {
		t.Fatalf("queued after restart = %d, want 2", got)
	}
	d.failAt = ""
	if n, err := re.Drain(d.fn); err != nil || n != 2 {
		t.Fatalf("drain after restart n=%d err=%v", n, err)
	}
	for p, c := range d.got {
		if c != 1 {
			t.Fatalf("%s delivered %d times", p, c)
		}
	}
	if len(d.got) != 6 || len(sidecars(t, re.Dir())) != 0 {
		t.Fatalf("got %v, left %v", d.got, sidecars(t, re.Dir()))
	}
}

// Crash after a record was accepted but before its progress was written:
// the next process hands out that one record again and nothing before it.
func TestCrashBeforeProgressRecorded(t *testing.T) {
	s, key := newSpool(t, Options{})
	fill(t, s, 6)
	got := map[string]int{}
	crashed := errors.New("killed")
	// The callback returning an error after the upload is the on-disk state
	// of a process killed between the upload and the cursor write.
	s.Drain(func(r Record) error {
		got[string(r.Payload)]++
		if string(r.Payload) == "rec-03" {
			return crashed
		}
		return nil
	})
	s.Close()

	re, _ := Open(s.Dir(), key, Options{})
	if n, err := re.Drain(func(r Record) error { got[string(r.Payload)]++; return nil }); err != nil || n != 3 {
		t.Fatalf("drain after crash n=%d err=%v", n, err)
	}
	for i := 0; i < 6; i++ {
		p, want := fmt.Sprintf("rec-%02d", i), 1
		if i == 3 {
			want = 2 // the one record whose acceptance was not recorded
		}
		if got[p] != want {
			t.Fatalf("%s delivered %d times, want %d (%v)", p, got[p], want, got)
		}
	}
}

// Crash after the last record's progress was written but before the
// segment was removed: nothing is handed out again, and the segment and
// its cursor are cleaned up.
func TestCrashBeforeSegmentRemoved(t *testing.T) {
	s, key := newSpool(t, Options{})
	fill(t, s, 4)
	s.Close()
	names, _ := s.segments()
	if len(names) != 1 {
		t.Fatalf("segments = %v", names)
	}
	_, nonce, err := s.readSegment(filepath.Join(s.Dir(), names[0]))
	if err != nil {
		t.Fatal(err)
	}
	// Every record accepted and recorded; the process dies before the remove.
	if ok, err := s.advanceLocked(names[0], 4, nonce); !ok || err != nil {
		t.Fatalf("advance ok=%v err=%v", ok, err)
	}

	re, _ := Open(s.Dir(), key, Options{})
	if got := re.Stats().Records; got != 0 {
		t.Fatalf("queued = %d, want 0", got)
	}
	n, err := re.Drain(func(r Record) error { t.Errorf("replayed %s", r.Payload); return nil })
	if err != nil || n != 0 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	if !re.Empty() || len(sidecars(t, re.Dir())) != 0 {
		t.Fatalf("left behind: %v", sidecars(t, re.Dir()))
	}
}

// Crash after the segment was removed but before its cursor was: the
// orphan is swept, and it is never applied to a later segment that gets
// the same name.
func TestOrphanCursorNeverApplied(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	s, key := newSpool(t, Options{Clock: clk})
	fill(t, s, 4)
	d := &deliverer{failAt: "rec-02", err: errUpload}
	s.Drain(d.fn)
	names, _ := s.segments()
	seg := filepath.Join(s.Dir(), names[0])
	stale, err := os.ReadFile(seg + cursorExt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(seg); err != nil { // the crash: segment gone, cursor left
		t.Fatal(err)
	}

	// Same clock and a fresh sequence: the next process reuses the name.
	re, _ := Open(s.Dir(), key, Options{Clock: clk})
	fill(t, re, 3)
	if again, _ := re.segments(); len(again) != 1 || again[0] != names[0] {
		t.Fatalf("name not reused: %v vs %v", again, names)
	}
	if len(sidecars(t, re.Dir())) != 0 {
		t.Fatalf("new segment inherited %v", sidecars(t, re.Dir()))
	}
	// Even put back after the segment was created, the old cursor does not
	// match the new segment's first record and is ignored.
	if err := os.WriteFile(seg+cursorExt, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := re.Stats().Records; got != 3 {
		t.Fatalf("queued = %d, want 3", got)
	}
	if n, err := re.Drain(func(Record) error { return nil }); err != nil || n != 3 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}

	// An orphan with no segment at all is swept by the next drain.
	if err := os.WriteFile(seg+cursorExt, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(seg+cursorTmpExt, stale, 0o600)
	re.Drain(func(Record) error { return nil })
	if len(sidecars(t, re.Dir())) != 0 {
		t.Fatalf("orphans left: %v", sidecars(t, re.Dir()))
	}
}

// A spool written by a build without cursors (0.7.0 to 0.8.2: segments
// only, same record format) drains from the start; a damaged cursor is
// treated the same way.
func TestSegmentWithoutCursorDrainsFromStart(t *testing.T) {
	s, key := newSpool(t, Options{SegmentBytes: 128})
	fill(t, s, 8)
	s.Close()
	if len(sidecars(t, s.Dir())) != 0 {
		t.Fatal("appending wrote a cursor")
	}
	names, _ := s.segments()
	for i, junk := range [][]byte{nil, []byte("KSC1"), bytes.Repeat([]byte{0xff}, 32)} {
		if i < len(names) && junk != nil {
			os.WriteFile(filepath.Join(s.Dir(), names[i]+cursorExt), junk, 0o600)
		}
	}
	re, _ := Open(s.Dir(), key, Options{SegmentBytes: 128})
	d := &deliverer{}
	if n, err := re.Drain(d.fn); err != nil || n != 8 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	for i, p := range d.order {
		if p != fmt.Sprintf("rec-%02d", i) {
			t.Fatalf("order %v", d.order)
		}
	}
	if len(sidecars(t, re.Dir())) != 0 {
		t.Fatalf("left behind: %v", sidecars(t, re.Dir()))
	}
}

// partial leaves a spool with one partly delivered segment (and so a
// cursor on disk).
func partial(t *testing.T, opt Options) (*Spool, []byte) {
	t.Helper()
	s, key := newSpool(t, opt)
	fill(t, s, 4)
	s.Drain((&deliverer{failAt: "rec-02", err: errUpload}).fn)
	if c := sidecars(t, s.Dir()); len(c) != 1 || !strings.HasSuffix(c[0], cursorExt) {
		t.Fatalf("expected one cursor, got %v", c)
	}
	return s, key
}

// Everything that removes a segment removes its cursor.
func TestCursorRemovedWithSegment(t *testing.T) {
	t.Run("purge", func(t *testing.T) {
		s, _ := partial(t, Options{})
		if err := s.Purge(); err != nil {
			t.Fatal(err)
		}
		if ents, _ := os.ReadDir(s.Dir()); len(ents) != 0 {
			t.Fatalf("left after purge: %v", ents)
		}
		// Nothing remembered either: new records all drain.
		fill(t, s, 3)
		if n, _ := s.Drain(func(Record) error { return nil }); n != 3 {
			t.Fatalf("drained %d after purge", n)
		}
	})
	t.Run("purge dir", func(t *testing.T) {
		s, _ := partial(t, Options{})
		s.Close()
		os.WriteFile(filepath.Join(s.Dir(), "1-000001.seg"+cursorTmpExt), []byte("x"), 0o600)
		if err := PurgeDir(s.Dir()); err != nil {
			t.Fatal(err)
		}
		if ents, _ := os.ReadDir(s.Dir()); len(ents) != 0 {
			t.Fatalf("left after PurgeDir: %v", ents)
		}
	})
	t.Run("age", func(t *testing.T) {
		clk := clock.NewFake(time.Unix(1_700_000_000, 0))
		s, _ := partial(t, Options{Clock: clk})
		clk.Advance(8 * 24 * time.Hour)
		if err := s.Prune(); err != nil {
			t.Fatal(err)
		}
		st := s.Stats()
		if ents, _ := os.ReadDir(s.Dir()); len(ents) != 0 || st.DroppedAge != 1 || st.DroppedTotal != 2 {
			t.Fatalf("left %v, stats %+v (2 of 4 records were still queued)", ents, st)
		}
	})
	t.Run("size", func(t *testing.T) {
		clk := clock.NewFake(time.Unix(1_700_000_000, 0))
		s, _ := partial(t, Options{Clock: clk, MaxBytes: 600, SegmentBytes: 400})
		for i := 0; i < 6; i++ {
			clk.Advance(time.Millisecond)
			s.Append("l", bytes.Repeat([]byte{'x'}, 150))
		}
		if st := s.Stats(); st.DroppedSize == 0 || st.Bytes > 600 {
			t.Fatalf("stats %+v", st)
		}
		if c := sidecars(t, s.Dir()); len(c) != 0 {
			t.Fatalf("cursor outlived its evicted segment: %v", c)
		}
	})
	t.Run("quarantine", func(t *testing.T) {
		s, _ := partial(t, Options{})
		s.Close()
		wrong, _ := Open(s.Dir(), make([]byte, 32), Options{})
		if n, _ := wrong.Drain(func(Record) error { return nil }); n != 0 || wrong.Stats().Quarantined != 1 {
			t.Fatalf("n=%d stats=%+v", n, wrong.Stats())
		}
		if ents, _ := os.ReadDir(s.Dir()); len(ents) != 0 {
			t.Fatalf("left after quarantine: %v", ents)
		}
	})
}

// A purge that lands while a record is being delivered leaves no cursor
// behind for the segment it removed.
func TestPurgeDuringDrainLeavesNoCursor(t *testing.T) {
	s, _ := newSpool(t, Options{SegmentBytes: 128})
	fill(t, s, 8)
	calls := 0
	n, err := s.Drain(func(Record) error {
		calls++
		if calls == 2 {
			if err := s.Purge(); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("drain n=%d calls=%d err=%v", n, calls, err)
	}
	if ents, _ := os.ReadDir(s.Dir()); len(ents) != 0 || s.Stats().Quarantined != 0 {
		t.Fatalf("left %v, stats %+v", ents, s.Stats())
	}
}

// The age limit is applied when a drain starts, not only when something
// is appended: an expired segment is dropped, never delivered late.
func TestDrainEnforcesAge(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	s, _ := newSpool(t, Options{Clock: clk})
	fill(t, s, 3)
	clk.Advance(8 * 24 * time.Hour)
	n, err := s.Drain(func(r Record) error { t.Errorf("delivered expired %s", r.Payload); return nil })
	if err != nil || n != 0 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	if st := s.Stats(); st.DroppedAge != 1 || st.DroppedTotal != 3 || !s.Empty() {
		t.Fatalf("stats %+v", st)
	}
}

// When the cursor cannot be written the drain goes on and reports it, and
// this process still never hands an accepted record out twice.
func TestCursorWriteFailureDoesNotReplay(t *testing.T) {
	s, _ := newSpool(t, Options{})
	fill(t, s, 5)
	s.Close()
	names, _ := s.segments()
	// A non-empty directory where the cursor goes: the rename fails.
	block := filepath.Join(s.Dir(), names[0]+cursorExt)
	if err := os.MkdirAll(filepath.Join(block, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	d := &deliverer{failAt: "rec-03", err: errUpload}
	if n, err := s.Drain(d.fn); !errors.Is(err, errUpload) || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	d.failAt = ""
	n, err := s.Drain(d.fn)
	if n != 2 || err == nil {
		t.Fatalf("n=%d err=%v (want the cursor error reported)", n, err)
	}
	for p, c := range d.got {
		if c != 1 {
			t.Fatalf("%s delivered %d times", p, c)
		}
	}
	if len(d.got) != 5 {
		t.Fatalf("got %v", d.got)
	}
}

// A half-written segment a crash left behind is never read, and the next
// drain and purge remove it.
func TestHalfWrittenSegmentIgnored(t *testing.T) {
	s, _ := newSpool(t, Options{})
	fill(t, s, 2)
	tmp := filepath.Join(s.Dir(), "1-000009"+segTmpExt)
	os.WriteFile(tmp, []byte("partial"), 0o600)
	if st := s.Stats(); st.Segments != 1 || st.Records != 2 || st.Quarantined != 0 {
		t.Fatalf("stats %+v", st)
	}
	if n, err := s.Drain(func(Record) error { return nil }); err != nil || n != 2 || s.Stats().Quarantined != 0 {
		t.Fatalf("drain n=%d err=%v stats=%+v", n, err, s.Stats())
	}
	if ents, _ := os.ReadDir(s.Dir()); len(ents) != 0 {
		t.Fatalf("left: %v", ents)
	}
	os.WriteFile(tmp, []byte("partial"), 0o600)
	if err := PurgeDir(s.Dir()); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(s.Dir()); len(ents) != 0 {
		t.Fatalf("left after purge: %v", ents)
	}
}

// AppendAll queues the set as one segment after everything appended so far.
func TestAppendAllOrder(t *testing.T) {
	s, _ := newSpool(t, Options{SegmentBytes: 64})
	s.Append("l", []byte("first"))
	if err := s.AppendAll("l", "", [][]byte{[]byte("a"), []byte("b"), bytes.Repeat([]byte{'c'}, 200)}); err != nil {
		t.Fatal(err)
	}
	s.Append("l", []byte("last"))
	if st := s.Stats(); st.Segments != 3 || st.Records != 5 {
		t.Fatalf("stats %+v", st)
	}
	var got []string
	s.Drain(func(r Record) error { got = append(got, string(r.Payload[:1])); return nil })
	if strings.Join(got, "") != "fabcl" {
		t.Fatalf("order %v", got)
	}
}

// A cursor that claims more records than its segment holds (it matches
// the segment's first nonce, so it is not simply stale) is not one the
// segment could have produced: the segment drains from the start and is
// never skipped.
func TestCursorPastEndOfSegmentIgnored(t *testing.T) {
	s, key := newSpool(t, Options{})
	fill(t, s, 3)
	s.Close()
	names, _ := s.segments()
	_, nonce, err := s.readSegment(filepath.Join(s.Dir(), names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.advanceLocked(names[0], 4, nonce); !ok || err != nil {
		t.Fatalf("advance ok=%v err=%v", ok, err)
	}
	re, _ := Open(s.Dir(), key, Options{})
	d := &deliverer{}
	if n, err := re.Drain(d.fn); err != nil || n != 3 {
		t.Fatalf("drain n=%d err=%v (want all 3 records)", n, err)
	}
	if len(sidecars(t, re.Dir())) != 0 || !re.Empty() {
		t.Fatalf("left: %v", sidecars(t, re.Dir()))
	}
}

// A segment evicted for size while one of its records is being handled
// (the callback appends) counts only the records that were really lost.
func TestEvictionDuringDrainCountsOnlyLostRecords(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	s, _ := newSpool(t, Options{Clock: clk, MaxBytes: 700})
	fill(t, s, 4)
	calls := 0
	n, err := s.Drain(func(Record) error {
		calls++
		clk.Advance(time.Millisecond)
		// Pushes the spool over its cap: the segment being drained goes.
		return s.AppendAll("l", "", [][]byte{bytes.Repeat([]byte{'x'}, 600)})
	})
	if err != nil || n != 1 || calls != 1 {
		t.Fatalf("drain n=%d calls=%d err=%v", n, calls, err)
	}
	// Record 0 was handled; records 1 to 3 were lost.
	if st := s.Stats(); st.DroppedSize != 1 || st.DroppedTotal != 3 || st.Records != 1 {
		t.Fatalf("stats %+v, want 3 records lost and 1 queued", st)
	}
	if len(sidecars(t, s.Dir())) != 0 {
		t.Fatalf("left: %v", sidecars(t, s.Dir()))
	}
}

// A set whose write fails part-way (a full disk) leaves nothing queued and
// no partial file, however often it is retried; once it fits, each record
// is queued once.
func TestAppendAllIsAllOrNothing(t *testing.T) {
	s, _ := newSpool(t, Options{})
	fill(t, s, 2)
	set := [][]byte{bytes.Repeat([]byte{'a'}, 300), bytes.Repeat([]byte{'b'}, 300), bytes.Repeat([]byte{'c'}, 300)}

	full := errors.New("no space left on device")
	writeSegment = func(name string, b []byte, mode os.FileMode) error {
		// The first record of the set and part of the second reach the disk.
		if err := os.WriteFile(name, b[:500], mode); err != nil {
			return err
		}
		return full
	}
	defer func() { writeSegment = writeSynced }()
	for i := 0; i < 5; i++ {
		if err := s.AppendAll("l", "", set); !errors.Is(err, full) {
			t.Fatalf("err = %v", err)
		}
	}
	if ents, _ := os.ReadDir(s.Dir()); len(ents) != 1 || s.Stats().Records != 2 {
		t.Fatalf("after 5 failed writes: %d files and %d records queued (want 1 and 2)", len(ents), s.Stats().Records)
	}
	writeSegment = writeSynced

	if err := s.AppendAll("l", "", set); err != nil {
		t.Fatal(err)
	}
	var got []string
	if n, err := s.Drain(func(r Record) error { got = append(got, string(r.Payload[:1])); return nil }); err != nil || n != 5 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	if strings.Join(got, "") != "rrabc" {
		t.Fatalf("got %v, want the two earlier records then a, b, c once each", got)
	}
}

// A set written for a record is written once: while it is queued, another
// AppendAll for the same origin writes nothing. A caller interrupted
// between queueing the set and its record being marked delivered (the
// callback fails after AppendAll) is handed the record again and cannot
// queue a second set.
func TestAppendAllOncePerOrigin(t *testing.T) {
	s, key := newSpool(t, Options{})
	fill(t, s, 2)
	killed := errors.New("killed")
	_, err := s.Drain(func(r Record) error {
		if r.ID == "" {
			t.Fatal("record without an id")
		}
		if err := s.AppendAll("l", r.ID, [][]byte{[]byte("first-a"), []byte("first-b")}); err != nil {
			t.Fatal(err)
		}
		return killed // dies before the record is marked delivered
	})
	if !errors.Is(err, killed) {
		t.Fatal(err)
	}
	s.Close()

	re, _ := Open(s.Dir(), key, Options{})
	var got []string
	ids := map[string]bool{}
	fn := func(r Record) error {
		p := string(r.Payload)
		if strings.HasPrefix(p, "rec-") {
			ids[r.ID] = true
			// Handed out again: what it writes now must not be queued twice.
			return re.AppendAll("l", r.ID, [][]byte{[]byte("second-" + p)})
		}
		got = append(got, p)
		return nil
	}
	// The two records and rec-00's set; then the set rec-01 just became.
	if n, err := re.Drain(fn); err != nil || n != 4 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	if n, err := re.Drain(fn); err != nil || n != 1 {
		t.Fatalf("second drain n=%d err=%v", n, err)
	}
	// rec-00's set from before the crash, once, and rec-01's.
	if strings.Join(got, ",") != "first-a,first-b,second-rec-01" || len(ids) != 2 {
		t.Fatalf("got %v (ids %d)", got, len(ids))
	}
	if !re.Empty() || len(sidecars(t, re.Dir())) != 0 {
		t.Fatalf("left: %v", sidecars(t, re.Dir()))
	}
}

// The relay dies while a set is being written (part of the file is on
// disk, no cleanup runs). Nothing of it is ever drainable: the bytes are
// under the temporary name, never under a segment name, so the next
// process hands out nothing from them and does not see a corrupt segment.
func TestAppendAllCrashMidWriteLeavesNoSegment(t *testing.T) {
	s, key := newSpool(t, Options{})
	fill(t, s, 2)
	writeSegment = func(name string, b []byte, mode os.FileMode) error {
		if err := os.WriteFile(name, b[:len(b)/2], mode); err != nil {
			t.Fatal(err)
		}
		panic("killed mid-write")
	}
	defer func() { writeSegment = writeSynced }()
	func() {
		defer func() { recover() }()
		s.AppendAll("l", "", [][]byte{bytes.Repeat([]byte{'a'}, 300), bytes.Repeat([]byte{'b'}, 300)})
	}()
	writeSegment = writeSynced
	segs := 0
	ents, _ := os.ReadDir(s.Dir())
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".seg") {
			segs++
		}
	}
	if segs != 1 || len(ents) != 2 {
		t.Fatalf("%d segments among %d files, want the one earlier segment and the temporary file", segs, len(ents))
	}

	re, _ := Open(s.Dir(), key, Options{})
	var got []string
	n, err := re.Drain(func(r Record) error { got = append(got, string(r.Payload)); return nil })
	if err != nil || n != 2 || strings.Join(got, ",") != "rec-00,rec-01" || re.Stats().Quarantined != 0 {
		t.Fatalf("drain n=%d err=%v got=%v stats=%+v", n, err, got, re.Stats())
	}
	if ents, _ := os.ReadDir(re.Dir()); len(ents) != 0 {
		t.Fatalf("left: %v", ents)
	}
}

// AppendAll returns only once the set is durable: a failed fsync of the
// file or of the directory is returned and leaves nothing queued, so the
// caller does not mark the origin record as delivered.
func TestAppendAllSyncFailureQueuesNothing(t *testing.T) {
	boom := errors.New("fsync failed")
	set := [][]byte{[]byte("a"), []byte("b")}
	for _, step := range []string{"file", "dir"} {
		t.Run(step, func(t *testing.T) {
			s, _ := newSpool(t, Options{})
			fill(t, s, 2)
			var fileSyncs, dirSyncs int
			syncFile = func(f *os.File) error {
				fileSyncs++
				if step == "file" {
					return boom
				}
				return f.Sync()
			}
			syncSegmentDir = func(dir string) error {
				dirSyncs++
				if step == "dir" {
					return boom
				}
				return syncDir(dir)
			}
			restore := func() { syncFile, syncSegmentDir = func(f *os.File) error { return f.Sync() }, syncDir }
			defer restore()
			for i := 0; i < 3; i++ {
				if err := s.AppendAll("l", "origin", set); !errors.Is(err, boom) {
					t.Fatalf("err = %v, want the sync error", err)
				}
			}
			if ents, _ := os.ReadDir(s.Dir()); len(ents) != 1 || s.Stats().Records != 2 {
				t.Fatalf("after failed syncs: %d files, %d records queued (want 1 and 2)", len(ents), s.Stats().Records)
			}
			if fileSyncs != 3 || (step == "dir" && dirSyncs != 3) {
				t.Fatalf("file synced %d times, directory %d times", fileSyncs, dirSyncs)
			}
			restore()
			// Works again: the set is queued once, after a file and a directory sync.
			syncFile = func(f *os.File) error { fileSyncs++; return f.Sync() }
			syncSegmentDir = func(dir string) error { dirSyncs++; return syncDir(dir) }
			fileSyncs, dirSyncs = 0, 0
			if err := s.AppendAll("l", "origin", set); err != nil {
				t.Fatal(err)
			}
			if fileSyncs != 1 || dirSyncs != 1 || s.Stats().Records != 4 {
				t.Fatalf("file synced %d times, directory %d times, %d queued", fileSyncs, dirSyncs, s.Stats().Records)
			}
		})
	}
}

// The wall clock goes back while a record is turned into a set, and the
// relay dies before the record is marked delivered. The set still sorts
// after the record's segment, so the next process meets the record first,
// writes nothing for it again, and the set is delivered once.
func TestAppendAllSortsAfterOriginWhenClockGoesBack(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	s, key := newSpool(t, Options{Clock: clk})
	s.Append("l", []byte("HELD"))
	s.Drain(func(r Record) error {
		clk.Advance(-2 * time.Hour)
		if err := s.AppendAll("l", r.ID, [][]byte{[]byte("first")}); err != nil {
			t.Fatal(err)
		}
		return errors.New("killed")
	})
	s.Close()
	if names, _ := s.segments(); len(names) != 2 || !strings.Contains(names[1], ".w") {
		t.Fatalf("segment order %v, want the set after its origin", names)
	}

	re, _ := Open(s.Dir(), key, Options{Clock: clk})
	var got []string
	for i := 0; i < 4; i++ {
		re.Drain(func(r Record) error {
			if string(r.Payload) == "HELD" {
				return re.AppendAll("l", r.ID, [][]byte{[]byte("second")})
			}
			got = append(got, string(r.Payload))
			return nil
		})
	}
	if strings.Join(got, ",") != "first" || !re.Empty() {
		t.Fatalf("delivered %v, want the first set once", got)
	}
}
