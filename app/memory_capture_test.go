package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

// underClaudeCode adds what Claude Code gives a plugin hook to an environment.
func underClaudeCode(get func(string) string) func(string) string {
	return func(k string) string {
		if k == "CLAUDE_PLUGIN_ROOT" {
			return "/p"
		}
		if get != nil {
			return get(k)
		}
		return os.Getenv(k)
	}
}

func withCapture(t *testing.T) {
	t.Helper()
	old := features.MemoryCapture
	features.MemoryCapture = true
	t.Cleanup(func() { features.MemoryCapture = old })
}

func (h *harness) configFile() string { return filepath.Join(h.home, "plugin", "config.toml") }

func (h *harness) captureOn(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.configFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{capture.KeySwitch: "on", capture.KeyAck: capture.AckVersion, capture.KeyWriter: capture.WriterLocal, capture.KeyCommand: "x"} {
		if err := config.SetValue(h.configFile(), k, v); err != nil {
			t.Fatal(err)
		}
	}
	h.captureOffered(t)
}

// offeringProvider is signed in to an organization Kameas Fleet offers automatic
// capture to (memory_capture_kinds).
type offeringProvider struct{ orgProvider }

func (offeringProvider) Me() fleetapi.Me { return offerMe{} }

type offerMe struct{ me }

func (offerMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{fleetapi.CapMemoryCaptureKinds: true, fleetapi.CapMemorySync: true, fleetapi.CapMemoryProjectScope: true}}, nil
}

func offering() fleetapi.Provider {
	return offeringProvider{orgProvider{&official{Provider: stub.New()}}}
}

// captureOffered records Kameas Fleet's answer, just now, that it offers capture.
func (h *harness) captureOffered(t *testing.T) {
	t.Helper()
	if err := capture.RecordOffered(h.capturePaths(), time.Now(), "local"); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) capturePaths() capture.Paths {
	return capture.PathsIn(filepath.Join(h.home, "plugin"))
}

// runCaptureHook runs a capture hook the way a host does and returns stdout.
func (h *harness) runCaptureHook(t *testing.T, event string, body map[string]any, started *int) string {
	t.Helper()
	b, _ := json.Marshal(body)
	defer app.SetMemoryStdin(false, bytes.NewReader(b))()
	var out, errb bytes.Buffer
	env := h.env(context.Background(), &out, &errb)
	env.StartCaptureWorker = func() error { *started++; return nil }
	env.Getenv = underClaudeCode(env.Getenv)
	if code := app.RunEnv(env, []string{"hook", event}); code != 0 {
		t.Fatalf("hook exit %d", code)
	}
	if errb.Len() != 0 {
		t.Fatalf("hook wrote to stderr: %s", errb.String())
	}
	return out.String()
}

// In a release build nothing about capture is reachable.
func TestCaptureAbsentFromReleaseBuilds(t *testing.T) {
	if features.MemoryCapture {
		t.Skip("this build carries the feature")
	}
	h := newHarness(t, stub.New())
	h.captureOn(t) // even a config copied from elsewhere does nothing
	code, _, errb := h.run(t, "memory", "capture", "enable", "--writer", "api-key")
	if code == 0 || !strings.Contains(errb, "not available in this build") {
		t.Errorf("enable: %d %q", code, errb)
	}
	started := 0
	if out := h.runCaptureHook(t, capture.HookTool, map[string]any{"session_id": "s", "cwd": t.TempDir(), "tool_name": "Edit"}, &started); out != "" {
		t.Errorf("hook wrote %q", out)
	}
	if capture.QueueLen(h.capturePaths()) != 0 || started != 0 {
		t.Error("a release build captured")
	}
	_, out, _ := h.run(t, "status")
	if strings.Contains(out, "capture") {
		t.Errorf("status mentions capture: %s", out)
	}
	_, out, _ = h.run(t, "memory", "help")
	if strings.Contains(out, "memory capture ") {
		t.Errorf("help mentions capture: %s", out)
	}
}

func TestCaptureOffByDefaultAndOptIn(t *testing.T) {
	withCapture(t)
	h := newHarness(t, offering())
	_, out, _ := h.run(t, "memory", "capture", "status")
	if !strings.HasPrefix(out, "memory capture: off") {
		t.Errorf("status: %s", out)
	}
	started := 0
	h.runCaptureHook(t, capture.HookTool, map[string]any{"session_id": "s", "cwd": t.TempDir(), "tool_name": "Edit"}, &started)
	if capture.QueueLen(h.capturePaths()) != 0 || started != 0 {
		t.Fatal("captured while off")
	}
	if code, _, _ := h.run(t, "memory", "capture", "enable", "--writer", "whatever"); code != app.ExitUsage {
		t.Errorf("enable with an unknown writer: %d", code)
	}
	if code, _, _ := h.run(t, "memory", "capture", "enable"); code != app.ExitUsage {
		t.Errorf("enable with no writer: %d", code)
	}
	// Not a terminal: the notice is shown, nothing turns on.
	restore := app.SetMemoryStdin(false, strings.NewReader("y"+"\n"))
	code, _, _ := h.run(t, "memory", "capture", "enable", "--writer", "api-key")
	restore()
	if code == 0 {
		t.Error("enabled without a terminal")
	}
	// No, or anything else, records nothing.
	for _, answer := range []string{"n", "no", "Not now", "maybe"} {
		restore = app.SetMemoryStdin(true, strings.NewReader(answer+"\n"))
		_, out, _ = h.run(t, "memory", "capture", "enable", "--writer", "api-key")
		restore()
		if !strings.Contains(out, "Capture stays off") {
			t.Errorf("%q: %s", answer, out)
		}
		b, _ := os.ReadFile(h.configFile())
		if strings.Contains(string(b), "memory_capture") {
			t.Errorf("%q recorded something: %s", answer, b)
		}
	}
	// The notice appears verbatim with the true model line.
	restore = app.SetMemoryStdin(true, strings.NewReader("y"+"\n"))
	code, out, _ = h.run(t, "memory", "capture", "enable", "--writer", "api-key")
	restore()
	if code != 0 || !strings.Contains(out, notices.MemoryCapture(notices.MemoryCaptureAPIKey("Anthropic")).Text) || !strings.Contains(out, "Turn on capture? Press Enter or y for yes; anything else for no: ") {
		t.Errorf("enable: %d %s", code, out)
	}
	for _, banned := range []string{"I agree", "Accept", "Consent"} {
		if strings.Contains(out, banned) {
			t.Errorf("enable says %q", banned)
		}
	}
	_, out, _ = h.run(t, "status")
	if !strings.Contains(out, "memory capture: on (api-key)") {
		t.Errorf("status: %s", out)
	}
	// Off again: the switch is off and what waited is gone.
	if err := capture.Enqueue(h.capturePaths(), capture.Event{At: time.Now().UnixMilli(), Session: "s", Kind: "tool"}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := h.run(t, "memory", "capture", "disable"); code != 0 || capture.QueueLen(h.capturePaths()) != 0 {
		t.Errorf("disable: %d, queue %d", code, capture.QueueLen(h.capturePaths()))
	}
}

// Any member can pick the host CLI writer, after seeing its line, and it is never
// the default: with no writer chosen, capture stays off.
func TestHostCLIWriterIsChosenNeverDefault(t *testing.T) {
	withCapture(t)
	h := newHarness(t, offering())
	restore := app.SetMemoryStdin(true, strings.NewReader("y"+"\n"))
	defer restore()
	if code, out, _ := h.run(t, "memory", "capture", "enable"); code != app.ExitUsage || strings.Contains(out, "Turn on capture?pture") {
		t.Errorf("enable with no writer: %d %s", code, out)
	}
	_, out, _ := h.run(t, "memory", "capture", "status")
	if !strings.HasPrefix(out, "memory capture: off") {
		t.Errorf("status: %s", out)
	}
	line := notices.MemoryCaptureHostCLI(capture.DefaultTool, capture.DefaultProvider)
	code, out, _ := h.run(t, "memory", "capture", "enable", "--writer", capture.WriterHostCLI)
	if code != 0 || !strings.Contains(out, line) {
		t.Errorf("enable: %d %s", code, out)
	}
	_, out, _ = h.run(t, "memory", "capture", "status")
	if !strings.Contains(out, "counts against your plan") {
		t.Errorf("status does not show where usage goes: %s", out)
	}
	if strings.Contains(strings.ToLower(out), "subscription") {
		t.Errorf("no copy may market this: %s", out)
	}
}

// A hook prints nothing, whatever the event. On some hosts, stdout from a
// prompt hook would become part of the conversation.
func TestCaptureHooksArePureObservers(t *testing.T) {
	withCapture(t)
	h := newHarness(t, stub.New())
	h.captureOn(t)
	cwd := t.TempDir()
	started := 0
	for _, c := range []struct {
		event string
		body  map[string]any
	}{
		{capture.HookPrompt, map[string]any{"session_id": "s", "cwd": cwd, "prompt": "fix it"}},
		{capture.HookTool, map[string]any{"session_id": "s", "cwd": cwd, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.go"}}},
		{capture.HookStop, map[string]any{"session_id": "s", "cwd": cwd}},
	} {
		if out := h.runCaptureHook(t, c.event, c.body, &started); out != "" {
			t.Errorf("%s wrote %q", c.event, out)
		}
	}
	if capture.QueueLen(h.capturePaths()) != 3 || started != 3 {
		t.Errorf("queue %d, worker starts %d", capture.QueueLen(h.capturePaths()), started)
	}
	// It does not touch the relay's state.
	if _, err := os.Stat(h.state()); err == nil {
		t.Error("a capture hook set up Fleet state")
	}
}

// A worker's own model call does nothing through any Kenaz hook: no capture,
// no relay start, no session-end signal, no context.
func TestNoRecursion(t *testing.T) {
	withCapture(t)
	h := newHarness(t, stub.New())
	h.captureOn(t)
	cwd := t.TempDir()
	for _, event := range []string{"session-start", "session-end", capture.HookPrompt, capture.HookTool, capture.HookStop, capture.HookIndex} {
		b, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": cwd, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.go"}})
		restore := app.SetMemoryStdin(false, bytes.NewReader(b))
		var out, errb bytes.Buffer
		env := h.env(context.Background(), &out, &errb)
		get := env.Getenv
		env.Getenv = func(k string) string {
			if k == capture.WorkerEnv {
				return "1"
			}
			return get(k)
		}
		env.StartCaptureWorker = func() error { t.Errorf("%s started a worker", event); return nil }
		code := app.RunEnv(env, []string{"hook", event})
		restore()
		if code != 0 || out.Len() != 0 || errb.Len() != 0 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", event, code, out.String(), errb.String())
		}
	}
	if capture.QueueLen(h.capturePaths()) != 0 {
		t.Error("a worker session captured itself")
	}
}

func TestLogoutClearsTheQueue(t *testing.T) {
	withCapture(t)
	h := newHarness(t, stub.New())
	if err := capture.Enqueue(h.capturePaths(), capture.Event{At: time.Now().UnixMilli(), Session: "s", Kind: "tool"}); err != nil {
		t.Fatal(err)
	}
	h.run(t, "logout")
	if capture.QueueLen(h.capturePaths()) != 0 {
		t.Error("logout left the capture queue")
	}
}

// The default once someone opts in is the background observer, and changing the writer
// shows the new line first and only takes effect on the answer.
func TestWriterDefaultAndChange(t *testing.T) {
	withCapture(t)
	h := newHarness(t, offering())
	enable := func(answer string, args ...string) string {
		t.Helper()
		restore := app.SetMemoryStdin(true, strings.NewReader(answer+"\n"))
		defer restore()
		code, out, errb := h.run(t, append([]string{"memory", "capture", "enable"}, args...)...)
		if code != 0 {
			t.Fatalf("enable %v: %d %s", args, code, errb)
		}
		return out
	}
	out := enable("y", "--writer", "api-key")
	if !strings.Contains(out, notices.MemoryCaptureAPIKey("Anthropic")) {
		t.Errorf("line: %s", out)
	}
	_, out, _ = h.run(t, "status")
	if !strings.Contains(out, "memory capture: on (api-key)") {
		t.Errorf("status: %s", out)
	}
	// No on a change keeps the old writer, after showing the new line.
	out = enable("n", "--writer", "local", "--command", "my-model")
	if !strings.Contains(out, notices.MemoryCaptureLocal) || !strings.Contains(out, "Nothing changed") {
		t.Errorf("change declined: %s", out)
	}
	_, out, _ = h.run(t, "status")
	if !strings.Contains(out, "on (api-key)") {
		t.Errorf("status after decline: %s", out)
	}
	enable("y", "--writer", "local", "--command", "my-model")
	_, out, _ = h.run(t, "status")
	if !strings.Contains(out, "on (local)") {
		t.Errorf("status after change: %s", out)
	}
}

// The reminder is the only thing a hook prints, in the host's own format.
func TestSessionAgentReminderOutput(t *testing.T) {
	withCapture(t)
	h := newHarness(t, offering())
	restore := app.SetMemoryStdin(true, strings.NewReader("y"+"\n"))
	h.run(t, "memory", "capture", "enable", "--writer", capture.WriterSessionAgent)
	restore()
	started := 0
	out := h.runCaptureHook(t, capture.HookPrompt, map[string]any{"session_id": "s", "cwd": t.TempDir(), "prompt": "hi"}, &started)
	var got struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.HookSpecificOutput.HookEventName != "UserPromptSubmit" || !strings.Contains(got.HookSpecificOutput.AdditionalContext, "memory_save") {
		t.Fatalf("%v: %q", err, out)
	}
	if strings.Contains(out, `"continue"`) || strings.Contains(out, `"decision"`) {
		t.Errorf("a reminder must never steer the session: %s", out)
	}
	if out := h.runCaptureHook(t, capture.HookStop, map[string]any{"session_id": "s", "cwd": t.TempDir()}, &started); out != "" {
		t.Errorf("stop hook: %q", out)
	}
}

// Turned on, but Kameas Fleet hasn't said it offers capture to the organization: hooks
// queue nothing, the session gets the plain index, and status says why, truthfully.
func TestCaptureOnButNotOffered(t *testing.T) {
	withCapture(t)
	h := newHarness(t, stub.New())
	h.captureOn(t)
	cwd := t.TempDir()
	st, err := memory.Open(h.memoryDB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveNote(context.Background(), memory.NewNote{ProjectKey: memory.GlobalProject, Title: "Plain title", Body: "b", Source: memory.SourceCLI}, memory.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	for _, c := range []struct {
		name, code, line string
	}{
		{"never answered", "", "Kameas Fleet hasn't confirmed lately"},
		{"answered no", capture.PauseNotOffered, "isn't available for your organization yet"},
	} {
		s := capture.LoadState(h.capturePaths())
		s.OfferedAt, s.OfferCode = 0, c.code
		b, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(h.capturePaths().Dir, "state.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		started := 0
		for _, ev := range []string{capture.HookPrompt, capture.HookTool, capture.HookStop} {
			if out := h.runCaptureHook(t, ev, map[string]any{"session_id": "s", "cwd": cwd, "tool_name": "Edit", "prompt": "p"}, &started); out != "" {
				t.Errorf("%s/%s: %q", c.name, ev, out)
			}
		}
		if capture.QueueLen(h.capturePaths()) != 0 {
			t.Errorf("%s: queued %d", c.name, capture.QueueLen(h.capturePaths()))
		}
		if out := h.runCaptureHook(t, capture.HookIndex, map[string]any{"session_id": "s", "cwd": cwd}, &started); out != "" {
			t.Errorf("%s: rich index %q", c.name, out)
		}
		restore := app.SetMemoryStdin(false, strings.NewReader(`{"session_id":"s","cwd":"`+cwd+`"}`))
		_, plain, _ := h.run(t, "hook", "session-start")
		restore()
		if !strings.Contains(plain, "Plain title") {
			t.Errorf("%s: no plain index: %q", c.name, plain)
		}
		for _, args := range [][]string{{"memory", "capture", "status"}, {"status"}, {"doctor"}} {
			if _, out, _ := h.run(t, args...); !strings.Contains(out, "but not running: ") || !strings.Contains(out, c.line) {
				t.Errorf("%s: %v: %s", c.name, args, out)
			}
		}
	}
}

// The rich index appears only for someone who turned capture on.
func TestRichIndexHook(t *testing.T) {
	withCapture(t)
	h := newHarness(t, stub.New())
	cwd := t.TempDir()
	run := func() string {
		started := 0
		return h.runCaptureHook(t, capture.HookIndex, map[string]any{"session_id": "s", "cwd": cwd}, &started)
	}
	if out := run(); out != "" {
		t.Errorf("off: %q", out)
	}
	h.captureOn(t)
	if out := run(); out != "" {
		t.Errorf("nothing to show: %q", out)
	}
	restore := app.SetMemoryStdin(false, strings.NewReader(""))
	defer restore()
	p, err := memory.ProjectFor(cwd)
	if err != nil {
		t.Fatal(err)
	}
	st, err := memory.Open(h.memoryDB())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SaveAuto(context.Background(), memory.AutoNote{ProjectKey: p.Key, Title: "Captured title", Body: "b", Source: memory.SourceCodex, Kind: memory.KindObservation, Tags: []string{memory.AutoTag, "type:change", "session:abcd"}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	var got struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(run()), &got); err != nil {
		t.Fatal(err)
	}
	if got.HookSpecificOutput.HookEventName != "SessionStart" || !strings.Contains(got.HookSpecificOutput.AdditionalContext, "Captured title") || !strings.Contains(got.HookSpecificOutput.AdditionalContext, "untrusted") {
		t.Errorf("index output: %+v", got)
	}
}

func TestMemoryRecallSetting(t *testing.T) {
	h := newHarness(t, stub.New())
	// Nothing can make sure the member sees what reading every project means, so they
	// choose it.
	if _, out, _ := h.run(t, "memory", "recall"); !strings.Contains(out, "not tied to a project") {
		t.Errorf("default: %s", out)
	}
	restore := app.SetMemoryStdin(false, strings.NewReader(""))
	if code, _, errb := h.run(t, "memory", "recall", "all"); code == 0 || !strings.Contains(errb, "terminal") {
		t.Errorf("all, with no terminal: %d %s", code, errb)
	}
	if code, _, _ := h.run(t, "memory", "recall", "no-project"); code != 0 {
		t.Errorf("no-project needs no terminal: %d", code)
	}
	restore()
	if _, out, _ := h.run(t, "memory", "recall"); !strings.Contains(out, "not tied to a project") {
		t.Errorf("a refused all changed it: %s", out)
	}
	// In a terminal, Enter or yes takes it. Anything else changes nothing.
	for _, answer := range []string{"n\n", "no\n", "Not now\n", ""} {
		restore := app.SetMemoryStdin(true, strings.NewReader(answer))
		code, out, _ := h.run(t, "memory", "recall", "all")
		restore()
		if code != 0 || !strings.Contains(out, "Nothing changed.") || !strings.Contains(out, "not tied to a project") && strings.Contains(out, "all your projects") {
			t.Errorf("answer %q: %d %s", answer, code, out)
		}
		if _, now, _ := h.run(t, "memory", "recall"); !strings.Contains(now, "not tied to a project") {
			t.Errorf("answer %q changed it: %s", answer, now)
		}
	}
	defer app.SetMemoryStdin(true, strings.NewReader("y\n"))()
	if code, out, _ := h.run(t, "memory", "recall", "all"); code != 0 || !strings.Contains(out, "all your projects") {
		t.Errorf("all: %d %s", code, out)
	}
	if code, out, _ := h.run(t, "memory", "recall", "no-project"); code != 0 || !strings.Contains(out, "not tied to a project") {
		t.Errorf("no-project: %d %s", code, out)
	}
	if _, out, _ := h.run(t, "memory", "recall"); !strings.Contains(out, "not tied to a project") {
		t.Errorf("it did not stick: %s", out)
	}
	if code, _, _ := h.run(t, "memory", "recall", "maybe"); code != app.ExitUsage {
		t.Errorf("bad value: %d", code)
	}
}

// Capture is turned on only with Kameas Fleet's answer that it offers it: an
// unofficial build, a signed-out device, a failed read and an organization without
// memory_capture_kinds are all refused, and nothing is recorded.
func TestEnableRefusesWhatCannotBeConfirmed(t *testing.T) {
	withCapture(t)
	for _, c := range []struct {
		name string
		p    fleetapi.Provider
		want string
	}{
		{"unofficial", stub.New(), "isn't available in this build"},
		{"signed out, a writer that needs an account", &official{Provider: stub.New()}, "While you're signed out"},
		{"read fails", failingCapsProvider{orgProvider{&official{Provider: stub.New()}}}, "couldn't confirm"},
		{"not offered", signedInProvider(t), "isn't available for your organization yet"},
	} {
		h := newHarness(t, c.p)
		writers := []string{capture.WriterHostCLI, capture.WriterAPIKey, capture.WriterSessionAgent}
		if _, signedOut := c.p.(*official); signedOut {
			writers = []string{capture.WriterHostCLI, capture.WriterSessionAgent} // signed out, see TestSignedOutCapture
		}
		for _, w := range writers {
			restore := app.SetMemoryStdin(true, strings.NewReader("y"+"\n"))
			code, out, errb := h.run(t, "memory", "capture", "enable", "--writer", w)
			restore()
			if code == 0 || !strings.Contains(errb, c.want) || strings.Contains(out, "Turn on capture?") {
				t.Errorf("%s/%s: %d %s %s", c.name, w, code, out, errb)
			}
		}
		if b, _ := os.ReadFile(h.configFile()); strings.Contains(string(b), "memory_capture") {
			t.Errorf("%s recorded something: %s", c.name, b)
		}
		if _, err := os.Stat(h.capturePaths().Dir); err == nil {
			t.Errorf("%s: capture's directory was made", c.name)
		}
	}
	h := newHarness(t, stub.New())
	restore := app.SetMemoryStdin(true, strings.NewReader("y"+"\n"))
	defer restore()
	if _, out, _ := h.run(t, "memory", "capture", "status"); !strings.HasPrefix(out, "memory capture: off") {
		t.Errorf("status: %s", out)
	}
}

// The worker the hook starts carries nothing of the session's environment but its
// own settings.
func TestWorkerStartsWithAReducedEnvironment(t *testing.T) {
	got := capture.WorkerEnviron([]string{"PATH=/bin", "HOME=/h", "KENAZ_HOME=/k", "KENAZ_ENV=dev", "CLAUDE_CODE_OAUTH_TOKEN=x", "OPENAI_API_KEY=x", "OTEL_EXPORTER_OTLP_ENDPOINT=x", "GH_TOKEN=x"},
		capture.Settings{Writer: capture.WriterHostCLI}, app.KenazEnvVarForTest)
	joined := strings.Join(got, " ")
	for _, want := range []string{"PATH=/bin", "HOME=/h", "KENAZ_HOME=/k", "KENAZ_ENV=dev", capture.WorkerEnv + "=1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("lost %s: %s", want, joined)
		}
	}
	for _, bad := range []string{"OAUTH", "OPENAI", "OTEL", "GH_TOKEN"} {
		if strings.Contains(joined, bad) {
			t.Errorf("kept %s: %s", bad, joined)
		}
	}
}

// While capture is on the session gets the rich index alone, and it reads the
// signed-in account's store, which is where the worker writes.
func TestRichIndexReadsTheSignedInAccount(t *testing.T) {
	withCapture(t)
	p := signedInProvider(t)
	h := newHarness(t, p)
	h.captureOn(t)
	cwd := t.TempDir()
	proj, err := memory.ProjectFor(cwd)
	if err != nil {
		t.Fatal(err)
	}
	prof := accountProfile(t, p)
	st, err := memory.OpenProfile(memory.ProfilePath(filepath.Join(h.home, "plugin"), prof), prof)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SaveAuto(context.Background(), memory.AutoNote{ProjectKey: proj.Key, Title: "In the account", Body: "b", Source: memory.SourceCodex, Kind: memory.KindObservation, Tags: []string{memory.AutoTag}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	started := 0
	out := h.runCaptureHook(t, capture.HookIndex, map[string]any{"session_id": "s", "cwd": cwd}, &started)
	if !strings.Contains(out, "In the account") {
		t.Errorf("index: %q", out)
	}
	// The plain session-start index stays out of it while capture is on.
	restore := app.SetMemoryStdin(false, strings.NewReader(`{"session_id":"s","cwd":"`+cwd+`"}`))
	defer restore()
	_, plain, _ := h.run(t, "hook", "session-start")
	if strings.Contains(plain, "In the account") {
		t.Errorf("the session got both indexes: %s", plain)
	}
}

// CLI lists label automatic notes too.
func TestCLIListsLabelAutomaticNotes(t *testing.T) {
	h := newHarness(t, stub.New())
	st, err := memory.Open(h.memoryDB())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SaveAuto(context.Background(), memory.AutoNote{ProjectKey: memory.GlobalProject, Title: "Labelled", Body: "b", Source: memory.SourceCodex, Kind: memory.KindObservation, Tags: []string{memory.AutoTag}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	for _, args := range [][]string{{"memory", "list", "--project", "all"}, {"memory", "search", "Labelled", "--project", "all"}} {
		if _, out, errb := h.run(t, args...); !strings.Contains(out, "(auto, untrusted)") {
			t.Errorf("%v: %s %s", args, out, errb)
		}
	}
}

// failingCapsProvider is signed in, but its capabilities can't be read.
type failingCapsProvider struct{ orgProvider }

func (failingCapsProvider) Me() fleetapi.Me { return failingMe{} }

type failingMe struct{ me }

func (failingMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{}, errors.New("offline")
}

// orgProvider is signed in to one account, with everything an account profile needs.
type orgProvider struct{ *official }

func (p orgProvider) Auth() fleetapi.Auth { return orgAuth{auth{p.official}} }

type orgAuth struct{ auth }

func (a orgAuth) Session(context.Context) (fleetapi.Session, error) {
	return fleetapi.Session{Env: fleetapi.EnvDev, OrgID: "org-1", UserID: "user-1"}, nil
}

func signedInProvider(t *testing.T) fleetapi.Provider {
	t.Helper()
	return orgProvider{&official{Provider: stub.New()}}
}

func accountProfile(t *testing.T, _ fleetapi.Provider) memory.Profile {
	t.Helper()
	return memory.Profile{Env: string(fleetapi.EnvDev), OrgID: "org-1", UserID: "user-1"}
}

// The import asks for the capture opt-in, and a dry run writes nothing.
func TestMemoryImportCommand(t *testing.T) {
	withCapture(t)
	h := newHarness(t, stub.New())
	missing := filepath.Join(t.TempDir(), "none.db")
	if code, _, errb := h.run(t, "memory", "import", "claude-mem", "--dry-run", "--db", missing); code == 0 || !strings.Contains(errb, "no database") {
		t.Errorf("missing db: %d %s", code, errb)
	}
	if code, _, errb := h.run(t, "memory", "import", "claude-mem", "--yes", "--db", missing); code == 0 || !strings.Contains(errb, "capture") {
		t.Errorf("no opt-in: %d %s", code, errb)
	}
	if code, _, _ := h.run(t, "memory", "import", "claude-mem"); code != app.ExitUsage {
		t.Errorf("neither flag: %d", code)
	}
	if code, _, _ := h.run(t, "memory", "import", "something-else"); code != app.ExitUsage {
		t.Errorf("unknown tool: %d", code)
	}
}

// Comparing needs nothing of the other tool's.
func TestCaptureCompareWithoutTheOtherTool(t *testing.T) {
	withCapture(t)
	h := newHarness(t, stub.New())
	code, out, _ := h.run(t, "memory", "capture", "compare")
	if code != 0 || !strings.Contains(out, "No other memory tool") {
		t.Errorf("%d %s", code, out)
	}
}

// The desktop extension's setting wins there: its member may have no program to run.
func TestDesktopRecallSetting(t *testing.T) {
	h := newHarness(t, stub.New())
	run := func(v string) string {
		t.Helper()
		var out, errb bytes.Buffer
		env := h.env(context.Background(), &out, &errb)
		get := env.Getenv
		env.Getenv = func(k string) string {
			if k == variant.Shared.MemoryRecallVar {
				return v
			}
			return get(k)
		}
		if code := app.RunEnv(env, []string{"memory", "recall"}); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		return out.String()
	}
	if out := run("all"); !strings.Contains(out, "all your projects") {
		t.Errorf("all: %s", out)
	}
	if out := run("${user_config.memory_chat_recall}"); !strings.Contains(out, "not tied to a project") {
		t.Errorf("an unfilled value must not widen it: %s", out)
	}
	if out := run(""); !strings.Contains(out, "not tied to a project") {
		t.Errorf("neither set: %s", out)
	}
	defer app.SetMemoryStdin(true, strings.NewReader("y\n"))()
	// If the two disagree, the narrower one wins.
	if code, _, _ := h.run(t, "memory", "recall", "no-project"); code != 0 {
		t.Fatal(code)
	}
	if out := run("all"); !strings.Contains(out, "not tied to a project") {
		t.Errorf("extension all, command no-project: %s", out)
	}
	if code, _, _ := h.run(t, "memory", "recall", "all"); code != 0 {
		t.Fatal(code)
	}
	if out := run(""); !strings.Contains(out, "all your projects") {
		t.Errorf("command all, extension unset: %s", out)
	}
	if out := run("no-project"); !strings.Contains(out, "not tied to a project") {
		t.Errorf("command all, extension no-project: %s", out)
	}
}

// acctProvider is an official build that a login signs in to an organization, whose
// capabilities are keys.
type acctProvider struct {
	*official
	keys map[string]bool
}

func (p acctProvider) Auth() fleetapi.Auth { return acctAuth{auth{p.official}} }
func (p acctProvider) Me() fleetapi.Me     { return keysMe{keys: p.keys} }

type acctAuth struct{ auth }

func (a acctAuth) Session(context.Context) (fleetapi.Session, error) {
	if a.p.signedIn.Load() {
		return fleetapi.Session{Env: fleetapi.EnvDev, OrgID: "org-1", UserID: "user-1"}, nil
	}
	return fleetapi.Session{}, fleetapi.ErrNotSignedIn
}

type keysMe struct {
	me
	keys map[string]bool
}

func (m keysMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: m.keys}, nil
}

// Signed out there is no organization: capture with the member's own API key or a
// model on this device runs, once turned on signed out. Writers that need an account
// don't. Signing in to an organization without memory_capture_kinds stops it at
// once: nothing queued signed out carries over, and nothing runs for the account.
func TestSignedOutCapture(t *testing.T) {
	withCapture(t)
	for _, args := range [][]string{{"--writer", capture.WriterAPIKey}, {"--writer", capture.WriterLocal, "--command", "my-model"}} {
		p := acctProvider{&official{Provider: stub.New()}, map[string]bool{fleetapi.CapMemorySync: true}}
		h := newHarness(t, p)
		restore := app.SetMemoryStdin(true, strings.NewReader("y\n"))
		code, _, errb := h.run(t, append([]string{"memory", "capture", "enable"}, args...)...)
		restore()
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, errb)
		}
		var out, errb2 bytes.Buffer
		env := h.env(context.Background(), &out, &errb2)
		if offered, reason, profile := app.CaptureOfferedForTest(env); !offered || profile != "local" {
			t.Fatalf("%v: signed out: %v %q %q", args, offered, reason, profile)
		}
		if _, out, _ := h.run(t, "memory", "capture", "status"); !strings.HasPrefix(out, "memory capture: on ("+args[1]+")") || strings.Contains(out, "not running") {
			t.Errorf("%v: status: %s", args, out)
		}
		cwd := t.TempDir()
		started := 0
		h.runCaptureHook(t, capture.HookTool, map[string]any{"session_id": "s", "cwd": cwd, "tool_name": "Edit"}, &started)
		if capture.QueueLen(h.capturePaths()) != 1 {
			t.Fatalf("%v: signed out, nothing queued", args)
		}
		// Signing in to an organization without the key: the queue goes, nothing new
		// is queued, and the worker hears no.
		if code, _, errb := h.run(t, "login", "--no-browser"); code != 0 {
			t.Fatalf("login: %d %s", code, errb)
		}
		if capture.QueueLen(h.capturePaths()) != 0 {
			t.Errorf("%v: what was queued signed out survived the sign-in", args)
		}
		h.runCaptureHook(t, capture.HookTool, map[string]any{"session_id": "s", "cwd": cwd, "tool_name": "Edit"}, &started)
		if capture.QueueLen(h.capturePaths()) != 0 {
			t.Errorf("%v: queued for an account without the key", args)
		}
		env = h.env(context.Background(), &out, &errb2)
		if offered, reason, profile := app.CaptureOfferedForTest(env); offered || reason != capture.PauseNotOffered || profile == "local" {
			t.Errorf("%v: signed in without the key: %v %q %q", args, offered, reason, profile)
		}
	}
}

// Signed out, the writers that need an account are refused, and an account's capture
// doesn't carry on signed out: after logout it runs again only once it is turned on
// signed out.
func TestAccountCaptureStopsAtSignOut(t *testing.T) {
	withCapture(t)
	p := acctProvider{&official{Provider: stub.New()}, map[string]bool{fleetapi.CapMemorySync: true, fleetapi.CapMemoryCaptureKinds: true}}
	h := newHarness(t, p)
	if code, _, errb := h.run(t, "login", "--no-browser"); code != 0 {
		t.Fatalf("login: %d %s", code, errb)
	}
	restore := app.SetMemoryStdin(true, strings.NewReader("y\n"))
	code, _, errb := h.run(t, "memory", "capture", "enable", "--writer", capture.WriterAPIKey)
	restore()
	if code != 0 {
		t.Fatalf("enable: %d %s", code, errb)
	}
	var out, errb2 bytes.Buffer
	if offered, _, profile := app.CaptureOfferedForTest(h.env(context.Background(), &out, &errb2)); !offered || profile == "local" {
		t.Fatalf("signed in with the key: %v %q", offered, profile)
	}
	if code, _, errb := h.run(t, "logout"); code != 0 {
		t.Fatalf("logout: %d %s", code, errb)
	}
	if offered, reason, _ := app.CaptureOfferedForTest(h.env(context.Background(), &out, &errb2)); offered || reason != capture.PauseSignedOutWriter {
		t.Errorf("after logout: %v %q", offered, reason)
	}
	started := 0
	h.runCaptureHook(t, capture.HookTool, map[string]any{"session_id": "s", "cwd": t.TempDir(), "tool_name": "Edit"}, &started)
	if capture.QueueLen(h.capturePaths()) != 0 {
		t.Error("queued after logout")
	}
	restore = app.SetMemoryStdin(true, strings.NewReader("y\n"))
	defer restore()
	if code, _, errb := h.run(t, "memory", "capture", "enable", "--writer", capture.WriterHostCLI); code == 0 || !strings.Contains(errb, "While you're signed out") {
		t.Errorf("host-cli signed out: %d %s", code, errb)
	}
}
