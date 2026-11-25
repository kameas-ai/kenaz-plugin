// Package enroll builds the public EnrollBody and runs enroll plus the 6 h
// heartbeat through fleetapi.Enroller.
//
// The body is built entirely here, in public code. The device
// display name replaces the OS hostname by default.
package enroll

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/device"
)

const nodeIDFile = "node_id"

// crockford is the ULID alphabet.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a ULID string (48-bit ms timestamp + 80 random bits).
func NewULID(now time.Time) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", err
	}
	// 128 bits → 26 base32 chars, most significant first.
	var out [26]byte
	var acc uint64
	bits := 0
	idx := 0
	// Prepend 2 zero bits so 130 bits divide into 26 groups of 5.
	acc, bits = 0, 2
	for _, v := range b {
		acc = acc<<8 | uint64(v)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = crockford[(acc>>uint(bits))&31]
			idx++
		}
	}
	return string(out[:]), nil
}

// LoadOrCreateNodeID returns the per-env node id, creating
// stateDir/node_id (0600) on first use. One node per OS user, machine and
// env.
//
// A stored id is kept only if Fleet would accept it (fleetapi.ValidNodeID).
// Every release has written a ULID here, so anything else got there some other
// way: a damaged file, or one written by hand. Fleet refuses such an id for
// good (400), so the device could never enroll with it; it is replaced with a
// new one, and one log line says so, giving the old value's length and nothing
// of what it held. The shape isn't narrowed to a ULID: an id Fleet accepts is a
// device Fleet may already know, and replacing it would register this device a
// second time for no gain.
//
// A new id is a new device to Fleet: the next enroll registers it, and a
// registration made under the old id, if Fleet ever took one, is left behind
// for an administrator to remove.
//
// The id returned is always the id the file holds. Making or replacing the
// file happens under a lock (stateDir/node_id.lock) and after a second look at
// the file, so that of any number of callers that find it missing or invalid
// at once, in this process or others, one writes and the rest return what it
// wrote. An id that couldn't be stored is never returned: a process would
// enroll and tag its telemetry under it until it restarted, and leave a
// device behind in Fleet.
func LoadOrCreateNodeID(stateDir string, now time.Time) (string, error) {
	p := filepath.Join(stateDir, nodeIDFile)
	// Without the lock, only a finished file is taken: one that ends with the
	// newline every writer puts after the id (every release wrote "<id>\n"). An
	// id can't hold a newline, so no part of a file being written in place ends
	// with one, and a reader can't take the first half of an id for an id.
	// Anything else is read again under the lock, where nothing is mid-write:
	// a file with no newline (written by hand, say) is then taken as it is.
	id, ok, whole, err := storedNodeID(p)
	if err != nil || (ok && whole) {
		return id, err
	}
	release, lerr := lockNodeID(p + ".lock")
	if lerr != nil {
		// No lock to be had (a state directory that can't be written): a valid
		// id that is already there is still the device's id.
		if ok && !errors.Is(lerr, errNodeIDLockBusy) {
			return id, nil
		}
		// The lock was held for the whole wait. A write takes microseconds, so
		// whoever holds it isn't writing: if the file has held one valid id
		// across two reads a moment apart, that is the device's id, and it is
		// better returned than refused.
		if errors.Is(lerr, errNodeIDLockBusy) {
			if stable, ok := stableNodeID(p); ok {
				return stable, nil
			}
		}
		return "", lerr
	}
	defer release()
	// Whoever held the lock before may have written it.
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	existed := err == nil
	if id := strings.TrimSpace(string(b)); existed && fleetapi.ValidNodeID(id) {
		// A valid id with no newline after it (written by hand): the same id is
		// written back once, finished the way every writer finishes, so that from
		// now on it is read without the lock. If that can't be done the id is
		// still the one in the file.
		if !strings.HasSuffix(string(b), "\n") {
			_ = writeNodeID(p, id)
		}
		return id, nil
	}
	id, err = NewULID(now)
	if err != nil {
		return "", err
	}
	if err := writeNodeID(p, id); err != nil {
		return "", err
	}
	if existed {
		logNodeIDReplaced(len(b))
	}
	return id, nil
}

// storedNodeID is the id in the file. ok says it is one Fleet accepts, and
// whole that the file ends with the newline a writer finishes with.
func storedNodeID(p string) (id string, ok, whole bool, err error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	id = strings.TrimSpace(string(b))
	return id, fleetapi.ValidNodeID(id), strings.HasSuffix(string(b), "\n"), nil
}

// errNodeIDLockBusy means the lock was held for the whole wait.
var errNodeIDLockBusy = errors.New("enroll: the node id is being written by another process and didn't become available")

// How long a caller waits for another to finish writing the node id. The
// writer holds the lock for one small write, so reaching the end of this means
// something is wrong, and the caller gets an error and no id.
var (
	nodeIDLockWait = 10 * time.Second
	nodeIDLockPoll = 2 * time.Millisecond
	// nodeIDStableWait is how far apart the two reads are that decide a file is
	// not being written, when the lock can't be had.
	nodeIDStableWait = 100 * time.Millisecond
)

// stableNodeID is the id in the file if two reads, nodeIDStableWait apart, find
// the same valid one.
func stableNodeID(p string) (string, bool) {
	first, ok, _, err := storedNodeID(p)
	if err != nil || !ok {
		return "", false
	}
	time.Sleep(nodeIDStableWait)
	second, ok, _, err := storedNodeID(p)
	return second, err == nil && ok && second == first
}

func lockNodeID(path string) (release func(), err error) {
	for end := time.Now().Add(nodeIDLockWait); ; time.Sleep(nodeIDLockPoll) {
		release, held, err := tryLockFile(path)
		if err != nil {
			return nil, err
		}
		if held {
			return release, nil
		}
		if time.Now().After(end) {
			return nil, errNodeIDLockBusy
		}
	}
}

// renameNodeID is os.Rename. Tests replace it with one that fails the way
// Windows does when another program holds the file open.
var renameNodeID = os.Rename

// writeNodeID stores id at p, under the lock. It writes a temporary file and
// renames it over p, so a reader sees the old content or the new and never
// part of one. Windows refuses to rename over a file another program holds
// open without sharing it; then the id is written into the file in place,
// which is safe here because every writer holds the lock, and a reader takes
// a file without the lock only once it ends with the newline that is written
// last (LoadOrCreateNodeID): caught empty or part-written, it waits for the lock. Either way the id
// is in the file when this returns nil, and isn't used when it returns an
// error.
func writeNodeID(p, id string) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".node_id-*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(id + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err == nil {
		if err = renameNodeID(tmp.Name(), p); err == nil {
			return nil
		}
	}
	_ = os.Remove(tmp.Name())
	// In place: one short write, then read back, so that a write that didn't
	// take, or took in part, is an error and not an id.
	if werr := writeNodeIDInPlace(p, id+"\n"); werr != nil {
		return errors.Join(err, werr)
	}
	if got, ok, whole, rerr := storedNodeID(p); rerr != nil || !ok || !whole || got != id {
		return errors.Join(err, errors.New("enroll: the node id could not be stored"))
	}
	return nil
}

// writeNodeIDInPlace writes content over the file at p. Tests replace it with
// one that writes less than it was given, as a full disk does.
var writeNodeIDInPlace = func(p, content string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// logNodeIDReplaced writes the one log line for a replaced node id. Tests
// replace it.
var logNodeIDReplaced = func(oldLen int) {
	slog.Warn("stored node id was not one Kameas Fleet accepts and was replaced; this device will register as a new one", "old_length", oldLen)
}

// Host describes the machine. Tests replace it; Hostname is consulted only
// when the member opted in to sending it (device.Settings.UseComputerName).
type Host struct {
	GOOS     string
	GOARCH   string
	Hostname func() (string, error)
}

// LocalHost returns the running machine's Host.
func LocalHost() Host {
	return Host{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Hostname: os.Hostname}
}

// BuildBody assembles the complete enroll/heartbeat body. It is the only
// constructor of fleetapi.EnrollBody in the plugin.
func BuildBody(stateDir, nodeID, version string, h Host) (fleetapi.EnrollBody, error) {
	s, err := device.Load(stateDir)
	if err != nil {
		return fleetapi.EnrollBody{}, fmt.Errorf("device settings: %w", err)
	}
	arch := h.GOARCH
	if arch != "amd64" && arch != "arm64" {
		arch = ""
	}
	b := fleetapi.EnrollBody{
		NodeID:     nodeID,
		Platform:   h.GOOS,
		Version:    version,
		Hostname:   device.Resolve(s, h.GOOS, nodeID, h.Hostname),
		Arch:       arch,
		ClientKind: fleetapi.ClientKindRelay,
	}
	return b, b.Validate()
}
