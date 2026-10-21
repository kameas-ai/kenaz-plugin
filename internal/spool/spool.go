// Package spool is the bounded, encrypted on-disk queue for uploads held
// while the org is paused, locked down or awaiting a blocking approval.
//
// It only ever holds payloads that are already projected onto Fleet's
// closed schema: raw harness content never reaches the spool.
//
// Layout: $STATE/spool/<unix-nanos>-<seq>.seg, append-only segments that
// roll at SegmentBytes. Each record is sealed independently with
// XChaCha20-Poly1305 under a 32-byte key held in the OS keychain, or in an
// owner-only file when the device has none (see KeyStore and FileKeys), with
// a random 24-byte nonce:
//
//	record := u32be(len(sealed)) || nonce(24) || sealed
//	plaintext := u8(len(lane)) || lane || payload
//
// Bounds: MaxBytes total and MaxAge per segment; the oldest segments are
// dropped first and every drop is counted. Delivery is at-least-once: a
// segment is deleted only after every record in it was accepted.
package spool

import (
	"crypto/rand"
	"encoding/binary"
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
	active  *os.File
	activeN int64
	seq     int
	stats   Stats
	counts  map[string]int // records per segment name
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
	s := &Spool{dir: dir, aead: aead, opt: opt, counts: map[string]int{}}
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
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	s.active, s.activeN = f, 0
	return nil
}

// Append seals and writes one record for lane, then enforces bounds.
func (s *Spool) Append(lane string, payload []byte) error {
	if len(lane) == 0 || len(lane) > 255 {
		return errors.New("spool: lane name must be 1..255 bytes")
	}
	if len(payload) > maxRecord {
		return errors.New("spool: record too large")
	}
	pt := make([]byte, 0, 1+len(lane)+len(payload))
	pt = append(pt, byte(len(lane)))
	pt = append(pt, lane...)
	pt = append(pt, payload...)
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nil, nonce, pt, nil)
	rec := make([]byte, 4, 4+len(nonce)+len(sealed))
	binary.BigEndian.PutUint32(rec, uint32(len(nonce)+len(sealed)))
	rec = append(rec, nonce...)
	rec = append(rec, sealed...)

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
		if err := os.Remove(filepath.Join(s.dir, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		total -= sizes[n]
		if expired {
			s.stats.DroppedAge++
		} else {
			s.stats.DroppedSize++
		}
		s.stats.DroppedTotal += s.recordCount(n)
		delete(s.counts, n)
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
	f, err := os.Open(filepath.Join(s.dir, name))
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

// Record is one spooled payload.
type Record struct {
	Lane    string
	Payload []byte
}

// Drain delivers segments oldest first. fn is called per record; a
// non-nil error stops draining and keeps that segment (at-least-once).
// The active segment is rolled first so everything appended so far drains.
func (s *Spool) Drain(fn func(Record) error) (delivered int, err error) {
	s.mu.Lock()
	if s.active != nil {
		s.active.Close()
		s.active = nil
	}
	names, err := s.segments()
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	for _, n := range names {
		p := filepath.Join(s.dir, n)
		recs, err := s.readSegment(p)
		if err != nil {
			s.mu.Lock()
			os.Remove(p)
			s.stats.Quarantined++
			delete(s.counts, n)
			s.mu.Unlock()
			continue
		}
		for _, r := range recs {
			if err := fn(r); err != nil {
				return delivered, err
			}
			delivered++
		}
		s.mu.Lock()
		os.Remove(p)
		delete(s.counts, n)
		s.mu.Unlock()
	}
	return delivered, nil
}

func (s *Spool) readSegment(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	var hdr [4]byte
	ns := s.aead.NonceSize()
	for {
		if _, err := io.ReadFull(f, hdr[:]); err == io.EOF {
			return out, nil
		} else if err != nil {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n < uint32(ns) || n > maxRecord+uint32(ns)+64 {
			return nil, ErrCorrupt
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, ErrCorrupt
		}
		pt, err := s.aead.Open(nil, buf[:ns], buf[ns:], nil)
		if err != nil || len(pt) < 1 || len(pt) < 1+int(pt[0]) {
			return nil, ErrCorrupt
		}
		ll := int(pt[0])
		out = append(out, Record{Lane: string(pt[1 : 1+ll]), Payload: pt[1+ll:]})
	}
}

// Purge deletes every segment (logout, withdrawal, node removed).
func (s *Spool) Purge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		s.active.Close()
		s.active = nil
	}
	names, err := s.segments()
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		if err := os.Remove(filepath.Join(s.dir, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	s.counts = map[string]int{}
	// Make the removals durable so a crash cannot bring purged segments
	// back.
	if err := syncDir(s.dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// syncDir fsyncs a directory so entry removals and renames in it survive
// a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
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
		st.Records += s.recordCount(n)
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

// PurgeDir removes every segment in dir without needing the key (logout
// from a process that does not hold the spool open).
func PurgeDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".seg") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
