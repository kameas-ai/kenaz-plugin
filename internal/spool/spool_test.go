package spool

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/clock"
)

func newSpool(t *testing.T, opt Options) (*Spool, []byte) {
	t.Helper()
	key, err := GetOrCreateKey(&Memory{}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(t.TempDir(), "spool"), key, opt)
	if err != nil {
		t.Fatal(err)
	}
	return s, key
}

func TestRoundTripOrder(t *testing.T) {
	s, _ := newSpool(t, Options{SegmentBytes: 256})
	for i := 0; i < 20; i++ {
		if err := s.Append("telemetry", []byte(fmt.Sprintf("rec-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if st := s.Stats(); st.Segments < 2 || st.Records != 20 {
		t.Fatalf("stats = %+v", st)
	}
	var got []string
	n, err := s.Drain(func(r Record) error {
		if r.Lane != "telemetry" {
			t.Fatalf("lane %q", r.Lane)
		}
		got = append(got, string(r.Payload))
		return nil
	})
	if err != nil || n != 20 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	for i, g := range got {
		if g != fmt.Sprintf("rec-%02d", i) {
			t.Fatalf("order broken at %d: %s", i, g)
		}
	}
	if !s.Empty() {
		t.Fatal("not empty after drain")
	}
}

func TestDrainStopsAndKeepsOnError(t *testing.T) {
	s, _ := newSpool(t, Options{})
	s.Append("l", []byte("a"))
	s.Append("l", []byte("b"))
	boom := errors.New("upload failed")
	_, err := s.Drain(func(r Record) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	n, err := s.Drain(func(Record) error { return nil })
	if err != nil || n != 2 {
		t.Fatalf("redelivery n=%d err=%v", n, err)
	}
}

// Segments are unreadable without the key.
func TestEncryptedAtRest(t *testing.T) {
	s, _ := newSpool(t, Options{})
	secret := []byte("PROJECTED-BATCH-CANARY")
	s.Append("telemetry", secret)
	s.Close()
	names, _ := s.segments()
	raw, _ := os.ReadFile(filepath.Join(s.Dir(), names[0]))
	if bytes.Contains(raw, secret) || bytes.Contains(raw, []byte("telemetry")) {
		t.Fatal("plaintext visible on disk")
	}
	other := make([]byte, 32)
	wrong, _ := Open(s.Dir(), other, Options{})
	n, _ := wrong.Drain(func(Record) error { t.Fatal("decrypted with wrong key"); return nil })
	if n != 0 || wrong.Stats().Quarantined != 1 {
		t.Fatalf("wrong key: n=%d stats=%+v", n, wrong.Stats())
	}
}

// Writes beyond MaxBytes evict the oldest segment, counted.
func TestSizeBound(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	s, _ := newSpool(t, Options{MaxBytes: 4096, SegmentBytes: 1024, Clock: clk})
	payload := bytes.Repeat([]byte{'x'}, 200)
	for i := 0; i < 60; i++ {
		clk.Advance(time.Millisecond)
		if err := s.Append("l", payload); err != nil {
			t.Fatal(err)
		}
	}
	st := s.Stats()
	if st.Bytes > 4096 || st.DroppedSize == 0 || st.DroppedTotal == 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestAgeBound(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	s, _ := newSpool(t, Options{Clock: clk, SegmentBytes: 64})
	s.Append("l", []byte("old"))
	clk.Advance(8 * 24 * time.Hour)
	s.Append("l", []byte("new"))
	if err := s.Prune(); err != nil {
		t.Fatal(err)
	}
	var got []string
	s.Drain(func(r Record) error { got = append(got, string(r.Payload)); return nil })
	if len(got) != 1 || got[0] != "new" || s.Stats().DroppedAge != 1 {
		t.Fatalf("got=%v stats=%+v", got, s.Stats())
	}
}

// Purge leaves spool/ empty within 1 s.
func TestPurge(t *testing.T) {
	s, _ := newSpool(t, Options{SegmentBytes: 128})
	for i := 0; i < 50; i++ {
		s.Append("l", bytes.Repeat([]byte{'y'}, 64))
	}
	start := time.Now()
	if err := s.Purge(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("purge too slow")
	}
	if !s.Empty() {
		t.Fatal("segments remain")
	}
	s.Append("l", []byte("z"))
	if err := PurgeDir(s.Dir()); err != nil || !s.Empty() {
		t.Fatalf("PurgeDir: %v", err)
	}
	if err := PurgeDir(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestKeyStoreMemory(t *testing.T) {
	ks := &Memory{}
	if _, err := ks.Get("dev"); !errors.Is(err, ErrNoKey) {
		t.Fatal(err)
	}
	a, _ := GetOrCreateKey(ks, "dev")
	b, _ := GetOrCreateKey(ks, "dev")
	if !bytes.Equal(a, b) || len(a) != 32 {
		t.Fatal("key not stable")
	}
	ks.Delete("dev")
	if _, err := ks.Get("dev"); !errors.Is(err, ErrNoKey) {
		t.Fatal("delete failed")
	}
	if _, err := Open(t.TempDir(), []byte("short"), Options{}); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestCountsSurviveReopen(t *testing.T) {
	s, key := newSpool(t, Options{})
	for i := 0; i < 3; i++ {
		s.Append("l", []byte("r"))
	}
	s.Close()
	re, _ := Open(s.Dir(), key, Options{})
	if got := re.Stats().Records; got != 3 {
		t.Fatalf("records after reopen = %d", got)
	}
}
