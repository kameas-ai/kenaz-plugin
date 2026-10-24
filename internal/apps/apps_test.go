package apps

import (
	"errors"
	"path"
	"strings"
	"testing"
)

// fakeMachine is a probe over a fixed set of paths, for any OS.
func fakeMachine(goos, home string, files []string, onPath map[string]string, env map[string]string) Probe {
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	return Probe{
		GOOS:   goos,
		Home:   home,
		Getenv: func(k string) string { return env[k] },
		LookPath: func(name string) (string, error) {
			if p, ok := onPath[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		Exists: func(p string) bool { return set[p] },
		Glob: func(pattern string) ([]string, error) {
			var out []string
			for f := range set {
				if ok, _ := path.Match(pattern, f); ok {
					out = append(out, f)
				}
			}
			return out, nil
		},
	}
}

func byID(t *testing.T, as []App) map[ID]App {
	t.Helper()
	m := map[ID]App{}
	for _, a := range as {
		m[a.ID] = a
	}
	if len(m) != 4 {
		t.Fatalf("want 4 apps, got %v", as)
	}
	return m
}

func TestDetectNothing(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		got := byID(t, Detect(fakeMachine(goos, "/home/u", nil, nil, nil)))
		for id, a := range got {
			if a.Installed || a.Where != "" || a.CLI != "" {
				t.Errorf("%s %s: %+v", goos, id, a)
			}
		}
	}
}

func TestDetectMacOS(t *testing.T) {
	h := "/Users/u"
	desktopCLI := func(v string) string {
		return h + "/Library/Application Support/Claude/claude-code/" + v + "/abc123/claude.app/Contents/MacOS/claude"
	}
	files := []string{
		"/Applications/Claude.app",
		desktopCLI("2.1.299"), desktopCLI("2.1.308"), desktopCLI("2.1.30"),
		"/Applications/ChatGPT.app",
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex",
		h + "/.codex/config.toml",
	}
	got := byID(t, Detect(fakeMachine("darwin", h, files, nil, nil)))
	if a := got[ClaudeDesktop]; !a.Installed || a.Where != "/Applications/Claude.app" {
		t.Fatalf("desktop %+v", a)
	}
	// No claude on PATH and no ~/.claude: Claude Code itself is not
	// installed, but setup can still run the copy Claude Desktop keeps
	// (the newest one).
	if a := got[ClaudeCode]; a.Installed || a.CLI != desktopCLI("2.1.308") {
		t.Fatalf("claude code %+v", a)
	}
	if a := got[ChatGPTCodex]; !a.Installed || a.Where != "/Applications/ChatGPT.app" {
		t.Fatalf("chatgpt %+v", a)
	}
	// ~/.codex/config.toml (written by the desktop app) counts as Codex
	// being set up; the CLI is the copy ChatGPT ships.
	if a := got[Codex]; !a.Installed || a.CLI != "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex" {
		t.Fatalf("codex %+v", a)
	}

	// The Codex desktop app alone, in ~/Applications.
	got = byID(t, Detect(fakeMachine("darwin", h, []string{h + "/Applications/Codex.app", h + "/Applications/Codex.app/Contents/Resources/codex"}, nil, nil)))
	if a := got[ChatGPTCodex]; !a.Installed || a.CLI != h+"/Applications/Codex.app/Contents/Resources/codex" {
		t.Fatalf("codex app %+v", a)
	}
}

func TestDetectCLIsOnPath(t *testing.T) {
	got := byID(t, Detect(fakeMachine("linux", "/home/u", nil, map[string]string{"claude": "/usr/local/bin/claude", "codex": "/home/u/.npm/bin/codex"}, nil)))
	if a := got[ClaudeCode]; !a.Installed || a.Where != "/usr/local/bin/claude" || a.CLI != "/usr/local/bin/claude" {
		t.Fatalf("%+v", a)
	}
	if a := got[Codex]; !a.Installed || a.CLI != "/home/u/.npm/bin/codex" {
		t.Fatalf("%+v", a)
	}
	if got[ClaudeDesktop].Installed || got[ChatGPTCodex].Installed {
		t.Fatal("desktop apps detected from CLIs")
	}
}

func TestDetectConfigDirs(t *testing.T) {
	// CLAUDE_CONFIG_DIR and CODEX_HOME are honored; the native installer's
	// ~/.local/bin/claude is found off PATH.
	files := []string{"/cfg/claude/settings.json", "/cfg/codex/config.toml", "/home/u/.local/bin/claude"}
	got := byID(t, Detect(fakeMachine("linux", "/home/u", files, nil, map[string]string{"CLAUDE_CONFIG_DIR": "/cfg/claude", "CODEX_HOME": "/cfg/codex"})))
	if a := got[ClaudeCode]; !a.Installed || a.Where != "/cfg/claude/settings.json" || a.CLI != "/home/u/.local/bin/claude" {
		t.Fatalf("%+v", a)
	}
	if a := got[Codex]; !a.Installed || a.Where != "/cfg/codex/config.toml" || a.CLI != "" {
		t.Fatalf("%+v", a)
	}
}

func TestDetectLinuxDesktop(t *testing.T) {
	got := byID(t, Detect(fakeMachine("linux", "/home/u", []string{"/usr/share/applications/claude-desktop.desktop"}, nil, nil)))
	if a := got[ClaudeDesktop]; !a.Installed {
		t.Fatalf("%+v", a)
	}
	if got[ChatGPTCodex].Installed {
		t.Fatal("no ChatGPT desktop app exists for Linux")
	}
}

func TestDetectWindows(t *testing.T) {
	h := `C:\Users\u`
	la := h + `\AppData\Local`
	files := []string{
		la + `\AnthropicClaude\claude.exe`,
		h + `\.local\bin\claude.exe`,
		la + `\Microsoft\WindowsApps\ChatGPT.exe`,
		h + `\.codex\config.toml`,
	}
	got := byID(t, Detect(fakeMachine("windows", h, files, nil, map[string]string{"LOCALAPPDATA": la})))
	if a := got[ClaudeDesktop]; !a.Installed || !strings.HasSuffix(a.Where, `AnthropicClaude\claude.exe`) {
		t.Fatalf("%+v", a)
	}
	if a := got[ClaudeCode]; a.CLI != h+`\.local\bin\claude.exe` {
		t.Fatalf("%+v", a)
	}
	if a := got[ChatGPTCodex]; !a.Installed {
		t.Fatalf("%+v", a)
	}
	if a := got[Codex]; !a.Installed || a.Where != h+`\.codex\config.toml` {
		t.Fatalf("%+v", a)
	}
	// Without LOCALAPPDATA the layout is derived from the home directory.
	got = byID(t, Detect(fakeMachine("windows", h, files, nil, nil)))
	if !got[ClaudeDesktop].Installed {
		t.Fatal("home-derived LocalAppData")
	}
}

func TestFind(t *testing.T) {
	as := []App{{ID: Codex, Installed: true}}
	if !Find(as, Codex).Installed || Find(as, ClaudeCode).Installed || Find(as, ClaudeCode).ID != ClaudeCode {
		t.Fatal("Find")
	}
}
