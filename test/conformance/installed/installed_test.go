package installed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox puts every path under one temporary directory, hands no
// credential or host setting to children, and refuses a layout that
// reaches outside it.
func TestSandboxIsContained(t *testing.T) {
	for k, v := range map[string]string{
		"ANTHROPIC_API_KEY": "planted", "OPENAI_API_KEY": "planted", "CLAUDE_CODE_OAUTH_TOKEN": "planted",
		"KENAZ_ENV": "dev", "CLAUDE_CONFIG_DIR": "/somewhere/real", "CODEX_HOME": "/somewhere/real", "KENAZ_HOME": "/somewhere/real",
		"GITHUB_TOKEN": "planted", "MY_SERVICE_PASSWORD": "planted",
	} {
		t.Setenv(k, v)
	}
	s, err := newSandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for _, p := range []string{s.home, s.kenazHome, s.claudeDir, s.codexDir, s.work, s.tmp, s.bundle, s.exe} {
		if !within(p, s.root) || p == s.root {
			t.Errorf("%s is not inside %s", p, s.root)
		}
	}
	for _, kv := range s.env {
		if strings.Contains(kv, "planted") || strings.Contains(kv, "/somewhere/real") {
			t.Errorf("child environment carries %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	for _, k := range []string{"HOME", "USERPROFILE", "KENAZ_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA", "TMPDIR"} {
		if v := s.getenv(k); !within(v, s.root) {
			t.Errorf("$%s = %q is outside the sandbox", k, v)
		}
	}
	if s.getenv("KENAZ_ENV") != "" || s.getenv("ANTHROPIC_API_KEY") != "" {
		t.Error("a Fleet env or credential reaches children")
	}

	// A layout that reaches outside is refused.
	real, _ := os.UserHomeDir()
	for name, mutate := range map[string]func(s *sandbox, over map[string]string){
		"real home":         func(s *sandbox, _ map[string]string) { s.home = real },
		"real ~/.claude":    func(s *sandbox, _ map[string]string) { s.claudeDir = filepath.Join(real, ".claude") },
		"real ~/.codex":     func(s *sandbox, _ map[string]string) { s.codexDir = filepath.Join(real, ".codex") },
		"real ~/.kenaz":     func(s *sandbox, _ map[string]string) { s.kenazHome = filepath.Join(real, ".kenaz") },
		"env outside":       func(_ *sandbox, over map[string]string) { over["KENAZ_HOME"] = filepath.Join(real, ".kenaz") },
		"parent escape":     func(s *sandbox, _ map[string]string) { s.work = filepath.Join(s.root, "..", "elsewhere") },
		"root is real home": func(s *sandbox, _ map[string]string) { s.root = real },
		"credential in env": func(s *sandbox, _ map[string]string) { s.env = append(s.env, "ANTHROPIC_API_KEY=x") },
	} {
		c := *s
		c.env = append([]string(nil), s.env...)
		over := map[string]string{"KENAZ_HOME": c.kenazHome}
		mutate(&c, over)
		if err := c.assertContained(over); err == nil || !strings.Contains(err.Error(), "refusing to run") {
			t.Errorf("%s: not refused (%v)", name, err)
		}
	}
}

func TestScrubRemovesPaths(t *testing.T) {
	s := &sandbox{root: filepath.Join(string(filepath.Separator), "tmp", "kenaz-conformance-1"), realHome: filepath.Join(string(filepath.Separator), "Users", "someone")}
	got := s.scrub("wrote " + filepath.Join(s.root, "home", ".claude") + "\n and " + filepath.Join(s.realHome, ".claude"))
	if strings.Contains(got, "someone") || strings.Contains(got, "kenaz-conformance-1") || strings.Contains(got, "\n") {
		t.Fatal(got)
	}
	if len(s.scrub(strings.Repeat("x ", 500))) > 610 {
		t.Fatal("not bounded")
	}
}

func TestCheckSchema(t *testing.T) {
	for schema, ok := range map[string]bool{
		`{"type":"object","additionalProperties":false}`:                                           true,
		`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`:                  true,
		`{"type":"object","properties":{"a":{"type":["null","array"],"items":{"type":"string"}}}}`: true,
		`{"type":"object","properties":{"a":{"type":"string"}},"required":["b"]}`:                  false,
		`{"type":"array"}`: false,
		`{"type":"object","properties":{"a":"string"}}`:                  false,
		`{"type":"object","properties":{"a":{"description":"no type"}}}`: false,
		`not json`: false,
	} {
		if why := checkSchema(json.RawMessage(schema)); (why == "") != ok {
			t.Errorf("%s: %q", schema, why)
		}
	}
}

func TestTOMLAndHooks(t *testing.T) {
	text := "[otel]\nx = 1\n\n[marketplaces.kenaz]\nsource_type = \"local\"\nsource = \"/a/b\"\n\n[plugins.\"kenaz@kenaz\"]\nenabled = true\n"
	if !tomlSectionHas(text, "[marketplaces.kenaz]", func(l string) bool {
		return strings.HasPrefix(l, "source =") && unquoteTOML(strings.SplitN(l, "=", 2)[1]) == "/a/b"
	}) {
		t.Fatal("marketplace source not found")
	}
	if tomlSectionHas(text, "[otel]", func(l string) bool { return l == "enabled = true" }) {
		t.Fatal("matched a key of another table")
	}
	if unquoteTOML(` 'C:\x\y' `) != `C:\x\y` || unquoteTOML(`"C:\\x"`) != `C:\x` {
		t.Fatal("unquoteTOML")
	}
	hooks, err := hookCommands(filepath.Join("..", "..", "..", "plugin", "hooks", "hooks.json"))
	if err != nil || len(hooks) == 0 {
		t.Fatal(err)
	}
	for _, h := range hooks {
		if !strings.HasPrefix(h.command, "kenaz hook ") {
			t.Fatalf("plugin hook %q does not start kenaz hook", h.command)
		}
	}
}

// The journey refuses builds that could reach Fleet and unknown cells
// before it creates anything.
func TestRunRefusesWhatItMustNotRun(t *testing.T) {
	for name, o := range map[string]Options{
		"fleet tag":       {Feature: "skills", Host: "codex-cli", Tags: "fleet,kenaz_context_library"},
		"dev variant":     {Feature: "skills", Host: "codex-cli", Tags: "kenaz_dev"},
		"unknown host":    {Feature: "skills", Host: "opencode"},
		"unknown feature": {Feature: "everything", Host: "codex-cli"},
	} {
		if res, err := Run(context.Background(), o); err == nil || len(res.Steps) != 0 {
			t.Errorf("%s: ran (%v)", name, err)
		}
	}
}

func TestSecretLike(t *testing.T) {
	for name, want := range map[string]bool{
		"ANTHROPIC_API_KEY": true, "OPENAI_BASE_URL": true, "CLAUDE_CONFIG_DIR": true, "CLAUDECODE": true, "CODEX_HOME": true,
		"KENAZ_ENV": true, "FLEET_PLUGIN_ENV": true, "GH_TOKEN": true, "github_token": true, "SOME_SECRET": true,
		"PATH": false, "HOME": false, "GOCACHE": false, "SystemRoot": false, "LANG": false,
	} {
		if secretLike(name) != want {
			t.Errorf("%s: %v", name, !want)
		}
	}
}
