package capture

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Queue bounds. With MaxEventBytes they also bound the queue's disk use.
const (
	MaxQueueEvents = 2000
	MaxEventBytes  = 64 << 10
	// QueueTTL is how long a queued event may wait before it is deleted
	// unprocessed.
	QueueTTL = 24 * time.Hour
)

// ErrQueueFull means an event was dropped because the queue is at its bound.
var ErrQueueFull = errors.New("capture queue full")

// Paths is where capture keeps its files.
type Paths struct{ Dir string }

// PathsIn returns the capture paths under the kenaz state root.
func PathsIn(stateRoot string) Paths {
	return Paths{Dir: filepath.Join(stateRoot, "memory", "capture")}
}

func (p Paths) queue() string   { return filepath.Join(p.Dir, "queue") }
func (p Paths) state() string   { return filepath.Join(p.Dir, "state.json") }
func (p Paths) lock() string    { return filepath.Join(p.Dir, "worker.lock") }
func (p Paths) dropped() string { return filepath.Join(p.Dir, "dropped") }
func (p Paths) work() string    { return filepath.Join(p.Dir, "work") }

func (p Paths) ensure() error {
	for _, d := range []string{filepath.Dir(p.Dir), p.Dir, p.queue(), p.work()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Enqueue writes one event, or reports why it could not. One file per event
// keeps a hook fast and safe next to other hooks and the worker: it needs no
// lock, and a crash can leave at most a temp file, which ListQueue ignores
// and the next pass removes.
func Enqueue(p Paths, ev Event) error {
	if err := p.ensure(); err != nil {
		return err
	}
	ents, err := os.ReadDir(p.queue())
	if err != nil {
		return err
	}
	if len(ents) >= MaxQueueEvents {
		noteDropped(p)
		return ErrQueueFull
	}
	ev.V = 1
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if len(b) > MaxEventBytes {
		// Only possible with text that escapes badly. Keep the fact of the
		// call, not its content.
		ev.Input, ev.Output = "", ""
		if b, err = json.Marshal(ev); err != nil {
			return err
		}
	}
	r := make([]byte, 6)
	if _, err := rand.Read(r); err != nil {
		return err
	}
	// The clock in the name keeps events of one millisecond in order.
	name := fmt.Sprintf("%020d-%019d-%s.json", ev.At, time.Now().UnixNano(), hex.EncodeToString(r))
	tmp := filepath.Join(p.queue(), name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(p.queue(), name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// queued is one event file.
type queued struct {
	name string
	ev   Event
}

// listQueue returns the readable events, oldest first. Files it cannot use
// are deleted: nothing unreadable stays on disk.
func listQueue(p Paths, now time.Time) ([]queued, error) {
	ents, err := os.ReadDir(p.queue())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []queued
	for _, e := range ents {
		path := filepath.Join(p.queue(), e.Name())
		if strings.HasSuffix(e.Name(), ".tmp") {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > time.Minute {
				os.Remove(path)
			}
			continue
		}
		b, err := os.ReadFile(path)
		var ev Event
		if err != nil || json.Unmarshal(b, &ev) != nil {
			os.Remove(path)
			continue
		}
		if now.Sub(time.UnixMilli(ev.At)) > QueueTTL {
			os.Remove(path)
			noteDropped(p)
			continue
		}
		out = append(out, queued{name: e.Name(), ev: ev})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func removeQueued(p Paths, items []queued) {
	for _, it := range items {
		os.Remove(filepath.Join(p.queue(), it.name))
	}
}

// QueueLen counts the waiting events without reading them.
func QueueLen(p Paths) int {
	ents, err := os.ReadDir(p.queue())
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".tmp") {
			n++
		}
	}
	return n
}

// PurgeQueue deletes every waiting event. Usage counters stay.
func PurgeQueue(p Paths) error {
	if err := os.RemoveAll(p.queue()); err != nil {
		return err
	}
	return os.RemoveAll(p.work())
}

// noteDropped counts one dropped event, by appending a byte. Appends need no
// lock, so a hook can do this next to the worker.
func noteDropped(p Paths) {
	f, err := os.OpenFile(p.dropped(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write([]byte{'.'})
}

// droppedCount is how many events were dropped so far.
func droppedCount(p Paths) int64 {
	fi, err := os.Stat(p.dropped())
	if err != nil {
		return 0
	}
	return fi.Size()
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
