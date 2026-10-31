package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
)

// errBackoff stands in for a provider's periodic-reload back-off error
// (the private client's adapter.ErrReloadBackoff), which wraps the store
// error that started it.
var errBackoff = errors.New("session reload backing off after a store read error")

// reloadNowFake implements both reloads: the periodic ReloadSession
// (counted in reloads, failing with a back-off error while backoff is set)
// and the explicit ReloadSessionNow (counted in nows, failing while nowFail
// is set), which ignores the back-off.
type reloadNowFake struct {
	reloadFake
	backoff atomic.Bool
	nows    atomic.Int32
	nowFail atomic.Bool
}

func (p *reloadNowFake) Auth() fleetapi.Auth {
	return reloadNowAuth{reloadAuth{fakeAuth{p.fakeProvider}, &p.reloadFake}, p}
}

type reloadNowAuth struct {
	reloadAuth
	n *reloadNowFake
}

func (a reloadNowAuth) ReloadSession(ctx context.Context) error {
	if a.n.backoff.Load() {
		a.n.reloads.Add(1)
		return fmt.Errorf("%w: %w", errBackoff, errors.New("keychain locked"))
	}
	return a.reloadAuth.ReloadSession(ctx)
}

func (a reloadNowAuth) ReloadSessionNow(context.Context) error {
	a.n.nows.Add(1)
	if a.n.nowFail.Load() {
		return errors.New("credential store unavailable")
	}
	a.n.signedIn.Store(a.n.stored.Load())
	return nil
}

// syncBuffer is a bytes.Buffer safe for the daemon's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// count reports how many log lines have level and contain msg.
func (s *syncBuffer) count(level, msg string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, line := range strings.Split(s.b.String(), "\n") {
		if strings.Contains(line, "level="+level) && strings.Contains(line, `msg="`+msg+`"`) {
			n++
		}
	}
	return n
}

func debugLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// With an Auth that has both reloads, the session-changed notice uses the
// explicit ReloadSessionNow (which ignores the periodic back-off) and the
// watch keeps using the periodic ReloadSession. A failed explicit reload
// answers reloaded=false.
func TestSessionChangedUsesReloadNow(t *testing.T) {
	p := &reloadNowFake{reloadFake: reloadFake{fakeProvider: newFake(false)}}
	p.backoff.Store(true) // the periodic reload is backing off throughout
	buf := &syncBuffer{}
	opt := testOpts(t, p, t.TempDir())
	opt.Logger = debugLogger(buf)
	opt.SessionCheckPeriod = 5 * time.Millisecond
	startDaemon(t, opt)
	c, err := Connect(context.Background(), opt.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.reloads.Load() >= 3 })
	p.stored.Store(true)
	if ok, err := c.SessionChanged(context.Background()); err != nil || !ok {
		t.Fatalf("SessionChanged = %v, %v", ok, err)
	}
	if n := p.nows.Load(); n != 1 {
		t.Fatalf("ReloadSessionNow calls = %d, want 1", n)
	}
	waitFor(t, func() bool {
		st, _ := c.Status(context.Background())
		return st.Session == SessionSignedIn && st.Enroll == string(enroll.StateEnrolled)
	})
	// The watch carries on with the periodic reload only.
	r := p.reloads.Load()
	waitFor(t, func() bool { return p.reloads.Load() >= r+3 })
	if n := p.nows.Load(); n != 1 {
		t.Fatalf("the watch called ReloadSessionNow (%d calls)", n)
	}
	if n := buf.count("WARN", "session reload failed"); n != 1 {
		t.Fatalf("back-off warnings = %d, want 1", n)
	}
	if buf.count("DEBUG", "session reload still failing") == 0 {
		t.Fatal("repeated back-off errors not logged at debug")
	}

	p.nowFail.Store(true)
	if ok, err := c.SessionChanged(context.Background()); err != nil || ok {
		t.Fatalf("failed explicit reload: SessionChanged = %v, %v; want false", ok, err)
	}
}

// An Auth with only ReloadSession: the session-changed notice falls back
// to it.
func TestSessionChangedFallsBackToReloadSession(t *testing.T) {
	p := &reloadFake{fakeProvider: newFake(false)}
	opt := testOpts(t, p, t.TempDir())
	opt.SessionCheckPeriod = time.Hour
	startDaemon(t, opt)
	c, err := Connect(context.Background(), opt.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	p.stored.Store(true)
	if ok, err := c.SessionChanged(context.Background()); err != nil || !ok {
		t.Fatalf("SessionChanged = %v, %v", ok, err)
	}
	if n := p.reloads.Load(); n != 1 {
		t.Fatalf("ReloadSession calls = %d, want 1", n)
	}
	waitFor(t, func() bool {
		st, _ := c.Status(context.Background())
		return st.Session == SessionSignedIn
	})
}

// A back-off (or any) error from the periodic reload does not stop the
// watch: once the store reads again, the watch picks up the change on its
// own, and the repeats were logged at debug, not every check at warn.
func TestSessionWatchSurvivesBackoff(t *testing.T) {
	p := &reloadNowFake{reloadFake: reloadFake{fakeProvider: newFake(false)}}
	p.backoff.Store(true)
	buf := &syncBuffer{}
	opt := testOpts(t, p, t.TempDir())
	opt.Logger = debugLogger(buf)
	opt.SessionCheckPeriod = 5 * time.Millisecond
	startDaemon(t, opt)
	c, err := Connect(context.Background(), opt.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.reloads.Load() >= 5 })
	p.stored.Store(true)
	p.backoff.Store(false)
	waitFor(t, func() bool {
		st, _ := c.Status(context.Background())
		return st.Session == SessionSignedIn && st.Enroll == string(enroll.StateEnrolled)
	})
	if n := p.nows.Load(); n != 0 {
		t.Fatalf("the watch called ReloadSessionNow (%d calls)", n)
	}
	if n := buf.count("WARN", "session reload failed"); n != 1 {
		t.Fatalf("back-off warnings = %d, want 1", n)
	}
	if buf.count("DEBUG", "session reload still failing") < 4 {
		t.Fatal("repeated back-off errors not logged at debug")
	}
	waitFor(t, func() bool { return buf.count("INFO", "session reload recovered") == 1 })
}
