// Package apps detects which of the apps Kenaz can serve are installed on
// this machine, and where their command-line tools are. It only looks:
// it stats paths and searches PATH, and never runs, opens or changes
// anything.
//
// What each app needs from Kenaz:
//
//   - Claude Code (CLI): `kenaz setup claude`.
//   - Claude Desktop, Code tab: its local sessions run Claude Code and read
//     the same user settings (~/.claude/settings.json, enabledPlugins,
//     extraKnownMarketplaces), so `kenaz setup claude` covers it.
//   - Claude Desktop, chat: a desktop extension (.mcpb), not available yet;
//     reported only when features.ClaudeDesktopChat is on.
//   - Codex CLI: `kenaz setup codex`.
//   - Codex in the ChatGPT desktop app (and the Codex desktop app): they run
//     the Codex app server with the same ~/.codex/config.toml, plugins and
//     marketplaces, so `kenaz setup codex` covers them.
package apps

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// ID names a detectable app.
type ID string

const (
	ClaudeCode    ID = "claude-code"
	ClaudeDesktop ID = "claude-desktop"
	Codex         ID = "codex"
	ChatGPTCodex  ID = "chatgpt-codex"
)

// App is one detection result.
type App struct {
	ID        ID
	Name      string
	Installed bool
	// Where is the first path that showed the app is installed (for the
	// report); "" when it is not installed.
	Where string
	// CLI is the absolute path of the command-line tool setup uses to
	// register plugins with this app's host ("" when none was found).
	CLI string
}

// Probe is the machine Detect looks at. System fills it for this process;
// tests use fake home directories and file sets.
type Probe struct {
	GOOS     string
	Home     string
	Getenv   func(string) string
	LookPath func(string) (string, error)
	// Exists reports whether a path exists; Glob expands a pattern.
	Exists func(string) bool
	Glob   func(string) ([]string, error)
}

// System is the probe for the current process.
func System() Probe {
	home, _ := os.UserHomeDir()
	return Probe{
		GOOS:     runtime.GOOS,
		Home:     home,
		Getenv:   os.Getenv,
		LookPath: exec.LookPath,
		Exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		Glob: filepath.Glob,
	}
}

func (p Probe) env(k string) string {
	if p.Getenv == nil {
		return ""
	}
	return p.Getenv(k)
}

func (p Probe) first(paths ...string) string {
	for _, c := range paths {
		if c != "" && p.Exists(c) {
			return c
		}
	}
	return ""
}

func (p Probe) look(name string) string {
	if p.LookPath == nil {
		return ""
	}
	if path, err := p.LookPath(name); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}
	return ""
}

// join builds a path for p.GOOS (tests probe Windows layouts on any OS).
func (p Probe) join(elem ...string) string {
	if p.GOOS == "windows" && runtime.GOOS != "windows" {
		return strings.Join(elem, `\`)
	}
	return filepath.Join(elem...)
}

func (p Probe) localAppData() string {
	if v := p.env("LOCALAPPDATA"); v != "" {
		return v
	}
	if v := p.env("LocalAppData"); v != "" {
		return v
	}
	return p.join(p.Home, "AppData", "Local")
}

func (p Probe) programFiles() string {
	if v := p.env("ProgramFiles"); v != "" {
		return v
	}
	return `C:\Program Files`
}

func exe(goos, name string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}

// ClaudeConfigDir is Claude Code's user configuration directory.
func (p Probe) ClaudeConfigDir() string {
	if v := p.env("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	return p.join(p.Home, ".claude")
}

// CodexHome is Codex's configuration directory.
func (p Probe) CodexHome() string {
	if v := p.env("CODEX_HOME"); v != "" {
		return v
	}
	return p.join(p.Home, ".codex")
}

// macApps are the application folders searched on macOS.
func (p Probe) macApp(name string) string {
	return p.first(p.join("/Applications", name), p.join(p.Home, "Applications", name))
}

// ClaudeDesktopPath returns where Claude Desktop is installed, or "".
//
//	macOS:   /Applications/Claude.app (or ~/Applications)
//	Windows: %LocalAppData%\AnthropicClaude (per-user installer), or the
//	         Microsoft Store / MSIX app alias in %LocalAppData%\Microsoft\WindowsApps
//	Linux:   the beta .deb (claude-desktop)
func (p Probe) ClaudeDesktopPath() string {
	switch p.GOOS {
	case "darwin":
		return p.macApp("Claude.app")
	case "windows":
		la := p.localAppData()
		return p.first(
			p.join(la, "AnthropicClaude", "claude.exe"),
			p.join(la, "Programs", "Claude", "Claude.exe"),
			p.join(p.programFiles(), "Claude", "Claude.exe"),
			p.join(la, "Microsoft", "WindowsApps", "claude.exe"),
		)
	case "linux":
		return p.first(
			"/usr/bin/claude-desktop",
			"/opt/Claude/claude-desktop",
			"/usr/share/applications/claude-desktop.desktop",
			p.join(p.Home, ".local", "share", "applications", "claude-desktop.desktop"),
		)
	}
	return ""
}

// claudeDesktopCLI returns the newest Claude Code that Claude Desktop
// keeps for its Code tab, or "" (macOS only; its layout elsewhere is not
// known).
func (p Probe) claudeDesktopCLI() string {
	if p.GOOS != "darwin" || p.Glob == nil {
		return ""
	}
	matches, _ := p.Glob(p.join(p.Home, "Library", "Application Support", "Claude", "claude-code", "*", "*", "claude.app", "Contents", "MacOS", "claude"))
	sort.Slice(matches, func(i, j int) bool { return versionLess(matches[i], matches[j]) })
	for i := len(matches) - 1; i >= 0; i-- {
		if p.Exists(matches[i]) {
			return matches[i]
		}
	}
	return ""
}

// ClaudeCLI returns the Claude Code CLI setup should run: `claude` on
// PATH, the native installer's location, or the copy Claude Desktop keeps.
func (p Probe) ClaudeCLI() string {
	if c := p.look("claude"); c != "" {
		return c
	}
	if c := p.first(
		p.join(p.Home, ".local", "bin", exe(p.GOOS, "claude")),
		p.join(p.Home, ".claude", "local", exe(p.GOOS, "claude")),
	); c != "" {
		return c
	}
	return p.claudeDesktopCLI()
}

// ChatGPTPath returns where an app that runs Codex locally is installed:
// the ChatGPT desktop app (Codex mode) or the Codex desktop app, or "".
func (p Probe) ChatGPTPath() string {
	switch p.GOOS {
	case "darwin":
		if a := p.macApp("ChatGPT.app"); a != "" {
			return a
		}
		return p.macApp("Codex.app")
	case "windows":
		la := p.localAppData()
		return p.first(
			p.join(la, "Microsoft", "WindowsApps", "ChatGPT.exe"),
			p.join(la, "Programs", "ChatGPT", "ChatGPT.exe"),
			p.join(la, "Microsoft", "WindowsApps", "Codex.exe"),
			p.join(la, "Programs", "Codex", "Codex.exe"),
		)
	}
	return ""
}

// CodexCLI returns the Codex CLI setup should run: `codex` on PATH, or the
// copy the ChatGPT or Codex desktop app ships (macOS).
func (p Probe) CodexCLI() string {
	if c := p.look("codex"); c != "" {
		return c
	}
	if p.GOOS == "darwin" {
		var cands []string
		for _, dir := range []string{"/Applications", p.join(p.Home, "Applications")} {
			cands = append(cands,
				p.join(dir, "ChatGPT.app", "Contents", "Resources", "codex-cli", "bin", "codex"),
				p.join(dir, "Codex.app", "Contents", "Resources", "codex"))
		}
		return p.first(cands...)
	}
	return ""
}

// Detect reports every app Kenaz can serve, in a fixed order.
func Detect(p Probe) []App {
	claudeCLI := p.ClaudeCLI()
	claudeWhere := p.look("claude")
	if claudeWhere == "" {
		claudeWhere = p.first(p.join(p.ClaudeConfigDir(), "settings.json"), p.join(p.Home, ".claude.json"))
	}
	if claudeWhere == "" && claudeCLI != "" && claudeCLI != p.claudeDesktopCLI() {
		// The native installer's copy, off PATH. The copy Claude Desktop
		// keeps for its Code tab does not count as Claude Code installed.
		claudeWhere = claudeCLI
	}
	desktop := p.ClaudeDesktopPath()

	codexCLI := p.CodexCLI()
	codexWhere := p.look("codex")
	if codexWhere == "" {
		codexWhere = p.first(p.join(p.CodexHome(), "config.toml"))
	}
	chatgpt := p.ChatGPTPath()

	return []App{
		{ID: ClaudeCode, Name: "Claude Code", Installed: claudeWhere != "", Where: claudeWhere, CLI: claudeCLI},
		{ID: ClaudeDesktop, Name: "Claude Desktop", Installed: desktop != "", Where: desktop, CLI: claudeCLI},
		{ID: Codex, Name: "Codex CLI", Installed: codexWhere != "", Where: codexWhere, CLI: codexCLI},
		{ID: ChatGPTCodex, Name: "ChatGPT desktop (Codex)", Installed: chatgpt != "", Where: chatgpt, CLI: codexCLI},
	}
}

// Find returns the result for id.
func Find(as []App, id ID) App {
	for _, a := range as {
		if a.ID == id {
			return a
		}
	}
	return App{ID: id}
}

// versionLess orders paths by the dotted version directory they contain
// (…/claude-code/<version>/…), falling back to string order.
func versionLess(a, b string) bool {
	va, vb := versionIn(a), versionIn(b)
	for i := 0; i < len(va) && i < len(vb); i++ {
		if va[i] != vb[i] {
			return va[i] < vb[i]
		}
	}
	if len(va) != len(vb) {
		return len(va) < len(vb)
	}
	return a < b
}

func versionIn(p string) []int {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i, s := range parts {
		if s == "claude-code" && i+1 < len(parts) {
			var out []int
			for _, f := range strings.Split(parts[i+1], ".") {
				n := 0
				for _, c := range f {
					if c < '0' || c > '9' {
						break
					}
					n = n*10 + int(c-'0')
				}
				out = append(out, n)
			}
			return out
		}
	}
	return nil
}
