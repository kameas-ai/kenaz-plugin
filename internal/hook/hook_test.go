package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
)

type fakeDaemon struct {
	mu      sync.Mutex
	ensure  time.Duration
	ensured int
	ends    []daemonstate.SessionEnd
}

func (f *fakeDaemon) Ensure(ctx context.Context) error {
	f.mu.Lock()
	f.ensured++
	f.mu.Unlock()
	select {
	case <-time.After(f.ensure):
	case <-ctx.Done():
	}
	return nil
}

func (f *fakeDaemon) ensureCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensured
}

func (f *fakeDaemon) SessionEnd(_ context.Context, e daemonstate.SessionEnd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, e)
	return nil
}

type banners []string

func (b banners) Banners(context.Context) []string { return b }

type skills bool

func (s skills) ChangedSinceLastScan(context.Context) bool { return bool(s) }

func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

var claudeEnv = env("CLAUDE_PLUGIN_ROOT", "/p")
var codexEnv = env("CLAUDE_PLUGIN_ROOT", "/p", "PLUGIN_ROOT", "/p")

func start(t *testing.T, deps Deps) map[string]any {
	t.Helper()
	var out bytes.Buffer
	Run(context.Background(), EventSessionStart, strings.NewReader(`{"session_id":"s"}`), &out, deps)
	if out.Len() == 0 {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("output is not one JSON object: %q", out.String())
	}
	return v
}

func hso(v map[string]any) map[string]any {
	return v["hookSpecificOutput"].(map[string]any)
}

func TestDetectHarness(t *testing.T) {
	if DetectHarness(claudeEnv) != HarnessClaude || DetectHarness(codexEnv) != HarnessCodex || DetectHarness(env()) != HarnessUnknown {
		t.Fatal("misdetected harness")
	}
}

// The hook never blocks past its deadline, whatever the daemon does.
func TestSessionStartNeverBlocks(t *testing.T) {
	d := &fakeDaemon{ensure: 10 * time.Second}
	t0 := time.Now()
	v := start(t, Deps{Daemon: d, Banners: banners{"x"}, Getenv: claudeEnv, StartDeadline: 200 * time.Millisecond})
	if el := time.Since(t0); el > time.Second {
		t.Fatalf("hook took %v", el)
	}
	if v != nil {
		t.Fatalf("expected no output after deadline, got %v", v)
	}
	if d.ensureCount() != 1 {
		t.Fatal("daemon.Ensure not called")
	}
}

func TestSessionStartQuietWhenNothingToSay(t *testing.T) {
	if v := start(t, Deps{Daemon: &fakeDaemon{}, Getenv: claudeEnv}); v != nil {
		t.Fatalf("unexpected output %v", v)
	}
}

func TestSessionStartClaude(t *testing.T) {
	v := start(t, Deps{Banners: banners{"Not signed in. Run /kenaz:login.", " "}, Skills: skills(true), Getenv: claudeEnv})
	h := hso(v)
	if h["hookEventName"] != "SessionStart" || h["reloadSkills"] != true {
		t.Fatalf("got %v", h)
	}
	if got := h["additionalContext"]; got != "Kameas Fleet: Not signed in. Run /kenaz:login." {
		t.Fatalf("context %q", got)
	}
}

// Codex rejects unknown SessionStart output fields, so reloadSkills must
// never be sent to it, and nothing at all when only skills changed.
func TestSessionStartCodexNoReloadSkills(t *testing.T) {
	if v := start(t, Deps{Skills: skills(true), Getenv: codexEnv}); v != nil {
		t.Fatalf("codex got output for skills-only change: %v", v)
	}
	v := start(t, Deps{Banners: banners{"Org paused"}, Skills: skills(true), Getenv: codexEnv})
	h := hso(v)
	if _, ok := h["reloadSkills"]; ok {
		t.Fatal("reloadSkills sent to codex")
	}
	for k := range h {
		if k != "hookEventName" && k != "additionalContext" {
			t.Fatalf("unexpected codex field %q", k)
		}
	}
	for k := range v {
		if k != "hookSpecificOutput" {
			t.Fatalf("unexpected top-level field %q", k)
		}
	}
}

func TestSessionStartUnknownHarnessIsConservative(t *testing.T) {
	v := start(t, Deps{Banners: banners{"b"}, Skills: skills(true), Getenv: env()})
	if _, ok := hso(v)["reloadSkills"]; ok {
		t.Fatal("reloadSkills sent to unknown harness")
	}
}

func TestContextTruncated(t *testing.T) {
	v := start(t, Deps{Banners: banners{strings.Repeat("é", 1000)}, Getenv: claudeEnv})
	s := hso(v)["additionalContext"].(string)
	if len(s) > MaxContext || !strings.HasSuffix(s, "…") {
		t.Fatalf("len %d", len(s))
	}
}

func TestSessionEndSendsHashOnly(t *testing.T) {
	d := &fakeDaemon{}
	var out bytes.Buffer
	Run(context.Background(), EventSessionEnd, strings.NewReader(`{"session_id":"secret-session","reason":"prompt_input_exit","cwd":"/home/u/repo"}`), &out, Deps{Daemon: d, Getenv: claudeEnv})
	if out.Len() != 0 {
		t.Fatalf("session-end wrote %q", out.String())
	}
	if len(d.ends) != 1 {
		t.Fatalf("ends %v", d.ends)
	}
	e := d.ends[0]
	if e.SessionHash != daemonstate.SessionHash("secret-session") || e.Reason != "prompt_input_exit" || e.Harness != "claude-code" {
		t.Fatalf("got %+v", e)
	}
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "secret-session") || strings.Contains(string(b), "/home/u") {
		t.Fatalf("raw data leaked: %s", b)
	}
}

func TestSessionEndBoundsReasonAndIgnoresJunk(t *testing.T) {
	d := &fakeDaemon{}
	Run(context.Background(), EventSessionEnd, strings.NewReader(`{"session_id":"s","reason":"user typed something"}`), &bytes.Buffer{}, Deps{Daemon: d})
	if d.ends[0].Reason != "other" {
		t.Fatalf("reason %q", d.ends[0].Reason)
	}
	Run(context.Background(), EventSessionEnd, strings.NewReader(`not json`), &bytes.Buffer{}, Deps{Daemon: d})
	Run(context.Background(), EventSessionEnd, nil, &bytes.Buffer{}, Deps{Daemon: d})
	if len(d.ends) != 1 {
		t.Fatal("junk input reached the daemon")
	}
}

// A stdin that never closes must not hold the hook past its deadline.
func TestSessionEndStdinNeverCloses(t *testing.T) {
	r, w, _ := os.Pipe()
	defer w.Close()
	t0 := time.Now()
	Run(context.Background(), EventSessionEnd, r, &bytes.Buffer{}, Deps{Daemon: &fakeDaemon{}, EndDeadline: 100 * time.Millisecond})
	if time.Since(t0) > time.Second {
		t.Fatal("blocked on stdin")
	}
}

func TestUnknownEventIsNoop(t *testing.T) {
	var out bytes.Buffer
	Run(context.Background(), "pre-tool-use", strings.NewReader(`{}`), &out, Deps{Banners: banners{"x"}})
	if out.Len() != 0 {
		t.Fatal("unknown event produced output")
	}
}

type fakeMemory struct {
	index string
	mu    sync.Mutex
	cwd   string
}

func (m *fakeMemory) Index(_ context.Context, cwd string) string {
	m.mu.Lock()
	m.cwd = cwd
	m.mu.Unlock()
	return m.index
}

func startWith(t *testing.T, stdin string, deps Deps) map[string]any {
	t.Helper()
	var out bytes.Buffer
	Run(context.Background(), EventSessionStart, strings.NewReader(stdin), &out, deps)
	if out.Len() == 0 {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("output is not one JSON object: %q", out.String())
	}
	return v
}

// The memory index is appended after the Fleet banners, gets the
// session's cwd from stdin, and is sent to both harnesses.
func TestSessionStartMemoryIndex(t *testing.T) {
	idx := "Kenaz memory index for this project:\n- t [abc, 2026-10-20]"
	for _, g := range []func(string) string{claudeEnv, codexEnv} {
		m := &fakeMemory{index: idx}
		v := startWith(t, `{"session_id":"s","cwd":"/home/u/repo"}`, Deps{Banners: banners{"Org paused"}, Memory: m, Getenv: g})
		got := hso(v)["additionalContext"]
		if got != "Kameas Fleet: Org paused\n\n"+idx {
			t.Fatalf("context %q", got)
		}
		if m.cwd != "/home/u/repo" {
			t.Fatalf("cwd %q", m.cwd)
		}
	}
	// Memory alone still produces output; no memory, no output.
	if v := startWith(t, `{}`, Deps{Memory: &fakeMemory{index: idx}, Getenv: claudeEnv}); hso(v)["additionalContext"] != idx {
		t.Fatalf("memory-only output %v", v)
	}
	if v := startWith(t, `{}`, Deps{Memory: &fakeMemory{}, Getenv: claudeEnv}); v != nil {
		t.Fatalf("empty memory produced %v", v)
	}
	// An oversized index is dropped, never truncated mid-entry.
	if v := startWith(t, `{}`, Deps{Memory: &fakeMemory{index: strings.Repeat("x", MaxMemoryContext+1)}, Getenv: claudeEnv}); v != nil {
		t.Fatalf("oversized index produced %v", v)
	}
}

// A stdin that never closes delays the memory index by at most stdinWait.
func TestSessionStartMemoryStdinNeverCloses(t *testing.T) {
	r, w, _ := os.Pipe()
	defer w.Close()
	var out bytes.Buffer
	t0 := time.Now()
	Run(context.Background(), EventSessionStart, r, &out, Deps{Memory: &fakeMemory{index: "idx"}, Getenv: claudeEnv})
	if time.Since(t0) > 2*time.Second || !strings.Contains(out.String(), "idx") {
		t.Fatalf("took %v, out %q", time.Since(t0), out.String())
	}
}

// The staff build's tag prefixes the whole session-start context, also
// when only the memory index is shown (issue #34 F6).
func TestSessionStartTag(t *testing.T) {
	idx := "Kenaz memory index for this project:\n- t [abc, 2026-10-20]"
	tag := "[kenaz-dev · dev]"
	if v := startWith(t, `{}`, Deps{Memory: &fakeMemory{index: idx}, Tag: tag, Getenv: claudeEnv}); hso(v)["additionalContext"] != tag+" "+idx {
		t.Fatalf("memory only: %v", v)
	}
	if v := startWith(t, `{}`, Deps{Banners: banners{"Org paused"}, Memory: &fakeMemory{index: idx}, Tag: tag, Getenv: codexEnv}); hso(v)["additionalContext"] != tag+" Kameas Fleet: Org paused\n\n"+idx {
		t.Fatalf("banner and memory: %v", v)
	}
	if v := startWith(t, `{}`, Deps{Memory: &fakeMemory{}, Tag: tag, Getenv: claudeEnv}); v != nil {
		t.Fatalf("a tag alone produced %v", v)
	}
}
