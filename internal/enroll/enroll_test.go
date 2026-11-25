package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/clock"
	"github.com/kameas-ai/kenaz-plugin/internal/device"
)

const canary = "CANARYHOST9f3e"

type fakeEnroller struct {
	mu     sync.Mutex
	bodies [][]byte
	errs   []error // consumed in order; nil = success
}

func (f *fakeEnroller) Enroll(_ context.Context, b fleetapi.EnrollBody) (fleetapi.EnrollResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(b)
	f.bodies = append(f.bodies, raw)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return fleetapi.EnrollResult{}, err
		}
	}
	return fleetapi.EnrollResult{NodeID: b.NodeID, OrgID: "org", Tier: "team"}, nil
}

func (f *fakeEnroller) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.bodies) }
func (f *fakeEnroller) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.bodies[len(f.bodies)-1])
}

func host() Host {
	return Host{GOOS: "darwin", GOARCH: "arm64", Hostname: func() (string, error) { return canary + ".local", nil }}
}

func TestNodeIDStable(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateNodeID(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(a) {
		t.Fatalf("not a ULID: %q", a)
	}
	b, _ := LoadOrCreateNodeID(dir, time.Now())
	if a != b {
		t.Fatalf("node id changed: %s → %s", a, b)
	}
}

// The canary hostname never leaves unless the member opts in.
func TestDeviceDisplayName(t *testing.T) {
	dir := t.TempDir()
	body := func() (fleetapi.EnrollBody, error) { return BuildBody(dir, "01JNODEID", "1.0.0", host()) }
	f := &fakeEnroller{}
	hb := &Heartbeat{Enroller: f, Body: body}
	ctx := context.Background()

	if _, err := hb.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.last(), canary) || !regexp.MustCompile(`"hostname":"macOS device [0-9a-f]{4}"`).MatchString(f.last()) {
		t.Fatalf("default body leaked or wrong: %s", f.last())
	}
	if !strings.Contains(f.last(), `"client_kind":"relay"`) {
		t.Fatalf("client_kind: %s", f.last())
	}
	device.Rename(dir, "Work laptop")
	hb.Once(ctx)
	if !strings.Contains(f.last(), `"hostname":"Work laptop"`) || strings.Contains(f.last(), canary) {
		t.Fatalf("renamed body: %s", f.last())
	}
	device.UseComputerName(dir)
	hb.Once(ctx)
	if !strings.Contains(f.last(), canary) {
		t.Fatalf("opt-in body should carry hostname: %s", f.last())
	}
}

// The body's keys are exactly the allowlisted ones.
func TestBodyKeysAllowlisted(t *testing.T) {
	b, err := BuildBody(t.TempDir(), "01JNODEID", "1.0.0", Host{GOOS: "linux", GOARCH: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	var m map[string]any
	json.Unmarshal(raw, &m)
	allowed := map[string]bool{}
	for _, k := range []string{"node_id", "platform", "version", "hostname", "arch", "os_version", "in_vm", "vm_id", "client_kind"} {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
}

func TestBuildBodyRejectsUnknownPlatform(t *testing.T) {
	if _, err := BuildBody(t.TempDir(), "n", "1", Host{GOOS: "plan9"}); err == nil {
		t.Fatal("want validation error")
	}
}

// Enroll at start and every 6 h ±10%; node_removed stops the loop.
func TestHeartbeatSchedule(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	f := &fakeEnroller{errs: []error{nil, nil, &fleetapi.APIError{Status: 410, Code: fleetapi.CodeNodeRemoved}}}
	var states []State
	var smu sync.Mutex
	hb := &Heartbeat{
		Enroller: f,
		Body:     func() (fleetapi.EnrollBody, error) { return BuildBody(dir, "n1", "1", host()) },
		Clock:    clk,
		Rand:     func() float64 { return 1 }, // max jitter: +10%
		OnState:  func(s State, _ fleetapi.EnrollResult) { smu.Lock(); states = append(states, s); smu.Unlock() },
	}
	done := make(chan error, 1)
	go func() { done <- hb.Run(context.Background()) }()

	waitFor(t, func() bool { return f.calls() == 1 && clk.Waiters() == 1 })
	clk.Advance(6*time.Hour + 30*time.Minute) // < 6.6h: no beat
	time.Sleep(10 * time.Millisecond)
	if f.calls() != 1 {
		t.Fatalf("beat too early: %d calls", f.calls())
	}
	clk.Advance(7 * time.Minute) // past 6.6h
	waitFor(t, func() bool { return f.calls() == 2 && clk.Waiters() == 1 })
	clk.Advance(7 * time.Hour)
	select {
	case err := <-done:
		if !isRemoved(err) {
			t.Fatalf("Run err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on node_removed")
	}
	if st, _ := hb.State(); st != StateRemoved {
		t.Fatalf("state = %s", st)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(states) != 2 || states[0] != StateEnrolled || states[1] != StateRemoved {
		t.Fatalf("transitions = %v", states)
	}
}

func TestHeartbeatJitterBounds(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.999} {
		hb := &Heartbeat{Rand: func() float64 { return r }}
		d := hb.next()
		if d < 324*time.Minute || d > 396*time.Minute {
			t.Fatalf("r=%v delay %v outside 6h±10%%", r, d)
		}
	}
}

func TestHeartbeatStopsOnUnofficialBuild(t *testing.T) {
	f := &fakeEnroller{errs: []error{fleetapi.ErrUnofficialBuild}}
	hb := &Heartbeat{Enroller: f, Body: func() (fleetapi.EnrollBody, error) { return BuildBody(t.TempDir(), "n", "1", host()) }, Clock: clock.NewFake(time.Now())}
	if err := hb.Run(context.Background()); !errors.Is(err, fleetapi.ErrUnofficialBuild) {
		t.Fatalf("err = %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHeartbeatKickAndSignedOutRetry(t *testing.T) {
	clk := clock.NewFake(time.Now())
	kick := make(chan struct{}, 1)
	f := &fakeEnroller{errs: []error{fleetapi.ErrNotSignedIn, nil}}
	hb := &Heartbeat{Enroller: f, Kick: kick, Clock: clk,
		Body: func() (fleetapi.EnrollBody, error) { return BuildBody(t.TempDir(), "n", "1", host()) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.Run(ctx)
	waitFor(t, func() bool { return f.calls() == 1 && clk.Waiters() == 1 })
	if st, _ := hb.State(); st != StateSignedOut {
		t.Fatalf("state = %s", st)
	}
	kick <- struct{}{} // login happened
	waitFor(t, func() bool { st, _ := hb.State(); return st == StateEnrolled })
}

// Plan required: an org whose plan does not include the plugin is a
// distinct state, the loop keeps going at the normal interval (not the
// 5 min error retry), and admission is picked up on the next beat. Older
// servers' refusal code maps to the same state.
func TestHeartbeatPlanRequiredKeepsBeating(t *testing.T) {
	for _, code := range []string{fleetapi.CodeCapabilityNotInTier, fleetapi.CodeLegacyEarlyAccessRequired} {
		t.Run(code, func(t *testing.T) { testHeartbeatPlanRequired(t, code) })
	}
}

func testHeartbeatPlanRequired(t *testing.T, code string) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
	ea := &fleetapi.APIError{Status: 403, Code: code, Details: map[string]string{"capability": fleetapi.PluginCapability}}
	f := &fakeEnroller{errs: []error{ea, nil}}
	hb := &Heartbeat{
		Enroller: f,
		Body:     func() (fleetapi.EnrollBody, error) { return BuildBody(dir, "n1", "1", host()) },
		Clock:    clk,
		Rand:     func() float64 { return 0.5 }, // no jitter: exactly 6 h
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hb.Run(ctx) }()

	waitFor(t, func() bool { return f.calls() == 1 && clk.Waiters() == 1 })
	if st, _ := hb.State(); st != StatePlanRequired {
		t.Fatalf("state = %s, want %s", st, StatePlanRequired)
	}
	clk.Advance(retryDelay + time.Minute) // the error retry would fire here
	time.Sleep(10 * time.Millisecond)
	if f.calls() != 1 {
		t.Fatalf("plan required must not use the 5 min error retry: %d calls", f.calls())
	}
	clk.Advance(6 * time.Hour)
	waitFor(t, func() bool { st, _ := hb.State(); return f.calls() == 2 && st == StateEnrolled })
}

// A stored node id Fleet would accept is kept, whatever its shape; one it would
// refuse for good is replaced by a new ULID, with one log line that gives the old
// value's length and nothing of what it held.
func TestStoredNodeIDIsValidated(t *testing.T) {
	ulid := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	var logged []int
	old := logNodeIDReplaced
	logNodeIDReplaced = func(n int) { logged = append(logged, n) }
	t.Cleanup(func() { logNodeIDReplaced = old })

	for name, stored := range map[string]string{
		"a ULID, as every release wrote it": "01K7ZQ5V3W4X5Y6Z7A8B9C0D1E\n",
		"no newline":                        "01K7ZQ5V3W4X5Y6Z7A8B9C0D1E",
		"CRLF and padding":                  "  01K7ZQ5V3W4X5Y6Z7A8B9C0D1E\r\n",
		"not a ULID, and one Fleet accepts": "device-7f3a_prod.example:1\n",
		"128 characters":                    strings.Repeat("x", 128) + "\n",
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, nodeIDFile)
		if err := os.WriteFile(p, []byte(stored), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadOrCreateNodeID(dir, time.Now())
		if err != nil || got != strings.TrimSpace(stored) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
		// Left as it is when it is finished with a newline; otherwise the same id
		// is written back once with one.
		want := stored
		if !strings.HasSuffix(stored, "\n") {
			want = strings.TrimSpace(stored) + "\n"
		}
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s: the file holds %q, want %q", name, b, want)
		}
	}
	if len(logged) != 0 {
		t.Fatalf("a valid id was logged as replaced: %v", logged)
	}

	for name, stored := range map[string]string{
		"empty":             "",
		"only whitespace":   " \n\t\n",
		"a space inside":    "01K7ZQ5V3W 4X5Y6Z7A8B9C0D1E\n",
		"a tab inside":      "01K7ZQ5V3W\t4X5Y6Z7A8B9C0D1E\n",
		"two lines":         "01K7ZQ5V3W4X5Y6Z7A8B9C0D1E\n01K7ZQ5V3W4X5Y6Z7A8B9C0D1F\n",
		"over-long":         strings.Repeat("A", 129) + "\n",
		"non-ASCII":         "nœud-secret-canary\n",
		"a control char":    "01K7ZQ5V3W\x004X5Y6Z7A8B9C0D1E\n",
		"an escape":         "\x1b[31msecret-canary\n",
		"DEL":               "01K7ZQ5V3W\x7f\n",
		"invalid UTF-8":     "\xff\xfesecret-canary\n",
		"megabyte of noise": strings.Repeat("secret-canary ", 80000),
	} {
		logged = nil
		dir := t.TempDir()
		p := filepath.Join(dir, nodeIDFile)
		if err := os.WriteFile(p, []byte(stored), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := LoadOrCreateNodeID(dir, time.Now())
		if err != nil || !ulid.MatchString(got) || !fleetapi.ValidNodeID(got) {
			t.Fatalf("%s: %q, %v", name, got, err)
		}
		b, _ := os.ReadFile(p)
		if string(b) != got+"\n" {
			t.Errorf("%s: the file holds %q, not the new id", name, b)
		}
		if runtime.GOOS != "windows" {
			if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
				t.Errorf("%s: mode %v", name, fi.Mode().Perm())
			}
		}
		if len(logged) != 1 || logged[0] != len(stored) {
			t.Errorf("%s: logged %v, want one line with length %d", name, logged, len(stored))
		}
		// Stable from then on, with nothing more logged and no temp file left.
		again, _ := LoadOrCreateNodeID(dir, time.Now())
		entries, _ := os.ReadDir(dir)
		if again != got || len(logged) != 1 || len(entries) != 2 { // node_id and its lock
			t.Errorf("%s: %q then %q, %d log lines, %d files", name, got, again, len(logged), len(entries))
		}
		// The body built from it is one Fleet takes.
		if _, err := BuildBody(dir, got, "1.0.0", host()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The log line itself carries the length and never the old value.
func TestNodeIDReplacedLogLine(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, nodeIDFile), []byte("secret canary value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreateNodeID(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "old_length=20") || strings.Contains(out, "canary") || strings.Contains(out, id) {
		t.Fatalf("%q", out)
	}
}

// Any number of callers starting at once, on a fresh directory or on a damaged
// file, all return the one id the file ends up holding: none goes on to enroll
// under an id that was never stored.
func TestNodeIDConcurrentStart(t *testing.T) {
	var replaced atomic.Int64
	old := logNodeIDReplaced
	logNodeIDReplaced = func(int) { replaced.Add(1) }
	t.Cleanup(func() { logNodeIDReplaced = old })
	rounds := 250
	if testing.Short() {
		rounds = 40
	}
	for _, stored := range []string{"missing", "", "bad id here\n", "\x00\x01\n"} {
		for _, callers := range []int{2, 4, 16} {
			for round := 0; round < rounds; round++ {
				dir := t.TempDir()
				p := filepath.Join(dir, nodeIDFile)
				if stored != "missing" {
					if err := os.WriteFile(p, []byte(stored), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				replaced.Store(0)
				got := make([]string, callers)
				errs := make([]error, callers)
				start := make(chan struct{})
				var wg sync.WaitGroup
				for i := 0; i < callers; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						got[i], errs[i] = LoadOrCreateNodeID(dir, time.Now())
					}()
				}
				close(start)
				wg.Wait()
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				inFile := strings.TrimSpace(string(b))
				if !fleetapi.ValidNodeID(inFile) {
					t.Fatalf("%q, %d callers: the file holds %q", stored, callers, b)
				}
				for i := range got {
					if errs[i] != nil || got[i] != inFile {
						t.Fatalf("%q, %d callers, round %d: caller %d returned %q (%v); the file holds %q", stored, callers, round, i, got[i], errs[i], inFile)
					}
				}
				wantLogs := int64(1)
				if stored == "missing" {
					wantLogs = 0
				}
				if n := replaced.Load(); n != wantLogs {
					t.Fatalf("%q, %d callers: %d replacement log lines, want %d", stored, callers, n, wantLogs)
				}
			}
		}
	}
}

// Windows refuses to rename over a file another program holds open. The id is
// then written in place, under the same lock, and callers still agree with the
// file; and when it can't be stored at all, no id is returned.
func TestNodeIDWhenRenameIsRefused(t *testing.T) {
	oldLog, oldRename := logNodeIDReplaced, renameNodeID
	logNodeIDReplaced = func(int) {}
	renameNodeID = func(string, string) error {
		return errors.New("The process cannot access the file because it is being used by another process.")
	}
	t.Cleanup(func() { logNodeIDReplaced, renameNodeID = oldLog, oldRename })
	for _, stored := range []string{"missing", "bad id here\n"} {
		for round := 0; round < 50; round++ {
			dir := t.TempDir()
			p := filepath.Join(dir, nodeIDFile)
			if stored != "missing" {
				if err := os.WriteFile(p, []byte(stored), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := make([]string, 4)
			var wg sync.WaitGroup
			for i := range got {
				wg.Add(1)
				go func() {
					defer wg.Done()
					id, err := LoadOrCreateNodeID(dir, time.Now())
					if err != nil {
						t.Error(err)
					}
					got[i] = id
				}()
			}
			wg.Wait()
			b, _ := os.ReadFile(p)
			for _, id := range got {
				if id != strings.TrimSpace(string(b)) || !fleetapi.ValidNodeID(id) {
					t.Fatalf("%q: returned %q; the file holds %q", stored, id, b)
				}
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 2 {
				t.Fatalf("a temporary file was left: %v", entries)
			}
		}
	}
	if runtime.GOOS == "windows" {
		return // a read-only directory doesn't stop a write there
	}
	// Nothing can be written: an error, and no id to enroll under.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, nodeIDFile), []byte("bad id here\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	release, _, err := tryLockFile(filepath.Join(dir, nodeIDFile+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if os.Geteuid() == 0 {
		return
	}
	if id, err := LoadOrCreateNodeID(dir, time.Now()); err == nil || id != "" {
		t.Fatalf("returned %q with nothing stored (%v)", id, err)
	}
}

// While an id is being written into the file in place, a reader never takes the
// part that is there for an id: it waits for the writer, and returns the whole one.
func TestNodeIDReaderNeverTakesAPartialWrite(t *testing.T) {
	const whole = "01K7ZQ5V3W4X5Y6Z7A8B9C0D1E"
	// Every prefix is an id Fleet would accept, which is what makes this matter.
	for _, part := range []string{whole[:1], whole[:10], whole[:25], whole} {
		dir := t.TempDir()
		p := filepath.Join(dir, nodeIDFile)
		release, held, err := tryLockFile(p + ".lock")
		if err != nil || !held {
			t.Fatal(err)
		}
		// The writer holds the lock and has written this much, with no newline yet.
		if err := os.WriteFile(p, []byte(part), 0o600); err != nil {
			t.Fatal(err)
		}
		got := make(chan string, 1)
		go func() {
			id, err := LoadOrCreateNodeID(dir, time.Now())
			if err != nil {
				t.Error(err)
			}
			got <- id
		}()
		select {
		case id := <-got:
			t.Fatalf("a reader took %q while the id was being written", id)
		case <-time.After(100 * time.Millisecond):
		}
		if err := os.WriteFile(p, []byte(whole+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		release()
		if id := <-got; id != whole {
			t.Fatalf("the reader returned %q after a write of %q", id, part)
		}
	}
	// A finished file is read without the lock, even while someone holds it.
	dir := t.TempDir()
	p := filepath.Join(dir, nodeIDFile)
	if err := os.WriteFile(p, []byte(whole+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, _, _ := tryLockFile(p + ".lock")
	defer release()
	done := make(chan string, 1)
	go func() { id, _ := LoadOrCreateNodeID(dir, time.Now()); done <- id }()
	select {
	case id := <-done:
		if id != whole {
			t.Fatal(id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a finished file waited for the lock")
	}
}

// An in-place write that didn't take, or took in part, is an error: the read back
// catches it, and no id is returned that the file doesn't hold.
func TestNodeIDInPlaceWriteIsReadBack(t *testing.T) {
	oldLog, oldRename, oldWrite := logNodeIDReplaced, renameNodeID, writeNodeIDInPlace
	logNodeIDReplaced = func(int) {}
	renameNodeID = func(string, string) error { return errors.New("sharing violation") }
	t.Cleanup(func() { logNodeIDReplaced, renameNodeID, writeNodeIDInPlace = oldLog, oldRename, oldWrite })
	for name, write := range map[string]func(p, content string) error{
		// A full disk: fewer bytes than asked, and no error from the write.
		"half the id":          func(p, content string) error { return os.WriteFile(p, []byte(content[:13]), 0o600) },
		"the id, no newline":   func(p, content string) error { return os.WriteFile(p, []byte(content[:26]), 0o600) },
		"nothing":              func(p, content string) error { return nil },
		"another id":           func(p, content string) error { return os.WriteFile(p, []byte("01K7ZQ5V3W4X5Y6Z7A8B9C0D1E\n"), 0o600) },
		"an id Fleet refuses":  func(p, content string) error { return os.WriteFile(p, []byte("not an id\n"), 0o600) },
		"an empty file":        func(p, content string) error { return os.WriteFile(p, nil, 0o600) },
		"the file is gone":     func(p, content string) error { return os.Remove(p) },
		"an error":             func(p, content string) error { return errors.New("disk full") },
		"the id and more text": func(p, content string) error { return os.WriteFile(p, []byte(content+"x"), 0o600) },
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, nodeIDFile), []byte("bad id here\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeNodeIDInPlace = write
		if id, err := LoadOrCreateNodeID(dir, time.Now()); err == nil || id != "" {
			t.Errorf("%s: returned %q (%v)", name, id, err)
		}
	}
	// The real writer, with the rename refused: the id is stored and returned.
	writeNodeIDInPlace = oldWrite
	dir := t.TempDir()
	id, err := LoadOrCreateNodeID(dir, time.Now())
	b, _ := os.ReadFile(filepath.Join(dir, nodeIDFile))
	if err != nil || string(b) != id+"\n" {
		t.Fatalf("%q %v, file %q", id, err, b)
	}
}

// An id with no newline after it (written by hand) is still this device's id.
// It is written back once, finished with the newline, so that later calls read
// it without the lock; where no lock can be had at all it is taken as it is.
func TestNodeIDWithNoNewlineIsKept(t *testing.T) {
	var logged atomic.Int64
	old := logNodeIDReplaced
	logNodeIDReplaced = func(int) { logged.Add(1) }
	t.Cleanup(func() { logNodeIDReplaced = old })
	dir := t.TempDir()
	p := filepath.Join(dir, nodeIDFile)
	if err := os.WriteFile(p, []byte("device-7f3a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := LoadOrCreateNodeID(dir, time.Now()); err != nil || id != "device-7f3a" {
		t.Fatalf("%q %v", id, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "device-7f3a\n" {
		t.Fatalf("not finished with a newline: %q", b)
	}
	if logged.Load() != 0 {
		t.Fatal("keeping the id was logged as replacing it")
	}
	// From now on it is read without the lock: with the lock held by someone
	// else, the call returns at once.
	release, held, err := tryLockFile(p + ".lock")
	if err != nil || !held {
		t.Fatal(err)
	}
	start := time.Now()
	id, err := LoadOrCreateNodeID(dir, time.Now())
	release()
	if err != nil || id != "device-7f3a" || time.Since(start) > time.Second {
		t.Fatalf("%q %v after %v: the file is still read under the lock", id, err, time.Since(start))
	}
	// Many callers on a file with no newline agree, and it is rewritten once.
	dir = t.TempDir()
	p = filepath.Join(dir, nodeIDFile)
	if err := os.WriteFile(p, []byte("device-7f3a"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := LoadOrCreateNodeID(dir, time.Now()); err != nil || id != "device-7f3a" {
				t.Errorf("%q %v", id, err)
			}
		}()
	}
	wg.Wait()
	if b, _ := os.ReadFile(p); string(b) != "device-7f3a\n" {
		t.Fatalf("%q", b)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	ro := t.TempDir()
	if err := os.WriteFile(filepath.Join(ro, nodeIDFile), []byte("device-7f3a"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o700) })
	if id, err := LoadOrCreateNodeID(ro, time.Now()); err != nil || id != "device-7f3a" {
		t.Fatalf("a read-only state directory: %q %v", id, err)
	}
}

// The lock held for the whole wait, by something that isn't writing: a valid id
// that stays the same across two reads is returned, not refused. A file that is
// missing, invalid or changing is still an error, and no id.
func TestNodeIDWhenTheLockStaysBusy(t *testing.T) {
	oldWait, oldStable := nodeIDLockWait, nodeIDStableWait
	nodeIDLockWait, nodeIDStableWait = 150*time.Millisecond, 60*time.Millisecond
	t.Cleanup(func() { nodeIDLockWait, nodeIDStableWait = oldWait, oldStable })
	busy := func(t *testing.T, content *string) (dir string) {
		t.Helper()
		dir = t.TempDir()
		p := filepath.Join(dir, nodeIDFile)
		if content != nil {
			if err := os.WriteFile(p, []byte(*content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		release, held, err := tryLockFile(p + ".lock")
		if err != nil || !held {
			t.Fatal(err)
		}
		t.Cleanup(release)
		return dir
	}
	str := func(s string) *string { return &s }
	// No newline, so it can't be taken without the lock; and the lock never comes.
	dir := busy(t, str("device-7f3a"))
	if id, err := LoadOrCreateNodeID(dir, time.Now()); err != nil || id != "device-7f3a" {
		t.Fatalf("a valid id on disk was refused: %q %v", id, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, nodeIDFile)); string(b) != "device-7f3a" {
		t.Fatalf("written without the lock: %q", b)
	}
	for name, content := range map[string]*string{"missing": nil, "empty": str(""), "invalid": str("bad id here\n")} {
		dir := busy(t, content)
		if id, err := LoadOrCreateNodeID(dir, time.Now()); !errors.Is(err, errNodeIDLockBusy) || id != "" {
			t.Errorf("%s: %q %v", name, id, err)
		}
	}
	// Changing between the two reads: someone is writing after all.
	dir = busy(t, str("01K7ZQ5V3W"))
	p := filepath.Join(dir, nodeIDFile)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				_ = os.WriteFile(p, []byte("01K7ZQ5V3W"+strings.Repeat("4", i%100+1)), 0o600)
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	id, err := LoadOrCreateNodeID(dir, time.Now())
	close(stop)
	<-done
	if !errors.Is(err, errNodeIDLockBusy) || id != "" {
		t.Fatalf("a file that was changing was taken: %q %v", id, err)
	}
}
