package mcp

import (
	"os"
	"runtime"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// HostEnvVar names the MCP host when the host cannot be told apart
// otherwise. The Claude Desktop extension manifest (mcpb/) sets it to
// HostClaudeDesktop.
var HostEnvVar = variant.Shared.MCPHostVar

// HostClaudeDesktop is HostEnvVar's value under Claude Desktop.
const HostClaudeDesktop = "claude-desktop"

// SignInRemedy is what a tool tells the user to do to sign in.
func SignInRemedy() string {
	exe, _ := os.Executable()
	home, _ := os.UserHomeDir()
	return signInRemedy(os.Getenv, runtime.GOOS, exe, home)
}

// signInRemedy: Claude Code and Codex have the plugin's /kenaz:login
// command. Claude Desktop chat has no plugin commands and its extension's
// kenaz is usually not on PATH, so the user is given the command for the
// executable this server runs from, with the home directory abbreviated
// (no user name in a tool result).
func signInRemedy(getenv func(string) string, goos, exe, home string) string {
	if strings.TrimSpace(getenv(HostEnvVar)) != HostClaudeDesktop {
		return "Run /" + variant.Current.Plugin + ":login."
	}
	args := " login"
	if e := strings.ToLower(strings.TrimSpace(getenv(variant.Shared.EnvVar))); e != variant.Current.DefaultEnv && variant.Current.Allowed(e) {
		args += " --env " + e
	}
	if exe == "" {
		return "Open a terminal and run `" + variant.Current.Binary + args + "`, then try again."
	}
	if goos == "windows" {
		cmd := exe
		if rel, ok := under(home, exe, `\`); ok {
			cmd = `$env:USERPROFILE\` + rel
		}
		return "Open PowerShell and run `& \"" + cmd + "\"" + args + "`, then try again."
	}
	cmd := shQuote(exe)
	if rel, ok := under(home, exe, "/"); ok {
		cmd = "~/" + shQuote(rel)
	}
	return "Open a terminal and run `" + cmd + args + "`, then try again."
}

// under returns path relative to dir when path is inside dir.
func under(dir, path, sep string) (string, bool) {
	if dir == "" {
		return "", false
	}
	dir = strings.TrimRight(dir, sep) + sep
	if sep == `\` { // Windows paths are case-insensitive
		if strings.HasPrefix(strings.ToLower(path), strings.ToLower(dir)) {
			return path[len(dir):], true
		}
		return "", false
	}
	if strings.HasPrefix(path, dir) {
		return path[len(dir):], true
	}
	return "", false
}

// shQuote quotes s for a POSIX shell when it needs quoting.
func shQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
