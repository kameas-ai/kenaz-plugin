package capture

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// Capture is offered only on Kameas Fleet's recent positive answer
// (memory_capture_kinds). Without one, a hook reads, queues and writes nothing.
func TestHookWithoutOfferWritesNothing(t *testing.T) {
	root := t.TempDir()
	p := PathsIn(root)
	now := time.Now()
	started := 0
	deps := HookDeps{Paths: p, Settings: activeSettings(), Host: memory.SourceClaudeCode, Home: testHome,
		Now: func() time.Time { return now }, StartWorker: func() error { started++; return nil }}
	body := map[string]any{"session_id": "s", "cwd": t.TempDir(), "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.go"}}
	for _, ev := range []string{HookPrompt, HookTool, HookStop} {
		if out := Handle(context.Background(), ev, payload(t, body), deps); out != "" {
			t.Errorf("%s: %q", ev, out)
		}
	}
	if QueueLen(p) != 0 || droppedCount(p) != 0 {
		t.Fatalf("queued %d, dropped %d", QueueLen(p), droppedCount(p))
	}
	// The worker is started to ask Fleet, once while a check is due.
	if started != 3 {
		t.Errorf("worker started %d times", started)
	}
	files := 0
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		// The worker's lock is the only file: it keeps to one worker, and holds nothing.
		if err == nil && !info.IsDir() && path != p.lock() {
			t.Errorf("written without an offer: %s", path)
			files++
		}
		return nil
	})
	if files != 0 {
		t.Errorf("%d files written without an offer", files)
	}

	// A stale yes is no yes either, and a check just made isn't repeated by hooks.
	st := LoadState(p)
	st.OfferedAt, st.SwitchesCheckedAt = now.Add(-offerFresh-time.Minute).UnixMilli(), now.UnixMilli()
	if err := saveState(p, st); err != nil {
		t.Fatal(err)
	}
	started = 0
	Handle(context.Background(), HookTool, payload(t, body), deps)
	if QueueLen(p) != 0 || started != 0 {
		t.Errorf("stale answer: queued %d, started %d", QueueLen(p), started)
	}

	// The session agent gets no reminder either.
	agent := deps
	agent.Settings.Writer = WriterSessionAgent
	if out := Handle(context.Background(), HookPrompt, payload(t, body), agent); out != "" {
		t.Errorf("reminder without an offer: %q", out)
	}

	// With the answer, the same hook queues.
	if err := RecordOffered(p, now, "local"); err != nil {
		t.Fatal(err)
	}
	Handle(context.Background(), HookTool, payload(t, body), deps)
	if QueueLen(p) != 1 {
		t.Errorf("offered: queued %d", QueueLen(p))
	}
}

// The worker fails closed: no Offered, an unknown answer, a failed read and a no
// all stop it and clear what waits; a yes lets it run; the key going away
// mid-session stops it at the next check.
func TestWorkerNeedsTheOffer(t *testing.T) {
	cwd := t.TempDir()
	now := time.Now()
	for _, c := range []struct {
		name    string
		offered func(context.Context) (bool, string, string)
		reason  string
	}{
		{"no answer source", nil, PauseUnconfirmed},
		{"unknown", func(context.Context) (bool, string, string) { return false, "", "local" }, PauseUnconfirmed},
		{"read failed", func(context.Context) (bool, string, string) { return false, PauseUnconfirmed, "local" }, PauseUnconfirmed},
		{"absent", func(context.Context) (bool, string, string) { return false, PauseNotOffered, "acct" }, PauseNotOffered},
		{"signed out, other writer", func(context.Context) (bool, string, string) { return false, PauseSignedOutWriter, "local" }, PauseSignedOutWriter},
	} {
		p := testPaths(t)
		m := &fakeModel{reply: `{"observations":[]}`}
		w, st := newWorker(t, p, m, activeSettings())
		w.Now = func() time.Time { return now }
		w.Offered = c.offered
		queueTools(t, p, "a", cwd, batchEvents, now)
		// The answer recorded at queue time is not a check: force one.
		s := LoadState(p)
		s.SwitchesCheckedAt = 0
		_ = saveState(p, s)
		w.Pass(context.Background())
		got := LoadState(p)
		if len(m.calls) != 0 || QueueLen(p) != 0 || got.PauseReason != c.reason || got.Offered(now) {
			t.Errorf("%s: calls %d, queue %d, state %+v", c.name, len(m.calls), QueueLen(p), got)
		}
		if n, _ := st.Count(context.Background()); n != 0 {
			t.Errorf("%s: %d notes", c.name, n)
		}
		if r := BuildReport(p, activeSettings(), now); r.NotOffered != c.reason {
			t.Errorf("%s: report %+v", c.name, r)
		}
	}

	// Offered: it runs. Then the key disappears: the next check stops it.
	p := testPaths(t)
	m := &fakeModel{reply: `{"observations":[]}`}
	w, _ := newWorker(t, p, m, activeSettings())
	w.Now = func() time.Time { return now }
	queueTools(t, p, "a", cwd, batchEvents, now)
	s := LoadState(p)
	s.SwitchesCheckedAt = 0
	_ = saveState(p, s)
	w.Pass(context.Background())
	if len(m.calls) != 1 || !LoadState(p).Offered(now) {
		t.Fatalf("offered: calls %d, state %+v", len(m.calls), LoadState(p))
	}
	later := now.Add(switchCheckEvery + time.Minute)
	w.Now = func() time.Time { return later }
	queueTools(t, p, "b", cwd, batchEvents, later) // hooks still had a current yes
	w.Offered = func(context.Context) (bool, string, string) { return false, PauseNotOffered, "local" }
	w.Pass(context.Background())
	if got := LoadState(p); len(m.calls) != 1 || QueueLen(p) != 0 || got.PauseReason != PauseNotOffered || got.Offered(later) {
		t.Fatalf("key gone: calls %d, queue %d, state %+v", len(m.calls), QueueLen(p), got)
	}
	// And hooks stop queueing at once.
	runHook(t, p, HookTool, map[string]any{"session_id": "c", "cwd": cwd, "tool_name": "Edit"}, later)
	if QueueLen(p) != 0 {
		t.Errorf("a hook queued after the key went: %d", QueueLen(p))
	}
	// A yes at the next check starts it again, with no failure pause in between.
	again := later.Add(switchCheckEvery + time.Minute)
	w.Now = func() time.Time { return again }
	w.Offered = offered
	queueTools(t, p, "d", cwd, batchEvents, again)
	s = LoadState(p)
	s.SwitchesCheckedAt = later.UnixMilli()
	_ = saveState(p, s)
	w.Pass(context.Background())
	if len(m.calls) != 2 {
		t.Errorf("after a yes: calls %d, state %+v", len(m.calls), LoadState(p))
	}
}

// The organization's off switch still wins over the offer.
func TestOrgOffWinsOverTheOffer(t *testing.T) {
	p := testPaths(t)
	m := &fakeModel{reply: `{"observations":[]}`}
	w, _ := newWorker(t, p, m, activeSettings())
	w.OrgAllows = func(context.Context) (bool, bool) { return false, true }
	queueTools(t, p, "a", t.TempDir(), batchEvents, time.Now())
	w.Pass(context.Background())
	if st := LoadState(p); len(m.calls) != 0 || QueueLen(p) != 0 || st.PauseReason != PauseOrgOff {
		t.Errorf("calls %d, queue %d, state %+v", len(m.calls), QueueLen(p), st)
	}
}

// A worker that isn't offered capture exits at once instead of idling.
func TestWorkerExitsWithoutOffer(t *testing.T) {
	p := testPaths(t)
	w, _ := newWorker(t, p, &fakeModel{}, activeSettings())
	w.Offered = func(context.Context) (bool, string, string) { return false, PauseNotOffered, "local" }
	w.Idle = time.Hour
	slept := 0
	w.Sleep = func(context.Context, time.Duration) { slept++ }
	if err := w.Run(context.Background()); err != nil || slept != 0 {
		t.Errorf("err %v, slept %d", err, slept)
	}
}

func TestNotOfferedLine(t *testing.T) {
	r := Report{On: true, Writer: WriterAPIKey, NotOffered: PauseNotOffered}
	if got := r.Line(); got != "memory capture: turned on (api-key), but not running: automatic memory capture isn't available for your organization yet." {
		t.Error(got)
	}
}

// A yes is for one profile. Signing in (or out) since it was given means nothing
// queued is processed, and Fleet is asked again for the profile now active; an
// account without the key then stays off.
func TestOfferIsForOneProfile(t *testing.T) {
	cwd := t.TempDir()
	now := time.Now()
	p := testPaths(t) // a yes for "local"
	m := &fakeModel{reply: `{"observations":[]}`}
	w, st := newWorker(t, p, m, activeSettings())
	w.Now = func() time.Time { return now }
	w.Open = func(context.Context) (*memory.Store, string, error) { return st, "acct", nil } // signed in since
	asked := 0
	w.Offered = func(context.Context) (bool, string, string) { asked++; return false, PauseNotOffered, "acct" }
	queueTools(t, p, "a", cwd, batchEvents, now)
	s := LoadState(p)
	s.SwitchesCheckedAt = now.UnixMilli() // a check just made: not due
	_ = saveState(p, s)
	w.Pass(context.Background())
	if got := LoadState(p); len(m.calls) != 0 || QueueLen(p) != 0 || got.Offered(now) || got.SwitchesCheckedAt != 0 {
		t.Fatalf("calls %d, queue %d, state %+v", len(m.calls), QueueLen(p), got)
	}
	if n, _ := st.Count(context.Background()); n != 0 {
		t.Errorf("%d notes", n)
	}
	// The next pass asks for the account, and it isn't offered: nothing runs.
	queueTools(t, p, "b", cwd, batchEvents, now)
	s = LoadState(p)
	s.OfferedFor = "acct"
	_ = saveState(p, s)
	w.Pass(context.Background())
	if got := LoadState(p); asked != 1 || len(m.calls) != 0 || QueueLen(p) != 0 || got.PauseReason != PauseNotOffered {
		t.Fatalf("asked %d, calls %d, queue %d, state %+v", asked, len(m.calls), QueueLen(p), got)
	}
}

// ResetOffer, at sign-in and sign-out, forgets the yes and clears the queue.
func TestResetOffer(t *testing.T) {
	p := testPaths(t)
	queueTools(t, p, "a", t.TempDir(), 2, time.Now())
	if err := ResetOffer(p); err != nil {
		t.Fatal(err)
	}
	if st := LoadState(p); QueueLen(p) != 0 || st.Offered(time.Now()) || st.OfferedFor != "" {
		t.Errorf("queue %d, state %+v", QueueLen(p), st)
	}
	if err := ResetOffer(PathsIn(t.TempDir())); err != nil {
		t.Errorf("nothing there: %v", err)
	}
}
