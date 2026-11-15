package spool

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// A cursor records how many leading records of a segment were already
// delivered, so a later drain (in this process or the next) resumes after
// them instead of replaying the whole segment. It sits next to its
// segment as <segment>.cur:
//
//	cursor := "KSC1" || u32be(delivered) || nonce(24) of the segment's first record
//
// The nonce binds the cursor to that segment's content: a cursor left
// behind by a crash is never applied to a later segment that reuses the
// name. A missing or invalid cursor means "nothing delivered" (segments
// written by builds without cursors drain from the start). A cursor holds
// no payload: a count and a copy of a nonce that is already in the clear
// in the segment.
const (
	cursorExt    = ".cur"
	cursorTmpExt = ".cur.tmp"
	cursorMagic  = "KSC1"
	// segTmpExt is a segment AppendAll is still writing (or one a crash
	// left half-written): never read, removed by purge and the next drain.
	segTmpExt = ".seg.tmp"
)

// isSidecar reports whether name is a cursor or a cursor's temp file.
func isSidecar(name string) bool {
	return strings.HasSuffix(name, cursorExt) || strings.HasSuffix(name, cursorTmpExt)
}

// firstNonce returns the nonce of the first record in a segment.
func firstNonce(path string, size int) ([]byte, error) {
	f, err := openTracked(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, 4+size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf[4:], nil
}

// progressLocked returns how many leading records of segment name are
// already delivered. Caller holds mu.
func (s *Spool) progressLocked(name string) int {
	if n, ok := s.done[name]; ok {
		return n
	}
	p := filepath.Join(s.dir, name)
	b, err := os.ReadFile(p + cursorExt)
	ns := s.aead.NonceSize()
	if err != nil || len(b) != len(cursorMagic)+4+ns || string(b[:len(cursorMagic)]) != cursorMagic {
		return 0
	}
	nonce, err := firstNonce(p, ns)
	if err != nil || !bytes.Equal(nonce, b[len(cursorMagic)+4:]) {
		return 0 // a cursor for another segment that had this name
	}
	return int(binary.BigEndian.Uint32(b[len(cursorMagic):]))
}

// advanceLocked records that the first n records of segment name are
// delivered. The in-memory count always advances, so this process never
// replays them even when the write fails; the cursor file makes it
// survive a restart. It reports false when the segment is gone (purged or
// evicted meanwhile): nothing is written then. Caller holds mu.
func (s *Spool) advanceLocked(name string, n int, nonce []byte) (bool, error) {
	p := filepath.Join(s.dir, name)
	if _, err := os.Stat(p); err != nil {
		return false, nil
	}
	s.done[name] = n
	b := make([]byte, 0, len(cursorMagic)+4+len(nonce))
	b = append(b, cursorMagic...)
	b = binary.BigEndian.AppendUint32(b, uint32(n))
	b = append(b, nonce...)
	// Write, fsync and rename, then fsync the directory, so the cursor is
	// either the old one or the new one after a crash, never a torn one.
	tmp := p + cursorTmpExt
	f, err := openTracked(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return true, err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return true, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return true, err
	}
	if err := f.Close(); err != nil {
		return true, err
	}
	if err := renameFile(tmp, p+cursorExt); err != nil {
		return true, err
	}
	return true, syncDir(s.dir)
}

// removeSegmentLocked deletes a segment and then its cursor. The segment
// goes first: a crash in between leaves an orphaned cursor (swept later,
// and bound to a segment that no longer exists), never a segment whose
// progress was forgotten. Caller holds mu.
func (s *Spool) removeSegmentLocked(name string) error {
	p := filepath.Join(s.dir, name)
	err := removeFile(p)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil {
		s.dropCursorLocked(name)
	}
	delete(s.counts, name)
	return err
}

// dropCursorLocked forgets segment name's progress. Caller holds mu.
func (s *Spool) dropCursorLocked(name string) {
	p := filepath.Join(s.dir, name)
	removeFile(p + cursorExt)
	removeFile(p + cursorTmpExt)
	delete(s.done, name)
}

// sweepCursorsLocked removes cursors whose segment is gone and segments a
// crash left half-written. Caller holds mu.
func (s *Spool) sweepCursorsLocked() {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, segTmpExt) {
			removeFile(filepath.Join(s.dir, n))
			continue
		}
		if !isSidecar(n) {
			continue
		}
		seg := strings.TrimSuffix(strings.TrimSuffix(n, cursorTmpExt), cursorExt)
		if _, err := os.Stat(filepath.Join(s.dir, seg)); errors.Is(err, os.ErrNotExist) {
			removeFile(filepath.Join(s.dir, n))
		}
	}
}

// syncDir fsyncs a directory so entry removals and renames in it survive
// a crash. Windows cannot fsync a directory (Access is denied), and its
// rename is durable without it.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
