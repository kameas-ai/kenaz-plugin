package app_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

var errNoKeychain = errors.New("no Secret Service on the session bus")

// fileProvider is a fake official provider whose token store supports the
// file fallback. It keeps a stand-in token file in the directory it is given.
type fileProvider struct {
	*official
	mu   sync.Mutex
	dirs []string
}

func (p *fileProvider) Auth() fleetapi.Auth { return fileAuth{auth{p.official}, p} }

func (p *fileProvider) dir() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.dirs) == 0 {
		return ""
	}
	return p.dirs[len(p.dirs)-1]
}

type fileAuth struct {
	auth
	p *fileProvider
}

func (a fileAuth) Login(ctx context.Context, r fleetapi.LoginRequest) (fleetapi.Session, error) {
	s, err := a.auth.Login(ctx, r)
	if d := a.p.dir(); d != "" {
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "token.json"), []byte(`{"secret":"`+canaryToken+`"}`), 0o600)
	}
	return s, err
}

func (a fileAuth) Logout(ctx context.Context) error {
	if d := a.p.dir(); d != "" {
		os.Remove(filepath.Join(d, "token.json"))
	}
	return a.auth.Logout(ctx)
}

func (a fileAuth) TokenStore() fleetapi.TokenStore { return fileTS{a.p} }

type fileTS struct{ p *fileProvider }

func (fileTS) Clear(context.Context) error { return nil }
func (t fileTS) UseFiles(dir string) error {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	t.p.dirs = append(t.p.dirs, dir)
	return nil
}

// fileHarness runs commands with no usable OS keychain: no injected key
// store, and a probe that fails.
type fileHarness struct{ *harness }

func newFileHarness(t *testing.T, p fleetapi.Provider) *fileHarness {
	return &fileHarness{&harness{home: t.TempDir(), p: p}}
}

func (h *fileHarness) env(ctx context.Context, out, errb *bytes.Buffer) app.Env {
	e := h.harness.env(ctx, out, errb)
	e.KeyStore = nil
	e.KeychainProbe = func() error { return errNoKeychain }
	return e
}

func (h *fileHarness) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := app.RunEnv(h.env(context.Background(), &out, &errb), args)
	return code, out.String(), errb.String()
}

func (h *fileHarness) credDir() string { return filepath.Join(h.home, "plugin", "credentials") }

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// A usable keychain changes nothing: doctor and status say so, and the
// provider is never moved to files.
func TestCredentialStoreKeychainSelected(t *testing.T) {
	p := &fileProvider{official: &official{Provider: stub.New()}}
	h := newHarness(t, p)
	var out, errb bytes.Buffer
	e := h.env(context.Background(), &out, &errb)
	e.KeychainProbe = func() error { return nil }
	if code := app.RunEnv(e, []string{"doctor"}); code != 0 {
		t.Fatalf("doctor exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "credential store   OS keychain") || strings.Contains(out.String(), "file permissions") {
		t.Fatalf("doctor:\n%s", out.String())
	}
	out.Reset()
	if code := app.RunEnv(e, []string{"status"}); code != 0 || !strings.Contains(out.String(), "credential store: OS keychain\n") {
		t.Fatalf("status exit=%d:\n%s", code, out.String())
	}
	if d := p.dir(); d != "" {
		t.Fatalf("UseFiles called with a usable keychain: %s", d)
	}
	if _, err := os.Stat(filepath.Join(h.home, "plugin", "credentials")); !os.IsNotExist(err) {
		t.Fatalf("credentials dir created in keychain mode: %v", err)
	}
}

// No keychain: the file store is selected, the provider is pointed at
// <state>/credentials, and doctor/status show it with the security note.
func TestCredentialStoreFilesSelected(t *testing.T) {
	p := &fileProvider{official: &official{Provider: stub.New()}}
	h := newFileHarness(t, p)
	code, out, _ := h.run(t, "doctor")
	if code != 0 {
		t.Fatalf("doctor exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "credential store   files (no OS keychain) in "+h.credDir()) ||
		!strings.Contains(out, "protected only by file permissions in your home directory") {
		t.Fatalf("doctor:\n%s", out)
	}
	if p.dir() != h.credDir() {
		t.Fatalf("UseFiles dir = %q, want %q", p.dir(), h.credDir())
	}
	code, out, _ = h.run(t, "status")
	if code != 0 || !strings.Contains(out, "credential store: files (no OS keychain)\n") {
		t.Fatalf("status exit=%d:\n%s", code, out)
	}
}

// A provider that cannot keep files is reported by doctor.
func TestCredentialStoreFilesRefusedByProvider(t *testing.T) {
	h := newFileHarness(t, refusingProvider{&official{Provider: stub.New()}})
	code, out, _ := h.run(t, "doctor")
	if code == 0 || !strings.Contains(out, "[FAIL] credential store") {
		t.Fatalf("doctor exit=%d:\n%s", code, out)
	}
}

type refusingProvider struct{ *official }

func (p refusingProvider) Auth() fleetapi.Auth { return refusingAuth{auth{p.official}} }

type refusingAuth struct{ auth }

func (refusingAuth) TokenStore() fleetapi.TokenStore { return refusingTS{} }

type refusingTS struct{}

func (refusingTS) Clear(context.Context) error { return nil }
func (refusingTS) UseFiles(string) error       { return errors.New("not an absolute path") }

// Without a keychain the daemon starts, creating a 0600 32-byte spool key in
// a 0700 credentials directory; logout then deletes the key, the token file
// and the spool.
func TestDaemonStartsAndLogoutPurgesWithoutKeychain(t *testing.T) {
	p := &fileProvider{official: &official{Provider: stub.New()}}
	h := newFileHarness(t, p)
	if code, out, errb := h.run(t, "login"); code != 0 {
		t.Fatalf("login exit=%d out=%s err=%s", code, out, errb)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dout, derr bytes.Buffer
	ddone := make(chan int, 1)
	go func() { ddone <- app.RunEnv(h.env(ctx, &dout, &derr), []string{"daemon"}) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c, err := daemon.Connect(ctx, h.state()); err == nil {
			if st, _ := c.Status(ctx); st.Enroll == "enrolled" {
				break
			}
		}
		select {
		case code := <-ddone:
			t.Fatalf("daemon exited %d: %s", code, derr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never enrolled; stderr=%s", derr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b, err := os.ReadFile(filepath.Join(h.credDir(), "STORE")); err != nil || string(b) != "files\n" {
		t.Fatalf("store marker = %q, %v", b, err)
	}
	keyFile := filepath.Join(h.credDir(), "spool-dev.key")
	k, err := os.ReadFile(keyFile)
	if err != nil || len(k) != 32 {
		t.Fatalf("spool key file: len=%d err=%v", len(k), err)
	}
	if runtime.GOOS != "windows" {
		if m := mode(t, keyFile); m != 0o600 {
			t.Fatalf("key file mode %#o", m)
		}
		if m := mode(t, h.credDir()); m != 0o700 {
			t.Fatalf("credentials dir mode %#o", m)
		}
	}
	if code, out, errb := h.run(t, "logout"); code != 0 {
		t.Fatalf("logout exit=%d out=%s err=%s", code, out, errb)
	}
	select {
	case code := <-ddone:
		if code != 0 {
			t.Fatalf("daemon exit=%d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop on logout")
	}
	for _, f := range []string{keyFile, filepath.Join(h.credDir(), "token.json"), filepath.Join(h.credDir(), "STORE")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s survived logout: %v", filepath.Base(f), err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(h.state(), "spool"))
	if len(ents) != 0 {
		t.Fatalf("spool not empty after logout: %v", ents)
	}
}

// Logout purges the spool and deletes its key in file mode even when the
// daemon is not running (the keychain-mode twin is
// TestLogoutPurgesEvenUnofficial).
func TestLogoutPurgesSpoolAndKeyFileWithoutKeychain(t *testing.T) {
	h := newFileHarness(t, stub.New())
	ks := spool.FileKeys{Dir: h.credDir()}
	key, err := spool.GetOrCreateKey(ks, "dev")
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(filepath.Join(h.state(), "spool"), key, spool.Options{})
	sp.Append("telemetry", []byte("x"))
	sp.Close()
	code, out, _ := h.run(t, "logout")
	if code != app.ExitUnofficial || !strings.Contains(out, "Local data purged") {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	ents, _ := os.ReadDir(filepath.Join(h.state(), "spool"))
	if len(ents) != 0 {
		t.Fatalf("spool not empty: %v", ents)
	}
	if _, err := ks.Get("dev"); err != spool.ErrNoKey {
		t.Fatalf("spool key file not deleted: %v", err)
	}
}

// A key file other users can read stops the daemon with the reason.
func TestDaemonRefusesLooseKeyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	h := newFileHarness(t, stub.New())
	if err := (spool.FileKeys{Dir: h.credDir()}).Set("dev", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(h.credDir(), "spool-dev.key"), 0o644)
	code, _, errb := h.run(t, "daemon")
	if code != app.ExitError || !strings.Contains(errb, "other users could read it") {
		t.Fatalf("exit=%d err=%s", code, errb)
	}
}

// A browser that cannot be opened (no display in a VM) falls back to the
// printed link instead of failing sign-in.
func TestLoginFallsBackWhenNoBrowser(t *testing.T) {
	h := newHarness(t, &official{Provider: stub.New()})
	var out, errb bytes.Buffer
	e := h.env(context.Background(), &out, &errb)
	e.OpenBrowser = func(string) error { return errors.New("no graphical display") }
	if code := app.RunEnv(e, []string{"login"}); code != 0 {
		t.Fatalf("login exit=%d out=%s err=%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "https://login.example/authorize") ||
		!strings.Contains(out.String(), "Could not open a browser here (no graphical display)") ||
		!strings.Contains(out.String(), "Signed in") {
		t.Fatalf("out:\n%s", out.String())
	}
}

// countingProbe counts probes and answers with err.
type countingProbe struct {
	mu  sync.Mutex
	n   int
	err error
}

func (c *countingProbe) probe() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.err
}

func (c *countingProbe) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *countingProbe) set(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func writeMarker(t *testing.T, dir, v string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "STORE"), []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hook and mcp never probe, before or after a store is recorded. With no
// recorded store they run signed out with the provider pointed at the
// (empty) files directory, so no keychain is touched.
func TestHookAndMCPNeverProbe(t *testing.T) {
	p := &fileProvider{official: &official{Provider: stub.New()}}
	h := newFileHarness(t, p)
	cp := &countingProbe{err: errNoKeychain}
	run := func(args ...string) int {
		var out, errb bytes.Buffer
		e := h.env(context.Background(), &out, &errb)
		e.KeychainProbe = cp.probe
		e.ServeMCP = func(context.Context, *sdk.Server) error { return nil }
		return app.RunEnv(e, args)
	}
	for _, marker := range []string{"", "files", "keychain"} {
		if marker != "" {
			writeMarker(t, h.credDir(), marker)
		}
		run("hook", "session-start")
		run("hook", "session-end")
		if code := run("mcp"); code != 0 {
			t.Fatalf("mcp exit %d (marker %q)", code, marker)
		}
		if n := cp.count(); n != 0 {
			t.Fatalf("probed %d times (marker %q)", n, marker)
		}
	}
	if p.dir() != h.credDir() {
		t.Fatalf("undecided/files mcp did not point the provider at files: %q", p.dir())
	}
	// Other commands do probe when nothing is recorded.
	os.Remove(filepath.Join(h.credDir(), "STORE"))
	run("status")
	if cp.count() != 1 {
		t.Fatalf("status probes = %d, want 1", cp.count())
	}
}

// Once files are recorded, a keychain that later appears does not move the
// credentials: doctor says it is staying with files.
func TestRecordedFilesDoNotFlap(t *testing.T) {
	p := &fileProvider{official: &official{Provider: stub.New()}}
	h := newFileHarness(t, p)
	if code, out, errb := h.run(t, "login"); code != 0 {
		t.Fatalf("login exit=%d out=%s err=%s", code, out, errb)
	}
	var out, errb bytes.Buffer
	e := h.env(context.Background(), &out, &errb)
	e.KeychainProbe = func() error { return nil } // the keychain is back
	if code := app.RunEnv(e, []string{"doctor"}); code != 0 {
		t.Fatalf("doctor exit=%d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "files (no OS keychain) in "+h.credDir()+" (recorded)") ||
		!strings.Contains(out.String(), "OS keychain available now; staying with files") {
		t.Fatalf("doctor:\n%s", out.String())
	}
	out.Reset()
	if code := app.RunEnv(e, []string{"status"}); code != 0 || !strings.Contains(out.String(), "credential store: files (no OS keychain)") {
		t.Fatalf("status:\n%s", out.String())
	}
}

// A recorded keychain that is unreachable fails clearly instead of switching
// to files (which would sign the member out).
func TestRecordedKeychainUnavailableFailsClosed(t *testing.T) {
	p := &fileProvider{official: &official{Provider: stub.New()}}
	h := newHarness(t, p) // injected key store: the real keychain is never used
	cp := &countingProbe{}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		e := h.env(context.Background(), &out, &errb)
		e.KeychainProbe = cp.probe
		code := app.RunEnv(e, args)
		return code, out.String(), errb.String()
	}
	if code, out, errb := run("login"); code != 0 {
		t.Fatalf("login exit=%d out=%s err=%s", code, out, errb)
	}
	cred := filepath.Join(h.home, "plugin", "credentials")
	if b, _ := os.ReadFile(filepath.Join(cred, "STORE")); string(b) != "keychain\n" {
		t.Fatalf("marker = %q", b)
	}
	cp.set(errNoKeychain) // the Secret Service went away
	const want = "OS keychain unavailable; try again or run `kenaz logout` to reset"
	if code, _, errb := run("login"); code == 0 || !strings.Contains(errb, want) {
		t.Fatalf("login exit=%d err=%s", code, errb)
	}
	if code, _, errb := run("daemon"); code != app.ExitError || !strings.Contains(errb, want) {
		t.Fatalf("daemon exit=%d err=%s", code, errb)
	}
	if code, out, _ := run("doctor"); code == 0 || !strings.Contains(out, "[FAIL] credential store   "+want) ||
		!strings.Contains(out, "no usable OS keychain (differs)") {
		t.Fatalf("doctor exit=%d:\n%s", code, out)
	}
	if d := p.dir(); d != "" {
		t.Fatalf("switched to files at %s", d)
	}
	// logout resets: the marker goes, and the next command chooses again.
	if code, out, errb := run("logout"); code != 0 {
		t.Fatalf("logout exit=%d out=%s err=%s", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(cred, "STORE")); !os.IsNotExist(err) {
		t.Fatalf("marker survived logout: %v", err)
	}
	if code, out, _ := run("status"); code != 0 || !strings.Contains(out, "credential store: files (no OS keychain)") {
		t.Fatalf("status after reset:\n%s", out)
	}
}

// A marker other users can read is refused, not followed.
func TestLooseMarkerRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	h := newFileHarness(t, &fileProvider{official: &official{Provider: stub.New()}})
	writeMarker(t, h.credDir(), "files")
	os.Chmod(filepath.Join(h.credDir(), "STORE"), 0o644)
	if code, out, _ := h.run(t, "doctor"); code == 0 || !strings.Contains(out, "other users could read it") {
		t.Fatalf("doctor exit=%d:\n%s", code, out)
	}
	if code, _, errb := h.run(t, "login"); code == 0 || !strings.Contains(errb, "credential store marker") {
		t.Fatalf("login exit=%d err=%s", code, errb)
	}
}
