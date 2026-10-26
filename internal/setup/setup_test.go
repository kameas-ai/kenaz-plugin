package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

var update = flag.Bool("update", false, "rewrite golden files")

type env struct {
	opts     Options
	out, err *bytes.Buffer
}

// newEnv builds Options rooted in temp dirs. It never touches the real
// ~/.claude, ~/.codex or managed-settings paths.
func newEnv(t *testing.T, port int) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	e.opts = Options{
		ClaudeDir:         filepath.Join(root, "home", ".claude"),
		CodexDir:          filepath.Join(root, "home", ".codex"),
		StateDir:          filepath.Join(root, "state"),
		ClaudeManagedPath: filepath.Join(root, "etc", "claude-managed.json"),
		CodexManagedPath:  filepath.Join(root, "etc", "codex-managed.toml"),
		Now:               func() time.Time { clock = clock.Add(time.Second); return clock },
		In:                strings.NewReader(""),
		Out:               e.out,
		Err:               e.err,
		Yes:               true,
	}
	e.setPort(t, port)
	return e
}

func (e *env) setPort(t *testing.T, port int) {
	t.Helper()
	os.MkdirAll(e.opts.StateDir, 0o700)
	b, _ := json.Marshal(daemonstate.State{PID: 1, Port: port, Nonce: "n"})
	if err := os.WriteFile(filepath.Join(e.opts.StateDir, daemonstate.FileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *env) seed(t *testing.T, h harness, fixture string) []byte {
	t.Helper()
	if fixture == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	p := e.opts.path(h)
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) read(t *testing.T, h harness) ([]byte, bool) {
	t.Helper()
	b, ok, err := readFile(e.opts.path(h))
	if err != nil {
		t.Fatal(err)
	}
	return b, ok
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		os.WriteFile(path, got, 0o644)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch:\n%s", name, unifiedDiff("want", "got", string(want), string(got)))
	}
}

// expected output for each fixture, and --undo restores the input
// byte for byte.
func TestGolden(t *testing.T) {
	cases := []struct {
		h       harness
		fixture string
		golden  string
		traces  bool
	}{
		{harnessClaude, "", "claude/absent", false},
		{harnessClaude, "claude/sigil.json", "claude/sigil", false},
		{harnessClaude, "claude/permissions.json", "claude/permissions", false},
		{harnessClaude, "claude/permissions.json", "claude/permissions-traces", true},
		{harnessCodex, "", "codex/absent", false},
		{harnessCodex, "codex/comments.toml", "codex/comments", false},
		{harnessCodex, "codex/otel-loopback.toml", "codex/otel-loopback", false},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			e := newEnv(t, 4318)
			e.opts.Traces = c.traces
			in := e.seed(t, c.h, c.fixture)
			if err := Apply(e.opts, c.h); err != nil {
				t.Fatal(err)
			}
			got, _ := e.read(t, c.h)
			golden(t, c.golden, got)

			if err := Undo(e.opts, c.h); err != nil {
				t.Fatal(err)
			}
			after, exists := e.read(t, c.h)
			if c.fixture == "" {
				if exists {
					t.Fatalf("undo left a file setup created:\n%s", after)
				}
			} else if !bytes.Equal(after, in) {
				t.Fatalf("undo is not byte-identical:\n%s", unifiedDiff("in", "after", string(in), string(after)))
			}
			if _, err := os.Stat(filepath.Join(e.opts.StateDir, "setup")); err == nil {
				entries, _ := os.ReadDir(filepath.Join(e.opts.StateDir, "setup"))
				if len(entries) != 0 {
					t.Fatalf("snapshot left behind: %v", entries)
				}
			}
		})
	}
}

// Another tool may seed sigil hooks; setup must keep them.
func TestSigilHooksPreserved(t *testing.T) {
	e := newEnv(t, 4318)
	in := e.seed(t, harnessClaude, "claude/sigil.json")
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	got, _ := e.read(t, harnessClaude)
	var a, b map[string]json.RawMessage
	json.Unmarshal(in, &a)
	json.Unmarshal(got, &b)
	if !sameJSON(a["hooks"], b["hooks"]) {
		t.Fatal("hooks changed")
	}
}

// a second apply produces no diff and no backup.
func TestIdempotent(t *testing.T) {
	for _, h := range []harness{harnessClaude, harnessCodex} {
		e := newEnv(t, 4318)
		fixture := "claude/permissions.json"
		if h == harnessCodex {
			fixture = "codex/comments.toml"
		}
		e.seed(t, h, fixture)
		if err := Apply(e.opts, h); err != nil {
			t.Fatal(err)
		}
		first, _ := e.read(t, h)
		backups, _ := filepath.Glob(e.opts.path(h) + backupInfix + "*")
		e.out.Reset()
		if err := Apply(e.opts, h); err != nil {
			t.Fatal(err)
		}
		second, _ := e.read(t, h)
		backups2, _ := filepath.Glob(e.opts.path(h) + backupInfix + "*")
		if !bytes.Equal(first, second) || len(backups2) != len(backups) || len(backups) != 1 {
			t.Fatalf("%v: not idempotent (backups %d → %d)", h.label(), len(backups), len(backups2))
		}
		if !strings.Contains(e.out.String(), "already configured") || strings.Contains(e.out.String(), "@@") {
			t.Fatalf("second run printed a diff: %s", e.out)
		}
	}
}

// invalid files and foreign exporters abort with no write.
func TestRefusals(t *testing.T) {
	cases := []struct {
		h       harness
		content string
		fixture string
		want    string
	}{
		{h: harnessClaude, content: `{"env": `, want: "not valid JSON"},
		{h: harnessClaude, content: `[1,2]`, want: "not valid JSON"},
		{h: harnessClaude, content: `{"env": "x"}`, want: "not an object"},
		{h: harnessClaude, content: `{"env": {"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.corp:4318"}}`, want: "not this machine"},
		{h: harnessClaude, content: `{"env": {"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:1"}}`, want: "override the relay"},
		{h: harnessCodex, fixture: "codex/nonloopback.toml", want: "not this machine"},
		{h: harnessCodex, fixture: "codex/subtable.toml", want: "sub-tables are not supported"},
		{h: harnessCodex, content: "otel.exporter = \"none\"\n", want: "root-level"},
		{h: harnessCodex, content: "[otel]\n[otel]\n", want: "duplicate"},
	}
	for _, c := range cases {
		e := newEnv(t, 4318)
		var in []byte
		if c.fixture != "" {
			in = e.seed(t, c.h, c.fixture)
		} else {
			in = []byte(c.content)
			os.MkdirAll(filepath.Dir(e.opts.path(c.h)), 0o700)
			os.WriteFile(e.opts.path(c.h), in, 0o600)
		}
		err := Apply(e.opts, c.h)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want error containing %q", c.content+c.fixture, err, c.want)
		}
		got, _ := e.read(t, c.h)
		if !bytes.Equal(got, in) {
			t.Errorf("%q: file was modified", c.content+c.fixture)
		}
		if _, err := os.Stat(filepath.Join(e.opts.StateDir, stateFile)); err == nil {
			t.Errorf("%q: state written on refusal", c.content+c.fixture)
		}
	}
}

func TestNoDaemon(t *testing.T) {
	e := newEnv(t, 4318)
	os.Remove(filepath.Join(e.opts.StateDir, daemonstate.FileName))
	if err := Apply(e.opts, harnessClaude); err == nil || !strings.Contains(err.Error(), "has not started") {
		t.Fatalf("got %v", err)
	}
	if _, ok := e.read(t, harnessClaude); ok {
		t.Fatal("wrote without a port")
	}
}

// confirmation rules.
func TestConfirm(t *testing.T) {
	e := newEnv(t, 4318)
	e.opts.Yes = false
	e.seed(t, harnessClaude, "claude/sigil.json")
	in, _ := e.read(t, harnessClaude)

	if err := Apply(e.opts, harnessClaude); !errors.Is(err, ErrDeclined) {
		t.Fatalf("non-TTY without --yes: %v", err)
	}
	e.opts.Interactive = true
	e.opts.In = strings.NewReader("n\n")
	if err := Apply(e.opts, harnessClaude); !errors.Is(err, ErrDeclined) {
		t.Fatalf("answered no: %v", err)
	}
	e.opts.DryRun = true
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.read(t, harnessClaude); !bytes.Equal(got, in) {
		t.Fatal("declined or dry run modified the file")
	}
	if !strings.Contains(e.out.String(), "+    \"CLAUDE_CODE_ENABLE_TELEMETRY\": \"1\"") {
		t.Fatalf("diff missing from output:\n%s", e.out)
	}
	e.opts.DryRun = false
	e.opts.In = strings.NewReader("y\n")
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.read(t, harnessClaude); bytes.Equal(got, in) {
		t.Fatal("answered yes but nothing written")
	}
}

// undo restores owned keys and leaves the user's later edits.
func TestUndoKeepsUserChanges(t *testing.T) {
	e := newEnv(t, 4318)
	e.seed(t, harnessClaude, "claude/permissions.json")
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.read(t, harnessClaude)
	obj, _ := parseObject(cur)
	obj.set("theme", json.RawMessage(`"dark"`))
	envRaw, _ := obj.get("env")
	envObj, _ := parseObject(envRaw)
	envObj.set("OTEL_LOG_TOOL_CONTENT", json.RawMessage(`"1"`))
	envRaw, _ = envObj.render("")
	obj.set("env", envRaw)
	edited, _ := obj.render("")
	os.WriteFile(e.opts.path(harnessClaude), edited, 0o600)

	if err := Undo(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.err.String(), "OTEL_LOG_TOOL_CONTENT changed since setup") {
		t.Fatalf("no warning: %s", e.err)
	}
	got, _ := e.read(t, harnessClaude)
	var m struct {
		Theme string
		Env   map[string]string
	}
	json.Unmarshal(got, &m)
	if m.Theme != "dark" || m.Env["OTEL_LOG_TOOL_CONTENT"] != "1" {
		t.Fatalf("user edits lost: %s", got)
	}
	if _, ok := m.Env["CLAUDE_CODE_ENABLE_TELEMETRY"]; ok {
		t.Fatal("owned key not removed")
	}
	if m.Env["OTEL_LOG_TOOL_DETAILS"] != "1" || m.Env["OTEL_LOG_USER_PROMPTS"] != "false" || m.Env["ANTHROPIC_SMALL_FAST_MODEL"] != "haiku" {
		t.Fatalf("prior values not restored: %v", m.Env)
	}

	// Codex: a user edit elsewhere in the file survives undo.
	e = newEnv(t, 4318)
	e.seed(t, harnessCodex, "codex/otel-loopback.toml")
	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	cur, _ = e.read(t, harnessCodex)
	os.WriteFile(e.opts.path(harnessCodex), []byte(strings.Replace(string(cur), `model = "gpt-5-mini"`, `model = "gpt-5"`, 1)), 0o600)
	if err := Undo(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	got, _ = e.read(t, harnessCodex)
	want := strings.Replace(string(e.seedBytes(t, "codex/otel-loopback.toml")), `model = "gpt-5-mini"`, `model = "gpt-5"`, 1)
	if string(got) != want {
		t.Fatalf("codex undo:\n%s", unifiedDiff("want", "got", want, string(got)))
	}
}

func (e *env) seedBytes(t *testing.T, fixture string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// a port change rewrites the endpoints and undo still returns to the
// original file.
func TestPortChangeAndBackupPruning(t *testing.T) {
	e := newEnv(t, 4318)
	in := e.seed(t, harnessCodex, "codex/comments.toml")
	for _, p := range []int{4318, 47318, 50000, 50001, 50002} {
		e.setPort(t, p)
		if err := Apply(e.opts, harnessCodex); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := e.read(t, harnessCodex)
	if !strings.Contains(string(got), "127.0.0.1:50002/v1/logs") || strings.Contains(string(got), ":4318") {
		t.Fatalf("endpoint not updated:\n%s", got)
	}
	backups, _ := filepath.Glob(e.opts.path(harnessCodex) + backupInfix + "*")
	if len(backups) != keepBackups {
		t.Fatalf("%d backups kept", len(backups))
	}
	if err := Undo(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	if after, _ := e.read(t, harnessCodex); !bytes.Equal(after, in) {
		t.Fatal("undo after port changes is not byte-identical")
	}
}

func TestManagedSnippets(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	b, err := ManagedClaude(4318)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ExtraKnownMarketplaces map[string]struct {
			Source     map[string]string
			AutoUpdate bool
		}
		EnabledPlugins map[string]bool
		Env            map[string]string
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	mk := m.ExtraKnownMarketplaces["kenaz"]
	if mk.Source["repo"] != "kameas-ai/kenaz-plugin" || mk.Source["source"] != "github" || !mk.AutoUpdate {
		t.Fatalf("marketplace %+v", mk)
	}
	if !m.EnabledPlugins["kenaz@kenaz"] {
		t.Fatal("plugin not enabled")
	}
	if m.Env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://127.0.0.1:4318" || m.Env["OTEL_LOG_USER_PROMPTS"] != "0" || m.Env["OTEL_LOGS_EXPORTER"] != "otlp" {
		t.Fatalf("env %v", m.Env)
	}
	var top map[string]json.RawMessage
	json.Unmarshal(b, &top)
	for k := range top {
		if k != "extraKnownMarketplaces" && k != "enabledPlugins" && k != "env" {
			t.Fatalf("managed snippet sets %q", k)
		}
	}

	c := ManagedCodex(47318)
	sec, err := scanOtel(splitLines(c), codexOwned)
	if err != nil || len(sec.entries) != len(codexOwned) || !strings.Contains(c, "127.0.0.1:47318/v1/traces") {
		t.Fatalf("codex snippet:\n%s", c)
	}

	var out bytes.Buffer
	if code := printManaged(&out, harnessClaude, 4318, true); code != 0 || !bytes.Equal(out.Bytes(), b) {
		t.Fatal("raw claude snippet differs")
	}
	out.Reset()
	printManaged(&out, harnessClaude, 4318, false)
	if !strings.Contains(out.String(), "2.1.217") || !strings.Contains(out.String(), "Claude Desktop") {
		t.Fatal("notes missing")
	}
}

func TestCommandParsing(t *testing.T) {
	var out, errb bytes.Buffer
	if OfficialRepo == "" {
		// A build with no public marketplace has nothing to deploy.
		for _, h := range []string{"claude", "codex"} {
			if code := Command(context.Background(), []string{"print-managed", h}, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "not available in this build") {
				t.Fatalf("print-managed %s: code %d: %s", h, code, errb.String())
			}
		}
		return
	}
	if code := Command(context.Background(), []string{"print-managed", "codex", "--raw", "--port", "47318"}, &out, &errb); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "[otel]\n") || !strings.Contains(out.String(), ":47318/") {
		t.Fatalf("got %q", out.String())
	}
	for _, args := range [][]string{{}, {"bogus"}, {"claude", "codex"}, {"print-managed", "x"}} {
		if code := Command(context.Background(), args, &out, &errb); code != exitUsage {
			t.Errorf("%v: code %d", args, code)
		}
	}
}

func TestDoctor(t *testing.T) {
	e := newEnv(t, 4318)
	e.seed(t, harnessClaude, "claude/sigil.json")
	e.seed(t, harnessCodex, "codex/nonloopback.toml")
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, name string, args ...string) (string, error) {
		switch name + " " + strings.Join(args, " ") {
		case "claude --version":
			return "2.1.300 (Claude Code)\n", nil
		case "codex --version":
			return "codex-cli 0.145.0\n", nil
		case "claude plugin list --json":
			return `[{"id":"` + tID + `"}]`, nil
		}
		return "", errors.New("not found")
	}
	cs := Doctor(context.Background(), e.opts, run)
	has := func(name, status, substr string) bool {
		for _, c := range cs {
			if c.Name == name && c.Status == status && strings.Contains(c.Detail, substr) {
				return true
			}
		}
		return false
	}
	for _, want := range []struct{ name, status, substr string }{
		{"relay", "warn", "not answering"},
		{"Claude Code", "ok", "exports to the relay"},
		{"Claude Code", "ok", "plugin " + variant.Current.Plugin + " is installed"},
		{"Codex", "warn", "older than the supported minimum 0.147.0"},
		{"Codex", "fail", "https://otel.example.com:4317"},
	} {
		if !has(want.name, want.status, want.substr) {
			t.Errorf("missing %+v in %+v", want, cs)
		}
	}
	if code := printDoctor(&bytes.Buffer{}, cs); code != exitError {
		t.Fatal("fail check did not fail doctor")
	}
}

func TestDiff(t *testing.T) {
	if d := unifiedDiff("a", "b", "x\ny\n", "x\ny\n"); d != "" {
		t.Fatalf("equal inputs diffed: %q", d)
	}
	d := unifiedDiff("a", "b", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\n2\n3\n4\nFIVE\n6\n7\n8\n9\n")
	want := "--- a\n+++ b\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+FIVE\n 6\n 7\n 8\n"
	if d != want {
		t.Fatalf("got\n%s\nwant\n%s", d, want)
	}
}
