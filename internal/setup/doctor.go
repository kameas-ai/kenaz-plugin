package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
)

// Check is one doctor finding.
type Check struct {
	Name   string
	Status string // ok, info, warn, fail
	Detail string
}

// Runner runs an external command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs commands with a short timeout.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// Minimum harness versions.
var (
	minClaude = [3]int{2, 1, 217}
	minCodex  = [3]int{0, 147, 0}
)

// Doctor inspects the harness configuration the relay depends on. It only
// reads files and runs `--version` / plugin-list commands.
func Doctor(ctx context.Context, o Options, run Runner) []Check {
	var cs []Check
	add := func(name, status, format string, a ...any) {
		cs = append(cs, Check{name, status, fmt.Sprintf(format, a...)})
	}

	port := 0
	st, err := daemonstate.Read(o.StateDir)
	switch {
	case errors.Is(err, daemonstate.ErrNoDaemon):
		add("relay", "warn", "not started yet; it starts with the first harness session that loads the plugin")
	case err != nil:
		add("relay", "fail", "%v", err)
	default:
		port = st.Port
		if healthz(ctx, st) {
			add("relay", "ok", "listening on 127.0.0.1:%d (version %s)", st.Port, st.Version)
		} else {
			add("relay", "warn", "daemon.json says port %d but the relay is not answering there", st.Port)
		}
	}

	cs = append(cs, doctorClaude(ctx, o, port, run)...)
	cs = append(cs, doctorCodex(ctx, o, port, run)...)
	if features.OpenCode {
		cs = append(cs, doctorOpenCode(ctx, o, run)...)
	}
	cs = append(cs, doctorBundle(o)...)
	return cs
}

// doctorBundle checks plugin registrations setup made from a release
// bundle: the host still points at the rendered plugin and its executable
// still exists.
func doctorBundle(o Options) []Check {
	state, err := loadState(o.StateDir)
	if err != nil {
		return nil
	}
	var cs []Check
	for _, h := range []harness{harnessClaude, harnessCodex} {
		fs := state.get(h)
		if fs == nil || fs.Bundle == nil {
			continue
		}
		src, enabled := o.registeredMarketplace(h)
		exe := ""
		if b, err := LoadBundle(fs.Bundle.Root); err == nil {
			exe = b.Exe
		}
		switch {
		case exe == "":
			cs = append(cs, Check{h.label(), "fail", fmt.Sprintf("the bundle at %s is missing or no longer verifies; re-run setup from the installed bundle", fs.Bundle.Root)})
		case src != fs.Bundle.Rendered || !enabled:
			cs = append(cs, Check{h.label(), "warn", "the bundle plugin registration was changed outside setup; re-run setup"})
		default:
			cs = append(cs, Check{h.label(), "ok", fmt.Sprintf("plugin registered from bundle %s (launches %s)", fs.Bundle.Version, exe)})
		}
	}
	return cs
}

// healthz verifies the relay with the daemon's HMAC challenge-response; a
// foreign listener on the port fails it.
func healthz(ctx context.Context, st daemonstate.State) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return daemon.Probe(ctx, daemon.Info{PID: st.PID, Port: st.Port, Version: st.Version, StartedAt: st.StartedAt, Nonce: st.Nonce}) == nil
}

func doctorClaude(ctx context.Context, o Options, port int, run Runner) []Check {
	var cs []Check
	add := func(status, format string, a ...any) {
		cs = append(cs, Check{"Claude Code", status, fmt.Sprintf(format, a...)})
	}
	if v, ok := toolVersion(ctx, run, "claude"); !ok {
		add("info", "not installed (or not on PATH)")
		return cs
	} else if !atLeast(v, minClaude) {
		add("warn", "version %d.%d.%d is older than the supported minimum %d.%d.%d", v[0], v[1], v[2], minClaude[0], minClaude[1], minClaude[2])
	}

	if b, ok, _ := readFile(o.ClaudeManagedPath); ok {
		if obj, err := parseObject(b); err == nil {
			if env, ok := obj.get("env"); ok {
				if e, err := parseObject(env); err == nil {
					if v, ok := e.get(claudeEndpointKey); ok {
						s, _ := rawString(v)
						add("info", "managed settings (%s) set the OTLP endpoint to %s; they override user settings", o.ClaudeManagedPath, s)
						checkEndpoint(add, s, port)
					}
				}
			}
		}
	}

	b, exists, err := readFile(o.path(harnessClaude))
	switch {
	case err != nil:
		add("fail", "%v", err)
		return cs
	case !exists:
		add("warn", "%s does not exist; run `kenaz setup claude`", o.path(harnessClaude))
		return cs
	}
	obj, err := parseObject(b)
	if err != nil {
		add("fail", "%s is not valid JSON", o.path(harnessClaude))
		return cs
	}
	env := &object{}
	if raw, ok := obj.get("env"); ok {
		env, _ = parseObject(raw)
		if env == nil {
			env = &object{}
		}
	}
	if v, ok := env.get(claudeEndpointKey); ok {
		s, _ := rawString(v)
		checkEndpoint(add, s, port)
	} else {
		add("warn", "telemetry is not routed to the relay; run `kenaz setup claude`")
	}
	for _, k := range claudePerSignalEndpoints {
		if _, ok := env.get(k); ok {
			add("warn", "env.%s overrides the relay endpoint for that signal", k)
		}
	}
	for _, k := range claudeContentFlags {
		if v, ok := env.get(k); ok && truthy(v) {
			add("warn", "env.%s is on: Claude Code collects that content locally (the relay drops it; it never reaches Kameas Fleet)", k)
		}
	}
	add("info", "Claude Desktop sessions use the app's launch environment, which overrides settings \"env\"")

	if out, err := run(ctx, "claude", "plugin", "list", "--json"); err == nil {
		if strings.Contains(out, `"`+pluginID+`"`) {
			add("ok", "plugin kenaz is installed")
		} else {
			add("warn", "plugin kenaz is not installed")
		}
	}
	return cs
}

func doctorCodex(ctx context.Context, o Options, port int, run Runner) []Check {
	var cs []Check
	add := func(status, format string, a ...any) {
		cs = append(cs, Check{"Codex", status, fmt.Sprintf(format, a...)})
	}
	if v, ok := toolVersion(ctx, run, "codex"); !ok {
		add("info", "not installed (or not on PATH)")
		return cs
	} else if !atLeast(v, minCodex) {
		add("warn", "version %d.%d.%d is older than the supported minimum %d.%d.%d", v[0], v[1], v[2], minCodex[0], minCodex[1], minCodex[2])
	}

	if b, ok, _ := readFile(o.CodexManagedPath); ok {
		if sec, err := scanOtel(splitLines(string(b)), codexOwned); err == nil && sec.header >= 0 {
			add("info", "managed config (%s) has an [otel] table; it overrides user config", o.CodexManagedPath)
		}
	}

	b, exists, err := readFile(o.path(harnessCodex))
	if err != nil {
		add("fail", "%v", err)
		return cs
	}
	lines := splitLines(string(b))
	sec, err := scanOtel(lines, codexOwned)
	switch {
	case !exists || (err == nil && sec.header < 0):
		add("warn", "telemetry is not routed to the relay; run `kenaz setup codex`")
	case err != nil:
		add("fail", "config.toml: %v", err)
	default:
		for k := range codexExporters {
			e, ok := sec.find(k)
			if !ok {
				add("warn", "[otel] %s is not set; run `kenaz setup codex`", k)
				continue
			}
			for _, m := range tomlEndpointRe.FindAllStringSubmatch(joinLines(lines[e.start:e.end]), -1) {
				checkEndpoint(add, m[1], port)
			}
		}
	}
	add("info", "plugin hooks need one-time trust in Codex (/hooks); telemetry works without them")

	if out, err := run(ctx, "codex", "plugin", "list"); err == nil {
		if codexListsKenaz(out) {
			add("ok", "plugin kenaz is installed")
		} else {
			add("warn", "plugin kenaz is not installed")
		}
	}
	return cs
}

func checkEndpoint(add func(status, format string, a ...any), endpoint string, port int) {
	if !isLoopbackURL(endpoint) {
		add("fail", "exports to %s, not to the local relay", endpoint)
		return
	}
	if port == 0 {
		return
	}
	if !strings.Contains(endpoint, ":"+strconv.Itoa(port)) {
		add("warn", "exports to %s but the relay listens on port %d; re-run setup", endpoint, port)
		return
	}
	add("ok", "exports to the relay (%s)", endpoint)
}

var semverRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func toolVersion(ctx context.Context, run Runner, tool string) ([3]int, bool) {
	out, err := run(ctx, tool, "--version")
	if err != nil {
		return [3]int{}, false
	}
	m := semverRe.FindStringSubmatch(out)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func atLeast(v, min [3]int) bool {
	for i := range v {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

func printDoctor(w io.Writer, cs []Check) int {
	code := exitOK
	for _, c := range cs {
		fmt.Fprintf(w, "[%-4s] %-11s %s\n", c.Status, c.Name, c.Detail)
		if c.Status == "fail" {
			code = exitError
		}
	}
	return code
}

// codexPluginRe matches the plugin's own name as a whole token in
// `codex plugin list` output, so kenaz-* plugins or a bare "kenaz"
// marketplace with other plugins do not count as this plugin.
var codexPluginRe = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])kenaz(@kenaz)?([^A-Za-z0-9_.-]|$)`)

func codexListsKenaz(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if codexPluginRe.MatchString(line) {
			return true
		}
	}
	return false
}

func doctorOpenCode(ctx context.Context, o Options, run Runner) []Check {
	var cs []Check
	add := func(status, format string, a ...any) {
		cs = append(cs, Check{"OpenCode", status, fmt.Sprintf(format, a...)})
	}
	if v, ok := toolVersion(ctx, run, "opencode"); !ok {
		add("info", "not installed (or not on PATH)")
		return cs
	} else if !atLeast(v, minOpenCode) {
		add("warn", "version %d.%d.%d is older than the supported minimum %d.%d.%d", v[0], v[1], v[2], minOpenCode[0], minOpenCode[1], minOpenCode[2])
	}

	path := o.path(harnessOpenCode)
	b, exists, err := readFile(path)
	switch {
	case err != nil:
		add("fail", "%v", err)
		return cs
	case !exists:
		add("warn", "%s does not exist; run `kenaz setup opencode`", path)
		return cs
	}
	obj, err := parseObject(b)
	if err != nil {
		add("fail", "%s is not plain JSON", path)
		return cs
	}

	mcp := &object{}
	if raw, ok := obj.get("mcp"); ok {
		if mcp, err = parseObject(raw); err != nil {
			mcp = &object{}
		}
	}
	if raw, ok := mcp.get(openCodeServerName); ok {
		var e struct {
			Command []string `json:"command"`
			Enabled *bool    `json:"enabled"`
		}
		_ = json.Unmarshal(raw, &e)
		switch {
		case e.Enabled != nil && !*e.Enabled:
			add("warn", "mcp.%s is disabled; context, skills and memory tools are unavailable", openCodeServerName)
		case len(e.Command) == 0 || !fileExists(e.Command[0]):
			add("fail", "mcp.%s does not launch an existing kenaz binary; re-run `kenaz setup opencode`", openCodeServerName)
		default:
			add("ok", "MCP server %s runs %s", openCodeServerName, e.Command[0])
		}
	} else {
		add("warn", "the kenaz MCP server is not registered; run `kenaz setup opencode`")
	}

	var plugins []json.RawMessage
	if raw, ok := obj.get("plugin"); ok {
		_ = json.Unmarshal(raw, &plugins)
	}
	found := false
	for _, p := range plugins {
		spec := pluginSpec(p)
		if isNpmAdapter(spec) {
			found = true
			add("info", "the Kenaz adapter is loaded from npm (%s)", spec)
			continue
		}
		if checkAdapter(spec) != nil {
			continue
		}
		found = true
		var tuple []json.RawMessage
		var opts struct {
			StateDir string `json:"state_dir"`
		}
		if json.Unmarshal(p, &tuple) == nil && len(tuple) > 1 {
			_ = json.Unmarshal(tuple[1], &opts)
		}
		if opts.StateDir != o.StateDir {
			add("warn", "the Kenaz adapter reports to %q, but this environment's relay state is %q; re-run `kenaz setup opencode`", opts.StateDir, o.StateDir)
		} else {
			add("ok", "the Kenaz adapter is loaded from %s", spec)
		}
	}
	if !found {
		add("warn", "telemetry is not reported: the Kenaz adapter is not in \"plugin\"; run `kenaz setup opencode`")
	}
	if fileExists(filepath.Join(o.OpenCodeDir, "opencode.jsonc")) {
		add("info", "opencode.jsonc is merged with opencode.json; setup only edits opencode.json")
	}
	add("info", "OpenCode support is new: the adapter is checked against OpenCode %d.%d.%d events; Fleet must accept the opencode client kind before OpenCode activity is sent", minOpenCode[0], minOpenCode[1], minOpenCode[2])
	return cs
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
