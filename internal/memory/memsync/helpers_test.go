package memsync

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

const consent = "memory-sync/v1"

var (
	alice = memory.Profile{Env: "dev", OrgID: "org-1", UserID: "alice"}
	bob   = memory.Profile{Env: "dev", OrgID: "org-1", UserID: "bob"}
)

// device is one machine with one account profile store and a worker.
type device struct {
	t     *testing.T
	dir   string
	p     memory.Profile
	fleet *fleetapitest.MemoryFleet
	st    *memory.Store
	w     *Worker
	logs  *bytes.Buffer

	mu      sync.Mutex
	session memory.Profile // what Session() reports; zero = signed out
	holder  string
}

func newDevice(t *testing.T, f *fleetapitest.MemoryFleet, p memory.Profile) *device {
	t.Helper()
	d := &device{t: t, dir: t.TempDir(), p: p, fleet: f, session: p, logs: &bytes.Buffer{}, holder: "w-" + filepath.Base(t.TempDir())}
	d.open()
	t.Cleanup(func() { d.st.Close() })
	return d
}

func (d *device) path() string { return memory.ProfilePath(d.dir, d.p) }

// open (re)opens the store and builds a fresh worker: a restart.
func (d *device) open() {
	d.t.Helper()
	if d.st != nil {
		d.st.Close()
	}
	st, err := memory.OpenProfile(d.path(), d.p)
	if err != nil {
		d.t.Fatal(err)
	}
	d.st = st
	d.holder += "'"
	d.w = New(Options{
		Store: st, Memory: d.fleet.SessionClient(d.sessionFn), Session: d.sessionFn, ConsentVersion: consent,
		Holder: d.holder, Log: slog.New(slog.NewTextHandler(d.logs, nil)), PullLimit: 2,
	})
}

// restart reopens after a crash and lets the dead worker's lease expire.
func (d *device) restart() {
	d.open()
	d.st.SetClock(func() time.Time { return time.Now().Add(leaseTTL + time.Second) })
}

func (d *device) sessionFn(context.Context) (fleetapi.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == (memory.Profile{}) {
		return fleetapi.Session{}, fleetapi.ErrNotSignedIn
	}
	return fleetapi.Session{Env: fleetapi.Env(d.session.Env), OrgID: d.session.OrgID, UserID: d.session.UserID}, nil
}

func (d *device) signIn(p memory.Profile) {
	d.mu.Lock()
	d.session = p
	d.mu.Unlock()
}

// enable turns sync on the way the approved executor does.
func (d *device) enable() {
	d.t.Helper()
	r := Remote{Memory: d.fleet.SessionClient(d.sessionFn), Session: d.sessionFn}
	if err := r.EnableSync(context.Background(), d.p, consent); err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.st.EnableSync(context.Background(), consent, memory.MutateOptions{SyncApproved: true}); err != nil {
		d.t.Fatal(err)
	}
}

func (d *device) sync() {
	d.t.Helper()
	if err := d.w.RunOnce(context.Background()); err != nil {
		d.t.Fatalf("sync: %v", err)
	}
}

func (d *device) save(title, body string, global bool) string {
	d.t.Helper()
	key := "proj0123456789ab"
	if global {
		key = memory.GlobalProject
	}
	r, err := d.st.SaveNote(context.Background(), memory.NewNote{ProjectKey: key, Title: title, Body: body, Source: memory.SourceCLI},
		memory.SaveOptions{SyncApproved: true})
	if err != nil {
		d.t.Fatal(err)
	}
	return r.ID
}

func (d *device) get(id string) (memory.Note, bool) {
	d.t.Helper()
	f, _, err := d.st.Get(context.Background(), []string{id})
	if err != nil {
		d.t.Fatal(err)
	}
	if len(f) == 0 {
		return memory.Note{}, false
	}
	return f[0], true
}

// bodies returns every visible note body, keyed by body.
func (d *device) bodies() map[string]memory.Note {
	d.t.Helper()
	es, err := d.st.Recent(context.Background(), memory.AllProjects, memory.MaxLimit)
	if err != nil {
		d.t.Fatal(err)
	}
	out := map[string]memory.Note{}
	for _, e := range es {
		n, _ := d.get(e.ID)
		out[n.Body] = n
	}
	return out
}

func (d *device) status() memory.SyncStatus {
	d.t.Helper()
	s, err := d.st.Status(context.Background())
	if err != nil {
		d.t.Fatal(err)
	}
	return s
}

// failingOnce returns a Fault that fails call once in the given phase.
func failingOnce(call string, after bool, err error) func(string, bool) error {
	var mu sync.Mutex
	done := false
	return func(c string, a bool) error {
		mu.Lock()
		defer mu.Unlock()
		if !done && c == call && a == after {
			done = true
			return err
		}
		return nil
	}
}

var errNet = errors.New("connection reset")

func ptr[T any](v T) *T { return &v }
