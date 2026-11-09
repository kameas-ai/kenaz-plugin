package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	memorytools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/memimport"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

type capsProvider struct {
	fleetapi.Provider
	keys map[string]bool
}

func (p capsProvider) Auth() fleetapi.Auth { return capsAuth{Auth: p.Provider.Auth()} }

// Flavor is official: only an official build hears Fleet's answers.
func (capsProvider) Flavor() fleetapi.Flavor { return fleetapi.FlavorOfficial }
func (p capsProvider) Me() fleetapi.Me       { return capsMe{Me: p.Provider.Me(), keys: p.keys} }

type capsAuth struct{ fleetapi.Auth }

func (capsAuth) Session(context.Context) (fleetapi.Session, error) {
	return fleetapi.Session{Env: fleetapi.EnvDev, OrgID: "o", UserID: "u"}, nil
}

type capsMe struct {
	fleetapi.Me
	keys map[string]bool
}

func (m capsMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: m.keys}, nil
}

// The local trial never applies with an official provider, in the check at opt-in or
// in the worker.
func TestCaptureTrialNeverOfficial(t *testing.T) {
	old := features.MemoryCaptureLocalTrial
	defer func() { features.MemoryCaptureLocalTrial = old }()
	official := officialSession{sessionProvider{stub.New(), fleetapi.EnvDev}}
	features.MemoryCaptureLocalTrial = true
	if captureTrialAllowed(official) {
		t.Error("the trial applied with an official provider")
	}
	if captureWorker(Env{Provider: official}, memoryPaths{}).HostCLIUnverifiedOK {
		t.Error("the worker skipped the check with an official provider")
	}
	if !captureTrialAllowed(nil) || !captureTrialAllowed(stub.New()) {
		t.Error("the trial did not apply without an official provider")
	}
	if !captureWorker(Env{Provider: stub.New()}, memoryPaths{}).HostCLIUnverifiedOK {
		t.Error("the worker ignored the trial")
	}
	features.MemoryCaptureLocalTrial = false
	if captureTrialAllowed(nil) || captureTrialAllowed(stub.New()) {
		t.Error("the trial applied with the feature off")
	}
}

// An off switch reads as Fleet means it, whatever the case of the key.
func TestCaptureSwitchParsing(t *testing.T) {
	const off = "memory_capture_off"
	for _, c := range []struct {
		keys  map[string]bool
		allow bool
	}{
		{nil, true},
		{map[string]bool{"context_sync": true}, true},
		{map[string]bool{off: false}, true},
		{map[string]bool{off: true}, false},
		{map[string]bool{"MEMORY_CAPTURE_OFF": true}, false},
	} {
		env := Env{Provider: capsProvider{Provider: stub.New(), keys: c.keys}}
		if allowed, known := env.captureCapAllows(context.Background(), off); !known || allowed != c.allow {
			t.Errorf("%v: allowed %v, known %v", c.keys, allowed, known)
		}
	}
	// Signed out, nothing is known.
	if _, known := (Env{Provider: stub.New()}).captureCapAllows(context.Background(), off); known {
		t.Error("a signed-out device knew an answer")
	}
}

// Signed in, capture is offered only on memory_capture_kinds, present and true,
// from an official build that could read Fleet's answer. The off switch still wins.
func TestCaptureOfferedParsing(t *testing.T) {
	old := features.MemoryCapture
	features.MemoryCapture = true
	defer func() { features.MemoryCapture = old }()
	if !captureAvailable() {
		t.Skip("capture isn't available on this platform")
	}
	kinds := fleetapi.CapMemoryCaptureKinds
	for _, c := range []struct {
		keys    map[string]bool
		offered bool
		reason  string
		refusal string
	}{
		{nil, false, capture.PauseNotOffered, usertext.CaptureNotOffered},
		{map[string]bool{"context_sync": true, fleetapi.CapMemorySync: true}, false, capture.PauseNotOffered, usertext.CaptureNotOffered},
		{map[string]bool{kinds: false}, false, capture.PauseNotOffered, usertext.CaptureNotOffered},
		{map[string]bool{kinds: true}, true, "", ""},
		{map[string]bool{"MEMORY_CAPTURE_KINDS": true}, true, "", ""},
		{map[string]bool{kinds: true, fleetapi.CapMemoryCaptureOff: true}, true, "", usertext.CaptureOrgOff},
	} {
		env := Env{Provider: capsProvider{Provider: stub.New(), keys: c.keys}}
		offered, reason, profile := env.captureOffered(context.Background())
		if offered != c.offered || reason != c.reason || profile == "local" || profile == "" {
			t.Errorf("%v: offered %v, reason %q, profile %q", c.keys, offered, reason, profile)
		}
		// Signed in, every writer gets the same answer.
		for _, w := range []string{capture.WriterAPIKey, capture.WriterHostCLI} {
			if got := env.captureRefusal(context.Background(), w); got != c.refusal {
				t.Errorf("%v/%s: refusal %q", c.keys, w, got)
			}
		}
	}
	// An unofficial build hears no answer: not offered, whoever is signed in.
	for _, p := range []fleetapi.Provider{stub.New(), unofficialCaps{capsProvider{Provider: stub.New(), keys: map[string]bool{kinds: true}}}} {
		env := Env{Provider: p}
		if offered, _, _ := env.captureOffered(context.Background()); offered {
			t.Errorf("%T: offered", p)
		}
		if env.captureRefusal(context.Background(), capture.WriterAPIKey) != usertext.CaptureNeedsOfficialBuild {
			t.Errorf("%T: %q", p, env.captureRefusal(context.Background(), capture.WriterAPIKey))
		}
	}
	// A failed read is not an answer.
	env := Env{Provider: capsErrProvider{capsProvider{Provider: stub.New()}}}
	if offered, reason, _ := env.captureOffered(context.Background()); offered || reason != capture.PauseUnconfirmed {
		t.Errorf("read failed: offered %v, %q", offered, reason)
	}
	if env.captureRefusal(context.Background(), capture.WriterAPIKey) != usertext.CaptureUnconfirmedForOrg {
		t.Errorf("read failed: %q", env.captureRefusal(context.Background(), capture.WriterAPIKey))
	}
	// The worker the app builds asks the same question.
	w := captureWorker(Env{Provider: capsProvider{Provider: stub.New(), keys: map[string]bool{kinds: true}}}, memoryPaths{})
	if offered, _, _ := w.Offered(context.Background()); !offered {
		t.Error("the worker doesn't hear the offer")
	}
	w = captureWorker(Env{Provider: capsProvider{Provider: stub.New()}}, memoryPaths{})
	if offered, _, _ := w.Offered(context.Background()); offered {
		t.Error("the worker ran without the offer")
	}
}

type unofficialCaps struct{ capsProvider }

func (unofficialCaps) Flavor() fleetapi.Flavor { return fleetapi.FlavorUnofficial }

type capsErrProvider struct{ capsProvider }

func (p capsErrProvider) Me() fleetapi.Me { return capsErrMe{p.Provider.Me()} }

type capsErrMe struct{ fleetapi.Me }

func (capsErrMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{}, errors.New("offline")
}

// Capture stays off on Windows, whatever the build, until its tests have run there
// for a release.
func TestCaptureAvailable(t *testing.T) {
	old := features.MemoryCapture
	defer func() { features.MemoryCapture = old }()
	features.MemoryCapture = false
	if captureAvailable() {
		t.Error("available with the feature off")
	}
	features.MemoryCapture = true
	if got, want := captureAvailable(), runtime.GOOS != "windows"; got != want {
		t.Errorf("available = %v on %s", got, runtime.GOOS)
	}
	src, err := os.ReadFile("memorycapture.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `features.MemoryCapture && runtime.GOOS != "windows"`) {
		t.Error("the Windows gate is gone")
	}
}

// The approval page says which notes are automatic and that they stay on this device.
// It reads that from the payload, so what is approved is what is shown.
func TestAdoptPageNamesAutomaticNotes(t *testing.T) {
	render := renderMemory(memory.OpAdopt)
	body := "text"
	page := func(kind string) approvals.Rendered {
		t.Helper()
		pl, err := json.Marshal(map[string]any{
			"profile": map[string]string{"env": "dev", "org_id": "o", "user_id": "u"},
			"records": []memory.HeldOp{{RecordID: "mem-1", Op: memory.OpAdopt, Title: "a title", Hash: memory.ContentHash(body), Body: body, Kind: kind}},
		})
		if err != nil {
			t.Fatal(err)
		}
		r, err := render(pl)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	auto := page(memory.KindObservation)
	if !strings.Contains(auto.Impact, "Automatic notes then sync once you let new notes sync without asking") || !strings.Contains(auto.Details[0].Label, "automatic note") {
		t.Errorf("automatic: %+v", auto)
	}
	if plain := page("raw"); strings.Contains(plain.Impact, "Automatic") || strings.Contains(plain.Details[0].Label, "automatic") {
		t.Errorf("deliberate: %+v", plain)
	}
}

// A notice names the installed tools; in tests that never starts one.
func TestHarnessVersionIsStubbedInTests(t *testing.T) {
	if got := harnessVersion("definitely-not-run"); got != "" {
		t.Fatalf("%q", got)
	}
}

// runCLIForTest is this test binary running as the CLI (see cliTestArgs).
func runCLIForTest(args string) {
	features.MemoryCapture = os.Getenv("KENAZ_TEST_CAPTURE_FEATURE") == "1"
	code := RunEnv(Env{
		Provider: stub.New(), Stdout: os.Stdout, Stderr: os.Stderr, Ctx: context.Background(),
		StartCaptureWorker: func() error { return nil },
	}, strings.Fields(args))
	os.Exit(code)
}

// hookStdinUnread runs one hook as a whole process, with the session's content already
// waiting in a pipe on its stdin, and returns how many of those bytes it left unread.
func hookStdinUnread(t *testing.T, event string, feature bool, hostEnv string, settings map[string]string, offered bool) int {
	t.Helper()
	home := filepath.Join(t.TempDir(), "h")
	if err := os.MkdirAll(filepath.Join(home, "plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if offered {
		// Kameas Fleet said just now that it offers capture to the organization.
		if err := capture.RecordOffered(capture.PathsIn(filepath.Join(home, "plugin")), time.Now(), "local"); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range settings {
		if err := config.SetValue(filepath.Join(home, "plugin", "config.toml"), k, v); err != nil {
			t.Fatal(err)
		}
	}
	cwd := t.TempDir()
	payload, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": cwd, "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_input": map[string]string{"command": "ls"}, "tool_response": "text the hook must not read", "prompt": "a prompt"})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Stdin = r
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	feat := "0"
	if feature {
		feat = "1"
	}
	cmd.Env = []string{cliTestArgs + "=hook " + event, "KENAZ_TEST_CAPTURE_FEATURE=" + feat, "HOME=" + t.TempDir(), variant.Shared.HomeVar + "=" + home, hostEnv}
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hook: %v: %s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the hook did not return: it is waiting on stdin")
	}
	w.Close()
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return len(rest)
}

// A registered hook is handed the session's prompts and tool output whatever the
// member chose. With capture off it must not read a byte of them.
func TestCaptureHookOffLeavesStdinUnread(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("capture is off on Windows")
	}
	const claude, codex = "CLAUDE_PLUGIN_ROOT=/p", "PLUGIN_ROOT=/p"
	on := map[string]string{capture.KeySwitch: "on", capture.KeyAck: capture.AckVersion, capture.KeyWriter: capture.WriterLocal, capture.KeyCommand: "true"}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range on {
			m[a] = b
		}
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	all := []string{capture.HookPrompt, capture.HookTool, capture.HookStop, capture.HookIndex}
	off := []struct {
		name    string
		feature bool
		host    string
		config  map[string]string
		events  []string
	}{
		{"a build without the feature", false, claude, on, all},
		{"never turned on", true, claude, nil, all},
		{"turned off", true, claude, with(capture.KeySwitch, "off"), all},
		{"an older notice", true, claude, with(capture.KeyAck, "memory-capture/v0"), all},
		{"no writer", true, claude, with(capture.KeyWriter, ""), all},
		{"Codex", true, codex, on, all},
		{"an unknown host", true, "X=1", on, all},
		{"memory off", true, claude, with("memory", "off"), all},
		{"a writer that reads no content", true, claude, with(capture.KeyWriter, capture.WriterSessionAgent), []string{capture.HookTool, capture.HookStop}},
	}
	for _, c := range off {
		for _, ev := range c.events {
			t.Run(c.name+"/"+ev, func(t *testing.T) {
				if hookStdinUnread(t, ev, c.feature, c.host, c.config, true) == 0 {
					t.Fatal("the hook read the session's content")
				}
			})
		}
	}
	// Turned on, but Kameas Fleet hasn't said it offers capture to the organization:
	// nothing is read either.
	for _, ev := range all {
		if hookStdinUnread(t, ev, true, claude, on, false) == 0 {
			t.Fatalf("not offered/%s: the hook read the session's content", ev)
		}
	}
	// The same harness sees a hook that does read: the proof above can fail.
	if got := hookStdinUnread(t, capture.HookTool, true, claude, on, true); got != 0 {
		t.Fatalf("capture on: %d bytes left unread", got)
	}
}

// With capture off a hook sets nothing up: no credential store, no keychain, no files.
func TestCaptureHookOffSetsNothingUp(t *testing.T) {
	old := features.MemoryCapture
	features.MemoryCapture = true
	defer func() { features.MemoryCapture = old }()
	home := filepath.Join(t.TempDir(), "h")
	var out bytes.Buffer
	env := Env{Provider: stub.New(), Stdout: &out, Stderr: &out, Ctx: context.Background(),
		Getenv: func(k string) string {
			switch k {
			case variant.Shared.HomeVar:
				return home
			case "CLAUDE_PLUGIN_ROOT":
				return "/p"
			}
			return ""
		},
		KeychainProbe: func() error { t.Error("the hook touched the keychain"); return nil },
	}
	for _, ev := range []string{capture.HookIndex, capture.HookPrompt, capture.HookTool, capture.HookStop} {
		if code := RunEnv(env, []string{"hook", ev}); code != ExitOK || out.Len() != 0 {
			t.Fatalf("%s: %d %q", ev, code, out.String())
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the hook left files behind: %v", err)
	}
}

// Bringing notes over counts as capture, so an organization's switch stops it too.
func TestImportRefusedWithCaptureOffForTheOrg(t *testing.T) {
	old := features.MemoryCapture
	features.MemoryCapture = true
	defer func() { features.MemoryCapture = old }()
	home := filepath.Join(t.TempDir(), "h")
	if err := os.MkdirAll(filepath.Join(home, "plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{capture.KeySwitch: "on", capture.KeyAck: capture.AckVersion, capture.KeyWriter: capture.WriterLocal, capture.KeyCommand: "true"} {
		if err := config.SetValue(filepath.Join(home, "plugin", "config.toml"), k, v); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	env := Env{Provider: capsProvider{Provider: stub.New(), keys: map[string]bool{fleetapi.CapMemoryCaptureKinds: true, fleetapi.CapMemoryCaptureOff: true}}, Stdout: &out, Stderr: &errb, Ctx: context.Background(),
		Getenv: func(k string) string {
			if k == variant.Shared.HomeVar {
				return home
			}
			return ""
		}, KeyStore: nil, KeychainProbe: func() error { return nil }}
	mp, err := env.memoryPaths()
	if err != nil {
		t.Fatal(err)
	}
	if code := runMemoryImport(context.Background(), env, mp, []string{"claude-mem", "--yes", "--db", filepath.Join(t.TempDir(), "none")}); code != ExitError || !strings.Contains(errb.String(), usertext.CaptureOrgOff) {
		t.Errorf("%d %s", code, errb.String())
	}
}

// A name from another database can't drive the terminal.
func TestImportPlanStripsTerminalEscapes(t *testing.T) {
	var out bytes.Buffer
	printImportPlan(Env{Stdout: &out}, memimport.Plan{Projects: map[string]memimport.ProjectPlan{"a\x1b[31mb\nc": {Notes: 1}}})
	if strings.ContainsAny(out.String(), "\x1b") {
		t.Errorf("%q", out.String())
	}
}

// A character device isn't a terminal: `kenaz memory recall all </dev/null` must not pass.
func TestTerminalCheckRefusesOtherDevices(t *testing.T) {
	old := os.Stdin
	defer func() { os.Stdin = old }()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	os.Stdin = f
	if memoryStdinTerminal() {
		t.Errorf("%s counted as a terminal", os.DevNull)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	os.Stdin = r
	if memoryStdinTerminal() {
		t.Error("a pipe counted as a terminal")
	}
}

// A project's root has its links resolved, so a linked home is compared both ways.
func TestHomeFormsResolveALink(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "nfs", "alice")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	t.Setenv("HOME", link)
	got := homeForms()
	if len(got) != 2 || got[0] != link || got[1] != real {
		t.Errorf("%v", got)
	}
	if name := memory.ProjectName("rk1-abcdef0123456789", real, got...); name == "alice" {
		t.Errorf("the username was sent: %q", name)
	}
}

// A note Fleet refused during a hold, kept for an edit, reads as queued, because it does
// go. An edit waiting for approval still asks, and still works once approved.
func TestSkippedProbeShowsAsQueued(t *testing.T) {
	for name, approved := range map[string]bool{"approved edit": true, "unapproved edit": false} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := memory.Profile{Env: "dev", OrgID: "o", UserID: "u"}
			st, err := memory.OpenProfile(filepath.Join(t.TempDir(), "m.db"), p)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if _, err := st.EnableSync(ctx, "memory-sync/v3", memory.MutateOptions{SyncApproved: true}); err != nil {
				t.Fatal(err)
			}
			if err := st.GrantSaves(ctx, ""); err != nil {
				t.Fatal(err)
			}
			id, _, err := st.SaveAuto(ctx, memory.AutoNote{ProjectKey: memory.GlobalProject, Title: "o", Body: "b", Source: memory.SourceCLI, Kind: memory.KindObservation})
			if err != nil {
				t.Fatal(err)
			}
			l, err := st.AcquireLease(ctx, "h", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if q, err := st.QueueAutoBacklog(ctx, l); q != 1 || err != nil {
				t.Fatal(q, err)
			}
			b, err := st.LeasePushBatch(ctx, l)
			if err != nil || len(b.Ops) != 1 {
				t.Fatalf("%+v %v", b, err)
			}
			no := false
			if _, err := st.ApplyPushResults(ctx, l, b.Ops, []memory.PushResult{{ID: id, Status: "rejected", Code: "invalid_field", Field: "kind", Retry: &no}}); err != nil {
				t.Fatal(err)
			}
			title := "a person's title"
			if _, err := st.UpdateNote(ctx, id, memory.Update{Title: &title}, memory.MutateOptions{SyncApproved: approved}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.EndSaveGrant(ctx); err != nil {
				t.Fatal(err)
			}
			s, err := st.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := syncNotesLine(s); !strings.Contains(got, "1 queued, 0 waiting for approval, 0 on this device only") {
				t.Errorf("%q", got)
			}
			if !approved {
				if got := memorytools.SyncSummary(s, true); !strings.Contains(got, "wait for your approval") {
					t.Errorf("the waiting edit isn't mentioned: %q", got)
				}
				held, err := st.HeldOps(ctx)
				if err != nil || len(held) != 1 {
					t.Fatalf("%+v %v", held, err)
				}
				if n, err := st.ApproveOps(ctx, held, ""); err != nil || n != 1 {
					t.Errorf("approved %d, %v", n, err)
				}
				if s, _ := st.Status(ctx); s.Queue["unapproved"] != 0 || s.Queue["ready"] != 2 {
					t.Errorf("%+v", s.Queue)
				}
			}
		})
	}
}
