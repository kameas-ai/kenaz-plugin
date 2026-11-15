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

	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
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
	// The verified bundle this kenaz runs from, if any: a pinned Codex
	// checkout must carry exactly its plugin tree.
	var verified *Bundle
	if o.BundleRoot != "" {
		verified, _ = LoadBundle(o.BundleRoot)
	}
	for _, h := range []harness{harnessClaude, harnessCodex} {
		fs := state.get(h)
		if fs != nil && fs.Adopted != nil {
			mk := o.kenazMarketplace(h)
			pinned, why := false, ""
			if mk.official() {
				pinned, why = o.releasePin(h, mk, verified)
			}
			// What Codex runs is its own copy of the plugin, made at
			// install time, not the pinned clone.
			stale := ""
			if h == harnessCodex && pinned {
				stale = o.codexCacheStale()
			}
			switch {
			case h == harnessClaude && mk.Inline && !pinned:
				cs = append(cs, Check{h.label(), "warn", fmt.Sprintf("the Kenaz plugin marketplace is declared inline (managed or user settings) and is not pinned to this release's commit; deploy the extraKnownMarketplaces entry from this release's `%s setup print-managed claude`", variant.Current.Binary)})
			// The same reading of the host's files as setup: turned off
			// means explicitly off (mk.Off). A plugin that is installed
			// but not listed as enabled may be enabled by settings setup
			// cannot read, so it is not reported as off.
			case !mk.official():
				cs = append(cs, Check{h.label(), "warn", "the Kenaz plugin marketplace from GitHub is no longer registered; re-run setup"})
			case mk.Off:
				cs = append(cs, Check{h.label(), "warn", fmt.Sprintf("the Kenaz plugin (%s) is installed but turned off, so it will not run. %s", bundlePluginID, enableHint(h))})
			case !mk.Installed && !mk.Inline:
				cs = append(cs, Check{h.label(), "warn", "the Kenaz plugin from GitHub is no longer installed; re-run setup"})
			case !pinned:
				cs = append(cs, Check{h.label(), "warn", fmt.Sprintf("the Kenaz plugin from GitHub %s; run `%s` from a release download to use its verified copy", why, o.initCmd())})
			case h == harnessCodex && stale != "":
				cs = append(cs, Check{h.label(), "warn", fmt.Sprintf("the marketplace is pinned to this release, but the copy of the Kenaz plugin that Codex runs %s; re-run setup to reinstall it", stale)})
			case o.KenazOnPath != nil && !o.KenazOnPath():
				cs = append(cs, Check{h.label(), "warn", fmt.Sprintf("the Kenaz plugin from GitHub starts %[1]s from PATH, and %[1]s is not on PATH; run `%[2]s`", variant.Current.Binary, o.initCmd())})
			default:
				cs = append(cs, Check{h.label(), "ok", fmt.Sprintf("plugin from GitHub (%s) pinned to %s, launching %s from PATH", OfficialRepo, mk.Pin, variant.Current.Binary)})
			}
			continue
		}
		if fs == nil || fs.Bundle == nil {
			continue
		}
		src, enabled := o.registeredMarketplace(h)
		exe, broken := o.registeredLauncher(fs.Bundle)
		switch {
		case broken != "":
			cs = append(cs, Check{h.label(), "fail", broken})
		case src != fs.Bundle.Rendered || !enabled:
			cs = append(cs, Check{h.label(), "warn", "the bundle plugin registration was changed outside setup; re-run setup"})
		case bundle.StablePath(exe) != exe:
			cs = append(cs, Check{h.label(), "warn", fmt.Sprintf("the plugin launches %s, which an upgrade removes; run `%s` to register it through %s", exe, o.initCmd(), bundle.StablePath(exe))})
		case fs.Bundle.Launcher == "" && isHomebrewCask(exe):
			cs = append(cs, Check{h.label(), "warn", fmt.Sprintf("the plugin launches %s from Homebrew's versioned directory, which `brew upgrade` removes; run `%s`", exe, o.initCmd())})
		default:
			cur := currentBundleVersion(o)
			if cur != "" && cur != fs.Bundle.Version {
				cs = append(cs, Check{h.label(), "warn", fmt.Sprintf("the registered plugin files are from %[1]s %[2]s and this %[1]s is %[3]s; run `%[4]s` to refresh them", variant.Current.Binary, fs.Bundle.Version, cur, o.initCmd())})
				continue
			}
			cs = append(cs, Check{h.label(), "ok", fmt.Sprintf("plugin registered from bundle %s (launches %s)", fs.Bundle.Version, exe)})
		}
	}
	return cs
}

func isHomebrewCask(exe string) bool {
	_, ok := bundle.HomebrewCaskPrefix(exe)
	return ok
}

// currentBundleVersion is the version of the bundle this kenaz runs from
// ("" when there is none or it does not verify).
func currentBundleVersion(o Options) string {
	if o.BundleRoot == "" {
		return ""
	}
	b, err := LoadBundle(o.BundleRoot)
	if err != nil {
		return ""
	}
	return b.Version
}

// registeredLauncher returns the executable a bundle registration's
// rendered plugin launches, and a non-empty problem when that executable
// is gone (for example the versioned Homebrew directory `brew upgrade`
// deleted, from state written before the launcher was recorded).
func (o Options) registeredLauncher(reg *BundleReg) (exe, problem string) {
	// A bundle that is still there must still verify. One that is gone is
	// expected after an upgrade removed its version, as long as the
	// recorded launcher (a version-independent path) still exists.
	if _, err := os.Stat(filepath.Join(reg.Root, BundleFile)); err == nil {
		b, err := LoadBundle(reg.Root)
		if err != nil {
			return "", fmt.Sprintf("the bundle at %s no longer verifies (%v); re-run setup from the installed bundle", reg.Root, err)
		}
		exe = b.Exe
	}
	if reg.Launcher != "" {
		fi, err := os.Stat(reg.Launcher)
		if err != nil || fi.IsDir() {
			return reg.Launcher, fmt.Sprintf("the plugin launches %[1]s, which no longer exists; run `%[3]s` to register this %[2]s again", reg.Launcher, variant.Current.Binary, o.initCmd())
		}
		return reg.Launcher, ""
	}
	if exe == "" {
		// Before the launcher was recorded, the plugin launched the
		// bundle's own executable, and the bundle is gone.
		return "", fmt.Sprintf("the bundle at %[1]s is missing, so the plugin cannot start; run `%[3]s` to register this %[2]s again", reg.Root, variant.Current.Binary, o.initCmd())
	}
	return exe, ""
}

// BrokenRegistrations lists, for the setup state in stateDir, each Claude
// Code or Codex plugin registration whose launcher no longer exists (for
// example after `brew upgrade` removed the version a pre-0.6.0 setup
// registered), as one sentence each. It only reads files; the fix is
// `kenaz init` (or `kenaz setup claude|codex`), which re-registers, with
// --env when stateDir is not the build's default environment's.
func BrokenRegistrations(stateDir string) []string {
	initCmd := Options{StateDir: stateDir}.initCmd()
	state, err := loadState(stateDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, h := range []harness{harnessClaude, harnessCodex} {
		fs := state.get(h)
		if fs == nil || fs.Bundle == nil {
			continue
		}
		missing := false
		if fs.Bundle.Launcher != "" {
			_, err := os.Stat(fs.Bundle.Launcher)
			missing = err != nil
		} else {
			// Cheap check (no re-verification): the bundle directory is gone.
			_, err := os.Stat(filepath.Join(fs.Bundle.Root, BundleFile))
			missing = err != nil
		}
		if missing {
			out = append(out, fmt.Sprintf("the Kenaz plugin in %[1]s points at a %[3]s that no longer exists (it was registered from %[2]s). Run `%[4]s` to fix it.", h.label(), fs.Bundle.Root, variant.Current.Binary, initCmd))
			continue
		}
		// The state directory moved (to ~/.kenaz/plugin): the host still
		// loads the plugin files from the old location, through the link
		// left there or not at all.
		if reg := fs.Bundle.registered(); !within(stateDir, reg) {
			out = append(out, fmt.Sprintf("the Kenaz plugin in %[1]s still uses its files from %[2]s, where an earlier version kept them. Run `%[3]s` to fix it.", h.label(), filepath.Dir(reg), initCmd))
		}
	}
	return out
}

// Registrations returns, from the setup state in stateDir, the path each
// Claude Code ("claude") or Codex ("codex") plugin registration loads the
// plugin from, as recorded, whichever build wrote it. In builds with
// OpenCode support it also has the relay state directory the OpenCode
// adapter entry was given ("opencode"), which opencode.json embeds. It
// only reads files; a missing or unreadable record has none.
func Registrations(stateDir string) map[string]string {
	_, regs := RecordedRegistrations(stateDir)
	return regs
}

// RecordedRegistrations is Registrations together with the build that
// recorded them (a variant name; kenaz for records from before builds were
// recorded). One build's `init` or `setup` never changes what another
// build registered, so a record replaces another only within one build.
func RecordedRegistrations(stateDir string) (build string, regs map[string]string) {
	regs = map[string]string{}
	b, err := os.ReadFile(filepath.Join(stateDir, stateFile))
	if err != nil {
		return variant.Kenaz.Name, regs
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		return variant.Kenaz.Name, regs
	}
	for host, fs := range map[string]*FileState{"claude": s.Claude, "codex": s.Codex} {
		if fs != nil && fs.Bundle != nil && fs.Bundle.Rendered != "" {
			regs[host] = fs.Bundle.Rendered
		}
	}
	if features.OpenCode && s.OpenCode != nil {
		if dir := adapterStateDir(json.RawMessage(s.OpenCode.Keys[openCodeKeyPlugin].Written)); dir != "" {
			regs["opencode"] = dir
		}
	}
	return s.build(), regs
}

// adapterStateDir returns the state_dir option of an OpenCode "plugin"
// entry for the Kenaz adapter (["<adapter dir>", {"state_dir": ...}]), or
// "" when raw is not one.
func adapterStateDir(raw json.RawMessage) string {
	var tuple []json.RawMessage
	var opts struct {
		StateDir string `json:"state_dir"`
	}
	if json.Unmarshal(raw, &tuple) == nil && len(tuple) > 1 {
		_ = json.Unmarshal(tuple[1], &opts)
	}
	return opts.StateDir
}

// Within reports whether p is dir or inside it (lexically).
func Within(dir, p string) bool { return within(dir, p) }

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
		o.checkUserEndpoint(harnessClaude, add, s, port)
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
			add("ok", "plugin %s is installed", variant.Current.Plugin)
		} else {
			add("warn", "plugin %s is not installed", variant.Current.Plugin)
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
				o.checkUserEndpoint(harnessCodex, add, m[1], port)
			}
		}
	}
	add("info", "plugin hooks need one-time trust in Codex (/hooks); telemetry works without them")

	if out, err := run(ctx, "codex", "plugin", "list"); err == nil {
		if codexListsKenaz(out) {
			add("ok", "plugin %s is installed", variant.Current.Plugin)
		} else {
			add("warn", "plugin %s is not installed", variant.Current.Plugin)
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
var codexPluginRe = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(variant.Current.Plugin) + `(@` + regexp.QuoteMeta(variant.Current.Marketplace) + `)?([^A-Za-z0-9_.-]|$)`)

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
		if sd := adapterStateDir(p); sd != o.StateDir {
			add("warn", "the Kenaz adapter reports to %q, but this environment's relay state is %q; re-run `kenaz setup opencode`", sd, o.StateDir)
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

// within reports whether p is inside dir.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
