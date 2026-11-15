// Package spool is the bounded, encrypted on-disk queue for uploads held
// while the org is paused, locked down or awaiting a blocking approval.
//
// It only ever holds payloads that are already projected onto Fleet's
// closed schema: raw harness content never reaches the spool.
//
// Layout: $STATE/spool/<unix-nanos>-<seq>.seg, append-only segments that
// roll at SegmentBytes (and <unix-nanos>-<seq>.w<record id>.seg for a set
// written by AppendAll with an origin). Each record is sealed independently with
// XChaCha20-Poly1305 under a 32-byte key held in the OS keychain, or in an
// owner-only file when the device has none (see KeyStore and FileKeys), with
// a random 24-byte nonce:
//
//	record := u32be(len(sealed)) || nonce(24) || sealed
//	plaintext := u8(len(lane)) || lane || payload
//
// Bounds: MaxBytes total and MaxAge per segment; the oldest segments are
// dropped first and every drop is counted.
//
// Delivery: a record is handed to the drain callback once. After each
// accepted record a per-segment cursor (<segment>.cur, see cursor.go) is
// written atomically, so a drain that stops part-way, or a restart,
// resumes after the last accepted record. The one exception is a crash
// between a record being accepted and its cursor being written: that
// single record is delivered again. AppendAll queues a set of records as
// one segment, all or none.
//
// Append does not fsync: after power loss a recently appended record can
// be missing. AppendAll and the cursor do.
package spool

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/kameas-ai/kenaz-plugin/internal/clock"
)

// Defaults.
const (
	DefaultMaxBytes     int64 = 50 << 20
	DefaultMaxAge             = 7 * 24 * time.Hour
	DefaultSegmentBytes int64 = 1 << 20
	maxRecord                 = 16 << 20
)

// ErrCorrupt means a segment failed to decrypt or parse (wrong key,
// truncation or tampering). The segment is quarantined, never delivered.
var ErrCorrupt = errors.New("spool: corrupt segment")

// Options configures a Spool. Zero values take the defaults.
type Options struct {
	MaxBytes     int64
	MaxAge       time.Duration
	SegmentBytes int64
	Clock        clock.Clock
}

// Stats is reported by status and fleet_status (counts only).
type Stats struct {
	Segments     int   `json:"segments"`
	Bytes        int64 `json:"bytes"`
	Records      int   `json:"records"`
	DroppedSize  int   `json:"dropped_size"`  // segments evicted by MaxBytes
	DroppedAge   int   `json:"dropped_age"`   // segments evicted by MaxAge
	Quarantined  int   `json:"quarantined"`   // corrupt segments removed
	DroppedTotal int   `json:"dropped_total"` // records lost to eviction
}

// Spool is safe for concurrent use within one process. The daemon's
// lockfile guarantees a single process per state directory.
type Spool struct {
	dir  string
	aead interface {
		Seal(dst, nonce, plaintext, ad []byte) []byte
		Open(dst, nonce, ciphertext, ad []byte) ([]byte, error)
		NonceSize() int
	}
	opt Options

	mu      sync.Mutex
	active  *tracked // the writer of the newest segment; closed before that segment is removed
	activeN int64
	seq     int
	stats   Stats
	counts  map[string]int // records per segment name
	done    map[string]int // delivered leading records per segment name
	// inflight is the segment whose record the drain callback is handling
	// now; inflightEvicted is set when that segment is evicted meanwhile.
	inflight        string
	inflightEvicted bool
}

// Open opens (creating) dir with key. len(key) must be 32.
func Open(dir string, key []byte, opt Options) (*Spool, error) {
	if len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("spool: key must be %d bytes", chacha20poly1305.KeySize)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = DefaultMaxBytes
	}
	if opt.MaxAge <= 0 {
		opt.MaxAge = DefaultMaxAge
	}
	if opt.SegmentBytes <= 0 {
		opt.SegmentBytes = DefaultSegmentBytes
	}
	if opt.Clock == nil {
		opt.Clock = clock.Real{}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, aead: aead, opt: opt, counts: map[string]int{}, done: map[string]int{}}
	return s, nil
}

// Dir returns the spool directory.
func (s *Spool) Dir() string { return s.dir }

func (s *Spool) segments() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".seg") {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return segTime(names[i]).Before(segTime(names[j])) || (segTime(names[i]).Equal(segTime(names[j])) && names[i] < names[j])
	})
	return names, nil
}

func segTime(name string) time.Time {
	ts, _, _ := strings.Cut(strings.TrimSuffix(name, ".seg"), "-")
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (s *Spool) roll() error {
	if s.active != nil {
		if err := s.active.Close(); err != nil {
			return err
		}
		s.active = nil
	}
	s.seq++
	name := fmt.Sprintf("%d-%06d.seg", s.opt.Clock.Now().UnixNano(), s.seq)
	f, err := openTracked(filepath.Join(s.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	s.active, s.activeN = f, 0
	s.dropCursorLocked(name) // a new segment never inherits a cursor
	return nil
}

// seal returns the on-disk form of one record for lane.
func (s *Spool) seal(lane string, payload []byte) ([]byte, error) {
	if len(lane) == 0 || len(lane) > 255 {
		return nil, errors.New("spool: lane name must be 1..255 bytes")
	}
	if len(payload) > maxRecord {
		return nil, errors.New("spool: record too large")
	}
	pt := make([]byte, 0, 1+len(lane)+len(payload))
	pt = append(pt, byte(len(lane)))
	pt = append(pt, lane...)
	pt = append(pt, payload...)
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := s.aead.Seal(nil, nonce, pt, nil)
	rec := make([]byte, 4, 4+len(nonce)+len(sealed))
	binary.BigEndian.PutUint32(rec, uint32(len(nonce)+len(sealed)))
	rec = append(rec, nonce...)
	rec = append(rec, sealed...)
	return rec, nil
}

// Append seals and writes one record for lane, then enforces bounds.
func (s *Spool) Append(lane string, payload []byte) error {
	rec, err := s.seal(lane, payload)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.activeN+int64(len(rec)) > s.opt.SegmentBytes {
		if err := s.roll(); err != nil {
			return err
		}
	}
	if _, err := s.active.Write(rec); err != nil {
		return err
	}
	s.activeN += int64(len(rec))
	s.counts[filepath.Base(s.active.Name())]++
	return s.enforceLocked()
}

// Steps of AppendAll's durable write, as variables so tests can make each
// one fail: writeSegment writes and fsyncs a whole new segment file under
// its temporary name, syncFile is the fsync inside it, and syncSegmentDir
// fsyncs the spool directory after the rename.
var (
	writeSegment   = writeSynced
	syncFile       = func(f *os.File) error { return f.Sync() }
	syncSegmentDir = syncDir
)

// writeSynced writes b to a new file and fsyncs it before closing.
func writeSynced(name string, b []byte, mode os.FileMode) error {
	f, err := openTracked(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := syncFile(f.File); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// AppendAll writes payloads as records of one new segment, all or none:
// the segment is written under a temporary name and renamed into place,
// so a failed or interrupted write leaves no part of the set queued. The
// segment may exceed SegmentBytes.
//
// Unlike Append, the set is durable when AppendAll returns nil: the file
// is fsynced before the rename and the directory after it (the directory
// sync is a no-op on Windows, see syncDir). A caller that then records the
// origin record as delivered (the cursor, which is also fsynced) can rely
// on the set surviving power loss. The segment is named to sort after
// every segment already queued, even when the wall clock went back, so it
// is never delivered before the record it was made from.
//
// origin, when not empty, is the Record.ID of the record the set was made
// from, and makes the call idempotent: the segment carries it in its name,
// and while that segment is queued a second AppendAll for the same origin
// writes nothing and returns nil. A caller that turns one record into a
// set, and is interrupted after the set was queued but before the record
// was marked delivered, therefore cannot queue a second set for it.
func (s *Spool) AppendAll(lane, origin string, payloads [][]byte) error {
	if len(payloads) == 0 {
		return nil
	}
	suffix := ".seg"
	if origin != "" {
		if strings.ContainsAny(origin, "/\\.") {
			return errors.New("spool: bad origin")
		}
		suffix = ".w" + origin + ".seg"
	}
	var buf []byte
	for _, p := range payloads {
		rec, err := s.seal(lane, p)
		if err != nil {
			return err
		}
		buf = append(buf, rec...)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if origin != "" {
		names, err := s.segments()
		if err != nil {
			return err
		}
		for _, n := range names {
			if strings.HasSuffix(n, suffix) {
				return nil // this record's set is already queued
			}
		}
	}
	if s.active != nil { // the new segment sorts after everything appended so far
		if err := s.active.Close(); err != nil {
			return err
		}
		s.active = nil
	}
	names, err := s.segments()
	if err != nil {
		return err
	}
	ts := s.opt.Clock.Now()
	if len(names) > 0 {
		// names are sorted by time: never sort before the newest one.
		if last := segTime(names[len(names)-1]); !ts.After(last) {
			ts = last.Add(time.Nanosecond)
		}
	}
	s.seq++
	name := fmt.Sprintf("%d-%06d%s", ts.UnixNano(), s.seq, suffix)
	p := filepath.Join(s.dir, name)
	if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("spool: segment %s exists", name)
	}
	tmp := strings.TrimSuffix(p, ".seg") + segTmpExt
	if err := writeSegment(tmp, buf, 0o600); err != nil {
		removeFile(tmp)
		return err
	}
	if err := renameFile(tmp, p); err != nil {
		removeFile(tmp)
		return err
	}
	if err := syncSegmentDir(s.dir); err != nil {
		// Not known to be durable: take it back so nothing is queued and
		// the caller keeps the record it came from.
		removeFile(p)
		return err
	}
	s.dropCursorLocked(name) // a new segment never inherits a cursor
	s.counts[name] = len(payloads)
	return s.enforceLocked()
}

// Prune applies MaxAge and MaxBytes now (the daemon calls it periodically).
func (s *Spool) Prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enforceLocked()
}

func (s *Spool) activeName() string {
	if s.active == nil {
		return ""
	}
	return filepath.Base(s.active.Name())
}

func (s *Spool) enforceLocked() error {
	names, err := s.segments()
	if err != nil {
		return err
	}
	now := s.opt.Clock.Now()
	var total int64
	sizes := make(map[string]int64, len(names))
	for _, n := range names {
		fi, err := os.Stat(filepath.Join(s.dir, n))
		if err != nil {
			continue
		}
		sizes[n] = fi.Size()
		total += fi.Size()
	}
	for _, n := range names {
		expired := now.Sub(segTime(n)) > s.opt.MaxAge
		over := total > s.opt.MaxBytes
		if !expired && !over {
			break
		}
		if n == s.activeName() {
			if !expired && len(names) == 1 {
				break // never evict the only (active) segment for size
			}
			s.active.Close()
			s.active = nil
		}
		lost := s.pendingLocked(n)
		if n == s.inflight {
			s.inflightEvicted = true
		}
		if err := s.removeSegmentLocked(n); err != nil {
			return err
		}
		total -= sizes[n]
		if expired {
			s.stats.DroppedAge++
		} else {
			s.stats.DroppedSize++
		}
		s.stats.DroppedTotal += lost
	}
	return nil
}

// recordCount returns the records in a segment, counting by walking the
// length prefixes when the in-memory count is unknown (segments from a
// previous daemon run). Caller holds mu.
func (s *Spool) recordCount(name string) int {
	if c, ok := s.counts[name]; ok {
		return c
	}
	f, err := openTracked(filepath.Join(s.dir, name), os.O_RDONLY, 0)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			return n
		}
		if _, err := f.Seek(int64(binary.BigEndian.Uint32(hdr[:])), io.SeekCurrent); err != nil {
			return n
		}
		n++
	}
}

// pendingLocked returns the records in a segment not yet delivered.
// Caller holds mu.
func (s *Spool) pendingLocked(name string) int {
	if n := s.recordCount(name) - s.progressLocked(name); n > 0 {
		return n
	}
	return 0
}

// Record is one spooled payload.
type Record struct {
	Lane    string
	Payload []byte
	// ID identifies this record on disk (its nonce, in hex). It is the
	// same on every drain and after a restart, and no other record has it.
	ID string
}

// Drain delivers segments oldest first, after applying MaxAge and
// MaxBytes. fn is called per record not yet delivered; a non-nil error
// stops draining and keeps that record and everything after it. Each
// accepted record advances the segment's cursor before the next one is
// handed out, so an accepted record is not handed out again (see the
// package comment for the one crash window). The active segment is rolled
// first so everything appended so far drains.
func (s *Spool) Drain(fn func(Record) error) (delivered int, err error) {
	s.mu.Lock()
	if s.active != nil {
		s.active.Close()
		s.active = nil
	}
	s.sweepCursorsLocked()
	err = s.enforceLocked()
	var names []string
	if err == nil {
		names, err = s.segments()
	}
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	var cursorErr error
	for _, n := range names {
		p := filepath.Join(s.dir, n)
		// Read under mu, and close before anything can remove the segment.
		s.mu.Lock()
		recs, nonce, err := s.readSegment(p)
		if errors.Is(err, os.ErrNotExist) {
			s.mu.Unlock()
			continue // purged or evicted since the listing
		}
		if err != nil {
			s.removeSegmentLocked(n)
			s.stats.Quarantined++
			s.mu.Unlock()
			continue
		}
		start := s.progressLocked(n)
		s.mu.Unlock()
		if start > len(recs) {
			start = 0 // not a cursor this segment could have produced
		}
		gone := false
		for i := start; i < len(recs) && !gone; i++ {
			s.mu.Lock()
			s.inflight, s.inflightEvicted = n, false
			s.mu.Unlock()
			ferr := fn(recs[i])
			s.mu.Lock()
			s.inflight = ""
			if ferr != nil {
				s.mu.Unlock()
				return delivered, ferr
			}
			delivered++
			if s.inflightEvicted && s.stats.DroppedTotal > 0 {
				// Evicted while this record was being handled (the callback
				// can append): it was counted as lost, but it was handled.
				s.stats.DroppedTotal--
			}
			ok, err := s.advanceLocked(n, i+1, nonce)
			s.mu.Unlock()
			if err != nil && cursorErr == nil {
				cursorErr = err
			}
			gone = !ok
		}
		if gone {
			continue // purged or evicted mid-drain: its records are not ours any more
		}
		s.mu.Lock()
		s.removeSegmentLocked(n)
		s.mu.Unlock()
	}
	return delivered, cursorErr
}

// readingSegment is called, in tests only, while readSegment holds the
// segment open.
var readingSegment func(path string)

// readSegment returns a segment's records and the nonce of its first
// record (nil when it has none). The caller holds mu: a segment is never
// open for reading while a purge or an eviction (which take mu) removes it,
// which Windows would refuse.
func (s *Spool) readSegment(path string) ([]Record, []byte, error) {
	f, err := openTracked(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	if readingSegment != nil {
		readingSegment(path)
	}
	var out []Record
	var first []byte
	var hdr [4]byte
	ns := s.aead.NonceSize()
	for {
		if _, err := io.ReadFull(f, hdr[:]); err == io.EOF {
			return out, first, nil
		} else if err != nil {
			return nil, nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n < uint32(ns) || n > maxRecord+uint32(ns)+64 {
			return nil, nil, ErrCorrupt
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, nil, ErrCorrupt
		}
		pt, err := s.aead.Open(nil, buf[:ns], buf[ns:], nil)
		if err != nil || len(pt) < 1 || len(pt) < 1+int(pt[0]) {
			return nil, nil, ErrCorrupt
		}
		if first == nil {
			first = buf[:ns:ns]
		}
		ll := int(pt[0])
		out = append(out, Record{Lane: string(pt[1 : 1+ll]), Payload: pt[1+ll:], ID: hex.EncodeToString(buf[:ns])})
	}
}

// Purge deletes every segment and cursor (logout, withdrawal, node removed).
func (s *Spool) Purge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		s.active.Close()
		s.active = nil
	}
	errs := []error{purgeFiles(s.dir)}
	s.counts, s.done = map[string]int{}, map[string]int{}
	// Make the removals durable so a crash cannot bring purged segments
	// back.
	if err := syncDir(s.dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Dropped returns how many records eviction has discarded so far
// (Stats.DroppedTotal) without walking the segments.
func (s *Spool) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats.DroppedTotal
}

// Stats returns current counters.
func (s *Spool) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	names, _ := s.segments()
	st.Segments = len(names)
	for _, n := range names {
		if fi, err := os.Stat(filepath.Join(s.dir, n)); err == nil {
			st.Bytes += fi.Size()
		}
		st.Records += s.pendingLocked(n)
	}
	return st
}

// Empty reports whether nothing is queued (used by the daemon's idle exit).
func (s *Spool) Empty() bool {
	names, err := func() ([]string, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.segments() }()
	if err != nil {
		return false
	}
	for _, n := range names {
		if fi, err := os.Stat(filepath.Join(s.dir, n)); err == nil && fi.Size() > 0 {
			return false
		}
	}
	return true
}

// Close closes the active segment.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		err := s.active.Close()
		s.active = nil
		return err
	}
	return nil
}

// PurgeDir removes every segment and cursor in dir without needing the
// key (logout from a process that does not hold the spool open).
func PurgeDir(dir string) error {
	return purgeFiles(dir)
}

// PurgeDirWait is PurgeDir for a caller that has just asked the process
// holding the spool to stop: it retries until everything is removed or
// wait has passed, and returns what could still not be removed. Windows
// refuses to remove a segment while that process has it open, so a single
// attempt made a moment too early would leave it on disk.
func PurgeDirWait(dir string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := purgeFiles(dir)
		if err == nil || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// purgeFiles removes segments first, then cursors, so a crash part-way
// never leaves a segment without the cursor that says what was sent.
func purgeFiles(dir string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, sidecars := range []bool{false, true} {
		for _, e := range ents {
			if n := e.Name(); (!sidecars && (strings.HasSuffix(n, ".seg") || strings.HasSuffix(n, segTmpExt))) || (sidecars && isSidecar(n)) {
				if err := removeFile(filepath.Join(dir, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}
