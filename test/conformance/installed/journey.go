package installed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/apps"
	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
	"github.com/kameas-ai/kenaz-plugin/test/conformance"
)

// Options selects one journey.
type Options struct {
	Root    string // module root
	Feature string // matrix feature id
	Host    string // matrix host id
	Tags    string // build tags for kenaz ("" = the default build)
	Log     io.Writer
}

// hostSpec is what the journey knows about a matrix host.
type hostSpec struct {
	label      string // the label `kenaz setup doctor` prints
	setupName  string // `kenaz setup <name>`
	cli        string // the host CLI's name
	mcpClient  string // the MCP client name the host announces
	clientKind string // the telemetry client kind (the OTel component journey's subtest)
}

var hosts = map[string]hostSpec{
	"claude-code-cli": {"Claude Code", "claude", "claude", "claude-code", "claude-code"},
	"codex-cli":       {"Codex", "codex", "codex", "codex-mcp-client", "codex"},
}

// Tools each feature must serve, by build. Local context file tools exist
// only with kenaz_context_library; the team file tools are served only while
// Fleet offers them to a signed-in member, so never here.
var (
	statusTools       = []string{"fleet_status"}
	contextTools      = []string{"context_search", "context_pull", "context_share", "context_promote", "team_members", "approval_status"}
	contextFileTools  = []string{"context_list", "context_read", "context_write"}
	contextTeamFiles  = []string{"context_share_file", "context_pull_files"}
	memoryTools       = []string{"memory_save", "memory_search", "memory_get", "memory_recent", "memory_timeline", "memory_update", "memory_forget", "memory_forget_all", "memory_export", "memory_sync_status", "memory_sync_settings"}
	skillsTools       = []string{"skills_search", "skills_install", "skills_uninstall", "skills_publish"}
	unofficialMessage = "unofficial build"
)

// officialDifferences names the checks that differ for an official build.
var officialDifferences = []string{
	"build: an official build links the proprietary Fleet client (tag fleet) and reports flavor official; this journey refuses to run one",
	"refusals: Fleet-backed tools here answer unofficial_build; an official signed-out build answers with its sign-in remedy instead",
	"otel: the signed-out relay refuses exports here because nobody can sign in; an official build accepts them once a member is signed in and has opted in",
	"plugin files: the bundle is assembled from this checkout's plugin/ and is unsigned; a release bundle is signed and verified against a public tag",
	"credential store: forced to files inside the temporary directory; a release install uses the OS keychain where there is one",
}

// unproven is what the installed tier leaves unproven, per feature.
var unproven = map[string][]string{
	"otel-export": {
		"that a real host session exports anything: no model call is made, so the host never emits telemetry",
		"receipt and acknowledgement by Kameas Fleet, opt-in and opt-out, retry and deduplication: the unofficial build cannot connect",
		"the projection of a live export by the installed binary: the allowlist check runs as a Go test against an in-process fake Fleet",
	},
	"context": {
		"team resolution, search, read, share, promotion, pagination and cross-team refusals against Fleet: the unofficial build cannot connect",
		"that a model in the host calls the tools, and the approval page a person completes",
		"team context files (context_share_file, context_pull_files) and the share grant: Fleet must offer them to a signed-in member",
	},
	"private-memory": {
		"memory sync, cross-host and two-device recall: they need a signed-in account",
		"automatic capture: it needs a host session with a model, and Fleet's confirmation",
		"that a model in the host calls the tools and recalls a note in a session",
	},
	"skills": {
		"browse, install, update, mandated materialization, publish and removal: they need Fleet's catalog",
		"that the host discovers and invokes an installed Fleet skill in a session with a model",
	},
}

var commonUnproven = []string{
	"the official, signed release artifact: this is an unofficial build of this checkout",
	"the locked host release: the host CLI found on the runner (if any) is whatever is installed, not a version recorded in compatibility.json",
	"anything that needs a model call, a Kameas Fleet sign-in or a person's approval",
}

// journey is one run's state.
type journey struct {
	o     Options
	h     hostSpec
	s     *sandbox
	res   conformance.InstalledResult
	relay *exec.Cmd
	// relayExited closes when relay has been waited for; relayGuard releases
	// what ties the relay's life to this process (proc_windows.go).
	relayExited chan struct{}
	relayGuard  func()
	port        int
	env         string // Fleet env directory name under the state root

	hostCLI string // absolute path, "" when absent
	// hostOriginal is the host's config as it was before setup (hostSeed).
	hostOriginal []byte
	rendered     string // the rendered plugin directory setup registered
	tags         map[string]bool
}

func (j *journey) logf(format string, a ...any) {
	if j.o.Log != nil {
		fmt.Fprintf(j.o.Log, format+"\n", a...)
	}
}

func (j *journey) step(id, status, command, detail string) {
	if j.s != nil {
		command, detail = j.s.scrub(command), j.s.scrub(detail)
	}
	if status != conformance.StepPass && detail == "" {
		detail = "no detail"
	}
	j.res.Steps = append(j.res.Steps, conformance.InstalledStep{ID: id, Status: status, Command: command, Detail: detail})
	j.logf("%-8s %s %s", status, id, detail)
}

func (j *journey) pass(id, command, detail string) { j.step(id, conformance.StepPass, command, detail) }
func (j *journey) fail(id, command, detail string) { j.step(id, conformance.StepFail, command, detail) }
func (j *journey) notRun(id, command, detail string) {
	j.step(id, conformance.StepNotRun, command, detail)
}

// check records pass when problems is empty, else fail with the problems.
func (j *journey) check(id, command, okDetail string, problems []string) bool {
	if len(problems) > 0 {
		j.fail(id, command, strings.Join(problems, "; "))
		return false
	}
	j.pass(id, command, okDetail)
	return true
}

// Run executes the journey and returns its result. The error is set only
// when the journey could not start honestly (bad options, a sandbox that is
// not contained); every other problem is a failed step in the result.
func Run(ctx context.Context, o Options) (conformance.InstalledResult, error) {
	h, ok := hosts[o.Host]
	if !ok {
		return conformance.InstalledResult{}, fmt.Errorf("unknown host %q", o.Host)
	}
	if _, ok := unproven[o.Feature]; !ok {
		return conformance.InstalledResult{}, fmt.Errorf("unknown feature %q", o.Feature)
	}
	j := &journey{o: o, h: h, tags: map[string]bool{}}
	for _, t := range strings.FieldsFunc(o.Tags, func(r rune) bool { return r == ',' || r == ' ' }) {
		j.tags[t] = true
	}
	if j.tags["fleet"] || j.tags["kenaz_dev"] {
		return conformance.InstalledResult{}, errors.New("the installed tier runs the public unofficial kenaz build only (no fleet or kenaz_dev tag)")
	}
	j.res.Build = conformance.InstalledBuild{Flavor: "unknown", Tags: o.Tags, OfficialDifferences: officialDifferences}
	j.res.HostCLI.Visibility = conformance.VisibilityNotRun
	j.res.Unproven = append(append([]string(nil), unproven[o.Feature]...), commonUnproven...)

	// A signal ends the journey through its deferred clean-up, not around it.
	ctx, stopSignals := signal.NotifyContext(ctx, abortSignals...)
	defer stopSignals()

	s, err := newSandbox()
	if err != nil {
		return j.res, err
	}
	j.s = s
	// Deferred in this order so that the relays are stopped first, then the
	// directory is removed, then the watchdog is told there is nothing left.
	defer s.startWatchdog()()
	defer s.close()
	defer j.stopRelay()
	if err := s.useFileCredentialStore(); err != nil {
		return j.res, err
	}
	j.pass("sandbox", "", "every path the journey and its children write is inside one temporary directory; credentials and host settings are stripped from the environment; the credential store is recorded as files there before kenaz first runs, so no OS keychain is probed")

	// aborted ends the journey when it was interrupted: what the steps after
	// that point would record is the interruption, not the product.
	aborted := func() error {
		if ctx.Err() != nil {
			return fmt.Errorf("the journey was interrupted (%v) and stopped its relay", context.Cause(ctx))
		}
		return nil
	}
	if !j.build(ctx) || !j.makeBundle() || !j.startRelay(ctx) {
		return j.res, aborted()
	}
	if afterRelayStarted != nil {
		afterRelayStarted(j)
	}
	if err := aborted(); err != nil {
		return j.res, err
	}
	j.findHost(ctx)
	if j.setup(ctx) {
		j.hostConfig()
		j.doctor(ctx)
		j.hostVisibility(ctx)
	}
	if err := aborted(); err != nil {
		return j.res, err
	}
	j.mcp(ctx)
	if o.Feature == "otel-export" {
		j.otel(ctx)
	}
	if o.Feature == "skills" {
		j.skillsCLI(ctx)
	}
	j.undo(ctx)
	return j.res, aborted()
}

// afterRelayStarted is called, in tests only, once the relay is running.
var afterRelayStarted func(j *journey)

// build compiles kenaz from this checkout into the bundle and refuses
// anything but the unofficial flavor.
func (j *journey) build(ctx context.Context) bool {
	args := []string{"build", "-tags", j.o.Tags, "-o", j.s.exe, "./cmd/kenaz"}
	command := "go build -tags '" + j.o.Tags + "' -o $TMP/bundle/bin/kenaz ./cmd/kenaz"
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", args...)
	cmd.Dir = j.o.Root // the builder's own environment: go needs its real cache
	if out, err := cmd.CombinedOutput(); err != nil {
		j.fail("build", command, "go build failed: "+lastLine(string(out)))
		return false
	}
	out, code := j.s.kenaz(ctx, "version", "--json")
	var v struct {
		Version string `json:"version"`
		Flavor  string `json:"flavor"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil {
		j.fail("build", "kenaz version --json", fmt.Sprintf("exit %d, or output that does not parse", code))
		return false
	}
	j.res.Build.Flavor, j.res.Build.Version = v.Flavor, v.Version
	if v.Flavor != "unofficial" {
		j.fail("build", "kenaz version --json", "flavor is "+v.Flavor+": the installed tier never runs a build that can connect to Kameas Fleet")
		return false
	}
	j.pass("build", command+" && kenaz version --json", "unofficial build of this checkout, version "+v.Version+" (an official build would report flavor official and is refused here)")
	return true
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// makeBundle lays this checkout's plugin files around the built binary the
// way a release archive does, so that setup registers the plugin by
// absolute path, and verifies it with the product's own loader.
func (j *journey) makeBundle() bool {
	const id = "bundle"
	files := map[string]string{}
	add := func(rel string, data []byte, mode os.FileMode) error {
		p := filepath.Join(j.s.bundle, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[rel] = hex.EncodeToString(sum[:])
		return nil
	}
	pluginDir := filepath.Join(j.o.Root, "plugin")
	err := filepath.WalkDir(pluginDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".DS_Store" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(j.o.Root, p)
		return add(filepath.ToSlash(rel), data, 0o600)
	})
	if err == nil {
		var data []byte
		if data, err = os.ReadFile(filepath.Join(j.o.Root, ".claude-plugin", "marketplace.json")); err == nil {
			err = add(".claude-plugin/marketplace.json", data, 0o600)
		}
	}
	if err != nil {
		j.fail(id, "", "copying plugin files: "+err.Error())
		return false
	}
	exeRel := "bin/" + filepath.Base(j.s.exe)
	exeBytes, err := os.ReadFile(j.s.exe)
	if err != nil {
		j.fail(id, "", err.Error())
		return false
	}
	sum := sha256.Sum256(exeBytes)
	files[exeRel] = hex.EncodeToString(sum[:])
	var pj struct{ Version string }
	if b, err := os.ReadFile(filepath.Join(pluginDir, ".claude-plugin", "plugin.json")); err == nil {
		_ = json.Unmarshal(b, &pj)
	}
	manifest, _ := json.MarshalIndent(map[string]any{
		"schema": 1, "name": "kenaz", "version": pj.Version, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"executable": exeRel, "executable_sha256": files[exeRel], "opencode": false, "files": files,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(j.s.bundle, bundle.BundleFile), manifest, 0o600); err != nil {
		j.fail(id, "", err.Error())
		return false
	}
	b, err := bundle.LoadBundle(j.s.bundle)
	if err != nil {
		j.fail(id, "", "the assembled bundle does not verify: "+err.Error())
		return false
	}
	j.pass(id, "", fmt.Sprintf("local bundle of this checkout's plugin/ (%d files, plugin version %s, unsigned) verifies with the product's bundle loader", len(files), b.Version))

	// What setup hands a host, rendered here whether or not a host CLI is on
	// the runner: the per-user plugin copy with absolute launch commands.
	dir := filepath.Join(j.s.root, "render-check")
	if _, err := bundle.Render(b, dir, true); err != nil {
		j.fail("plugin-render", "", "rendering the plugin copy: "+err.Error())
		return true
	}
	problems, hooks := j.checkRendered(dir)
	sort.Strings(problems)
	j.check("plugin-render", "", fmt.Sprintf("the plugin copy setup registers with a host declares one MCP server, kenaz, and %d hooks, all launching the bundle's binary by absolute path; no server or hook is added", hooks), problems)
	return true
}

// startRelay starts `kenaz daemon` in its own process group and waits for
// its state file. The credential store was recorded as files before kenaz
// first ran (useFileCredentialStore).
func (j *journey) startRelay(ctx context.Context) bool {
	const id, command = "relay", "kenaz daemon"
	if err := j.s.fileStoreRecorded(); err != nil {
		j.fail(id, command, err.Error())
		return false
	}
	cmd := exec.Command(j.s.exe, "daemon")
	cmd.Dir, cmd.Env = j.s.work, j.s.env
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		j.fail(id, command, err.Error())
		return false
	}
	j.relay = cmd
	j.relayGuard = guardProcess(cmd)
	j.s.recordRelayPid(cmd.Process.Pid)
	exited := make(chan struct{})
	j.relayExited = exited
	go func() { _ = cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(j.s.kenazHome, "plugin", "*", "daemon.json"))
		for _, m := range matches {
			var st struct {
				Port int `json:"port"`
			}
			if b, err := os.ReadFile(m); err == nil && json.Unmarshal(b, &st) == nil && st.Port > 0 {
				j.port, j.env = st.Port, filepath.Base(filepath.Dir(m))
				j.pass(id, command, "the relay of the unofficial build started on a loopback port and wrote its state file; credential store forced to files inside the temporary directory (a release install would use the OS keychain where there is one)")
				return true
			}
		}
		select {
		case <-exited:
			j.fail(id, command, "the relay exited before writing its state file")
			return false
		case <-ctx.Done():
			j.fail(id, command, "interrupted while the relay was starting")
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	j.fail(id, command, "the relay wrote no state file within 20 s")
	return false
}

// stopRelay stops the relay the journey started and any relay recorded in
// the sandbox (relay.go), before the temporary directory is removed.
func (j *journey) stopRelay() {
	j.s.stopRelays(j.relay, j.relayExited)
	if j.relayGuard != nil {
		j.relayGuard()
	}
}

var versionRe = regexp.MustCompile(`[0-9]+\.[0-9]+(\.[0-9]+)?[0-9A-Za-z.+-]*`)

// findHost looks for the real host CLI the way `kenaz setup` does (PATH,
// then the copies desktop apps ship), with the sandbox's home.
func (j *journey) findHost(ctx context.Context) {
	probe := apps.Probe{
		GOOS: runtime.GOOS, Home: j.s.home, Getenv: j.s.getenv, LookPath: exec.LookPath,
		Exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		Glob:   filepath.Glob,
	}
	if j.h.cli == "claude" {
		j.hostCLI = probe.ClaudeCLI()
	} else {
		j.hostCLI = probe.CodexCLI()
	}
	id, command := "host-cli", j.h.cli+" --version"
	if j.hostCLI == "" {
		j.res.HostCLI.Note = "host CLI absent: " + j.h.cli + " is not on PATH or in a desktop app on this runner"
		j.notRun(id, command, j.res.HostCLI.Note+"; registration with the host and host visibility are not run")
		return
	}
	j.res.HostCLI.Present = true
	out, code := j.s.run(ctx, 30*time.Second, j.hostCLI, "--version")
	v := versionRe.FindString(out)
	if code != 0 || v == "" {
		j.res.HostCLI.Version = "installed (version not reported)"
	} else {
		j.res.HostCLI.Version = j.h.cli + " " + v
	}
	j.pass(id, command, "found the real host CLI ("+j.res.HostCLI.Version+"); it is never signed in and never asked for a model call")
}

// setup runs the dry run, then the real setup, for the cell's host.
func (j *journey) setup(ctx context.Context) bool {
	cfg := j.hostConfigPath()
	// The host's config exists before setup, with a user's own content
	// (hostconfig.go): setup must change only its own keys, and undo must
	// put this file back byte for byte.
	j.hostOriginal = hostSeed(j.h.cli)
	if err := os.WriteFile(cfg, j.hostOriginal, 0o600); err != nil {
		j.fail("setup-dry-run", "", "writing the pre-existing host config: "+err.Error())
		return false
	}
	args := []string{"setup", j.h.setupName}
	if j.hostCLI == "" {
		// Without the host's CLI there is nothing to register the plugin
		// with; --no-bundle sets up telemetry only, which is what a user
		// without the host would get.
		args = append(args, "--no-bundle")
	}
	dry := append(append([]string(nil), args...), "--dry-run")
	out, code := j.s.kenaz(ctx, dry...)
	var problems []string
	if code != 0 {
		problems = append(problems, fmt.Sprintf("exit %d: %s", code, lastLine(out)))
	}
	if !strings.Contains(out, "(dry run: nothing written)") {
		problems = append(problems, "the dry run did not say that nothing was written")
	}
	if b, err := os.ReadFile(cfg); err != nil || !bytes.Equal(b, j.hostOriginal) {
		problems = append(problems, "the dry run changed the host's config file")
	}
	if _, err := os.Stat(filepath.Join(j.stateDir(), "setup-state.json")); err == nil {
		problems = append(problems, "the dry run recorded setup state")
	}
	j.check("setup-dry-run", "kenaz "+strings.Join(dry, " "), "shows the change and writes nothing", problems)

	apply := append(append([]string(nil), args...), "--yes")
	out, code = j.s.kenaz(ctx, apply...)
	if code != 0 {
		j.fail("setup-apply", "kenaz "+strings.Join(apply, " "), fmt.Sprintf("exit %d: %s", code, lastLine(out)))
		return false
	}
	detail := "telemetry keys written to the host's pre-existing config in the temporary directory"
	if j.hostCLI != "" {
		detail += "; plugin registered through the host's own CLI from the local bundle"
	} else {
		detail += "; plugin registration not attempted (the host CLI is absent, and the host's CLI is what registers a plugin)"
	}
	j.pass("setup-apply", "kenaz "+strings.Join(apply, " "), detail)
	return true
}

func (j *journey) stateDir() string { return filepath.Join(j.s.kenazHome, "plugin", j.env) }

func (j *journey) hostConfigPath() string {
	if j.h.cli == "claude" {
		return filepath.Join(j.s.claudeDir, "settings.json")
	}
	return filepath.Join(j.s.codexDir, "config.toml")
}

// hostConfig asserts what setup wrote: the owned telemetry keys, pointing
// at this relay, and (with the host CLI) the plugin registration, the MCP
// server and the hooks, all launching the bundle's binary by absolute path.
func (j *journey) hostConfig() {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", j.port)
	raw, err := os.ReadFile(j.hostConfigPath())
	if err != nil {
		j.fail("host-config-telemetry", "", "the host's config file was not written")
		return
	}
	var problems []string
	var claude struct {
		Env            map[string]string `json:"env"`
		EnabledPlugins map[string]bool   `json:"enabledPlugins"`
		Marketplaces   map[string]struct {
			Source struct {
				Source string `json:"source"`
				Path   string `json:"path"`
			} `json:"source"`
		} `json:"extraKnownMarketplaces"`
	}
	text := string(raw)
	if j.h.cli == "claude" {
		_ = json.Unmarshal(raw, &claude)
		problems = checkClaudeConfig(j.hostOriginal, raw, endpoint, j.hostCLI != "")
	} else {
		problems = checkCodexConfig(j.hostOriginal, raw, endpoint, j.hostCLI != "")
	}
	j.check("host-config-telemetry", "", "compared with the whole file as it was before setup, the host's config differs only in the telemetry keys setup owns, each with the exact value that points it at this relay with content logging off; the user's other keys are untouched", problems)

	const id = "host-config-plugin"
	if j.hostCLI == "" {
		j.notRun(id, "", "host CLI absent: the plugin, its MCP server and its hooks are registered by the host's own CLI, which is not on this runner")
		return
	}
	problems = nil
	matches, _ := filepath.Glob(filepath.Join(j.stateDir(), "setup", "bundle", "*"))
	if len(matches) != 1 {
		j.fail(id, "", fmt.Sprintf("want one rendered plugin copy under the relay state, found %d", len(matches)))
		return
	}
	j.rendered = matches[0]
	if j.h.cli == "claude" {
		mk := claude.Marketplaces["kenaz"]
		if mk.Source.Source != "directory" || !samePath(mk.Source.Path, j.rendered) {
			problems = append(problems, "extraKnownMarketplaces.kenaz does not point at the rendered plugin copy")
		}
		if !claude.EnabledPlugins["kenaz@kenaz"] {
			problems = append(problems, "enabledPlugins does not enable kenaz@kenaz")
		}
	} else {
		if !tomlSectionHas(text, "[marketplaces.kenaz]", func(line string) bool {
			k, v, ok := strings.Cut(line, "=")
			return ok && strings.TrimSpace(k) == "source" && samePath(unquoteTOML(v), j.rendered)
		}) {
			problems = append(problems, "[marketplaces.kenaz] does not point at the rendered plugin copy")
		}
		if !tomlSectionHas(text, `[plugins."kenaz@kenaz"]`, func(line string) bool {
			return strings.ReplaceAll(line, " ", "") == "enabled=true"
		}) {
			problems = append(problems, `[plugins."kenaz@kenaz"] is not enabled`)
		}
	}
	rp, hooks := j.checkRendered(j.rendered)
	problems = append(problems, rp...)
	sort.Strings(problems)
	j.check(id, "", fmt.Sprintf("the host's config registers and enables kenaz@kenaz from the rendered copy; its one MCP server and its %d hooks launch the bundle's binary by absolute path, and none was added", hooks), problems)
}

// checkRendered checks a rendered plugin copy: its one MCP server and every
// hook launch the bundle's binary by absolute path, and nothing was added.
// It returns the problems and the number of hooks.
func (j *journey) checkRendered(rendered string) (problems []string, hooks int) {
	// The rendered plugin: the MCP server and every hook launch the
	// bundle's binary by absolute path, and nothing was added.
	for _, name := range []string{".mcp.json", ".codex-mcp.json"} {
		var doc struct {
			Servers map[string]struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		b, err := os.ReadFile(filepath.Join(rendered, "plugin", name))
		if err != nil || json.Unmarshal(b, &doc) != nil {
			problems = append(problems, name+" is missing or does not parse")
			continue
		}
		srv, ok := doc.Servers["kenaz"]
		if !ok || len(doc.Servers) != 1 || !samePath(srv.Command, j.s.exe) || strings.Join(srv.Args, " ") != "mcp" {
			problems = append(problems, name+" does not register exactly one server, kenaz, launching the bundle's binary with `mcp`")
		}
	}
	want, err1 := hookCommands(filepath.Join(j.o.Root, "plugin", "hooks", "hooks.json"))
	got, err2 := hookCommands(filepath.Join(rendered, "plugin", "hooks", "hooks.json"))
	switch {
	case err1 != nil || err2 != nil:
		problems = append(problems, "hooks.json is missing or does not parse")
	case len(got) != len(want) || len(got) == 0:
		problems = append(problems, fmt.Sprintf("rendered hooks.json has %d hook(s), the plugin has %d", len(got), len(want)))
	default:
		for i := range want {
			// "kenaz hook x" must have become "\"<exe>\" hook x".
			exe, rest, ok := strings.Cut(strings.TrimPrefix(got[i].command, `"`), `"`)
			if got[i].event != want[i].event || !ok || !samePath(exe, j.s.exe) || rest != strings.TrimPrefix(want[i].command, "kenaz") {
				problems = append(problems, "hook "+want[i].event+" does not launch the bundle's binary with the plugin's arguments")
			}
		}
	}
	return problems, len(want)
}

type hookCommand struct{ event, command string }

// hookCommands lists a hooks.json's (event, command) pairs, sorted.
func hookCommands(path string) ([]hookCommand, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var out []hookCommand
	for event, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out = append(out, hookCommand{event, h.Command})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].event != out[b].event {
			return out[a].event < out[b].event
		}
		return hookArg(out[a].command) < hookArg(out[b].command)
	})
	return out, nil
}

// hookArg is the hook's argument (after " hook "), which orders the hooks
// of one event the same way before and after rendering.
func hookArg(command string) string {
	_, arg, _ := strings.Cut(command, " hook ")
	return arg
}

// tomlSectionHas reports whether a line of the named TOML table satisfies
// ok. It reads only what setup and the host write: one key per line.
func tomlSectionHas(text, header string, ok func(line string) bool) bool {
	in := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == header:
			in = true
		case strings.HasPrefix(line, "["):
			in = false
		case in && ok(line):
			return true
		}
	}
	return false
}

func unquoteTOML(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	var s string
	if json.Unmarshal([]byte(v), &s) == nil {
		return s
	}
	return v
}

var doctorLineRe = regexp.MustCompile(`^\[(ok|info|warn|fail)\s*\] (relay|Claude Code|Codex|OpenCode)\s+(.*)$`)

// doctor asserts that `kenaz setup doctor` sees what setup did, with no
// warning or failure for this cell's host.
func (j *journey) doctor(ctx context.Context) {
	const id, command = "doctor", "kenaz setup doctor"
	out, code := j.s.kenaz(ctx, "setup", "doctor")
	if code != 0 {
		j.fail(id, command, fmt.Sprintf("exit %d", code))
		return
	}
	type line struct{ status, detail string }
	byLabel := map[string][]line{}
	for _, l := range strings.Split(out, "\n") {
		if m := doctorLineRe.FindStringSubmatch(strings.TrimRight(l, "\r")); m != nil {
			byLabel[m[2]] = append(byLabel[m[2]], line{m[1], m[3]})
		}
	}
	has := func(label, status, text string) bool {
		for _, l := range byLabel[label] {
			if l.status == status && strings.Contains(l.detail, text) {
				return true
			}
		}
		return false
	}
	var problems []string
	if !has("relay", "ok", "listening on 127.0.0.1") {
		problems = append(problems, "doctor does not report the relay listening")
	}
	for _, ls := range byLabel {
		for _, l := range ls {
			if l.status == "fail" {
				problems = append(problems, "doctor reports a failure: "+l.detail)
			}
		}
	}
	if j.hostCLI == "" {
		sort.Strings(problems)
		j.check(id, command, "relay ok and no failure; doctor's checks of the host's own state are not run because the host CLI is absent (see host-cli)", problems)
		return
	}
	exports := 1
	if j.h.cli == "codex" {
		exports = 3
	}
	n := 0
	for _, l := range byLabel[j.h.label] {
		if l.status == "ok" && strings.HasPrefix(l.detail, "exports to the relay") {
			n++
		}
		if l.status == "warn" {
			problems = append(problems, "doctor warns about "+j.h.label+": "+l.detail)
		}
	}
	if n != exports {
		problems = append(problems, fmt.Sprintf("doctor reports %d exporter(s) to the relay, want %d", n, exports))
	}
	if !has(j.h.label, "ok", "plugin kenaz is installed") {
		problems = append(problems, "doctor does not report the plugin installed in the host")
	}
	if !has(j.h.label, "ok", "plugin registered from bundle") {
		problems = append(problems, "doctor does not report the bundle registration")
	}
	sort.Strings(problems)
	j.check(id, command, "relay ok; "+j.h.label+" exports to the relay, has the plugin installed and registered from the bundle; no warning or failure for it", problems)
}

var signInRe = regexp.MustCompile(`(?i)\b(log ?in|sign ?in|signed in|authenticat|unauthori[sz]ed|not logged|credentials?|api key|401)\b`)

// hostVisibility asks the real host CLI, with listing commands only,
// whether it sees the plugin and its MCP server. It never signs in and
// never starts a session. A listing that needs sign-in is recorded as
// such; one that the installed host version does not have is not run.
func (j *journey) hostVisibility(ctx context.Context) {
	const id = "host-visibility"
	if j.hostCLI == "" {
		j.notRun(id, "", "host CLI absent: nothing can ask the host whether it sees the plugin")
		return
	}
	type listing struct {
		args []string
		ok   func(out string) string // "" when the host shows the plugin
	}
	var listings []listing
	if j.h.cli == "claude" {
		listings = []listing{
			{[]string{"plugin", "list", "--json"}, func(out string) string {
				var ps []struct {
					ID      string `json:"id"`
					Enabled bool   `json:"enabled"`
					Servers map[string]struct {
						Command string `json:"command"`
					} `json:"mcpServers"`
				}
				if json.Unmarshal([]byte(out[strings.Index(out+"[", "["):]), &ps) != nil {
					return "output does not parse"
				}
				for _, p := range ps {
					if p.ID == "kenaz@kenaz" {
						if !p.Enabled {
							return "the host lists kenaz@kenaz as not enabled"
						}
						if !samePath(p.Servers["kenaz"].Command, j.s.exe) {
							return "the host's kenaz MCP server does not launch the bundle's binary"
						}
						return ""
					}
				}
				return "the host does not list kenaz@kenaz"
			}},
			{[]string{"plugin", "marketplace", "list"}, func(out string) string {
				if !strings.Contains(out, "kenaz") {
					return "the host does not list the kenaz marketplace"
				}
				return ""
			}},
			{[]string{"plugin", "details", "kenaz@kenaz"}, func(out string) string {
				for _, want := range []string{"SessionStart", "SessionEnd", "MCP servers (1)"} {
					if !strings.Contains(out, want) {
						return "the host's component inventory lacks " + want
					}
				}
				return ""
			}},
			// Connects to each MCP server and lists it; no model is involved.
			{[]string{"mcp", "list"}, func(out string) string {
				for _, l := range strings.Split(out, "\n") {
					if strings.Contains(l, "kenaz") && strings.Contains(l, " mcp") {
						if strings.Contains(l, "Connected") {
							return ""
						}
						return "the host lists the kenaz MCP server but did not connect to it"
					}
				}
				return "the host does not list the kenaz MCP server"
			}},
		}
	} else {
		listings = []listing{
			{[]string{"plugin", "list"}, func(out string) string {
				for _, l := range strings.Split(out, "\n") {
					if strings.Contains(l, "kenaz@kenaz") {
						if strings.Contains(l, "installed") && strings.Contains(l, "enabled") && !strings.Contains(l, "disabled") {
							return ""
						}
						return "the host lists kenaz@kenaz but not as installed and enabled"
					}
				}
				return "the host does not list kenaz@kenaz"
			}},
			{[]string{"plugin", "marketplace", "list"}, func(out string) string {
				if !strings.Contains(out, "kenaz") {
					return "the host does not list the kenaz marketplace"
				}
				return ""
			}},
			{[]string{"mcp", "list", "--json"}, func(out string) string {
				var ss []struct {
					Name      string `json:"name"`
					Enabled   bool   `json:"enabled"`
					Transport struct {
						Command string   `json:"command"`
						Args    []string `json:"args"`
					} `json:"transport"`
				}
				if json.Unmarshal([]byte(out[strings.Index(out+"[", "["):]), &ss) != nil {
					return "output does not parse"
				}
				for _, s := range ss {
					if s.Name == "kenaz" {
						if !s.Enabled || !samePath(s.Transport.Command, j.s.exe) || strings.Join(s.Transport.Args, " ") != "mcp" {
							return "the host's kenaz MCP server is not enabled, or does not launch the bundle's binary with `mcp`"
						}
						return ""
					}
				}
				return "the host does not list the kenaz MCP server"
			}},
		}
	}
	var results []listingResult
	for _, l := range listings {
		r := listingResult{name: j.h.cli + " " + strings.Join(l.args, " ")}
		r.out, r.code = j.s.run(ctx, 90*time.Second, j.hostCLI, l.args...)
		if r.code == 0 {
			r.why = l.ok(r.out)
		}
		results = append(results, r)
	}
	v := classifyListings(results)
	var names []string
	for _, r := range results {
		names = append(names, r.name)
	}
	command := strings.Join(names, "; ")
	note := ""
	if len(v.worked) > 0 {
		note += "worked without sign-in: " + strings.Join(v.worked, ", ") + ". "
	}
	if len(v.needSignIn) > 0 {
		note += "host listing needs sign-in: " + strings.Join(v.needSignIn, ", ") + ". "
	}
	j.res.HostCLI.Visibility, j.res.HostCLI.Note = v.visibility, j.s.scrub(note)
	switch v.status {
	case conformance.StepFail:
		// Includes a listing that exits non-zero without asking for sign-in:
		// the host has not shown that it sees the plugin.
		j.fail(id, command, strings.Join(v.problems, "; "))
	case conformance.StepPass:
		j.pass(id, command, "the host itself lists the plugin and its MCP server, with no sign-in and no model call. "+j.res.HostCLI.Note)
	default:
		j.notRun(id, command, note)
	}
}

// mcp speaks MCP to the installed binary: initialize, tools/list (the
// feature's tools, with schemas that parse, for this build's tags), then
// the feature's signed-out behavior.
func (j *journey) mcp(ctx context.Context) {
	c, err := j.s.startMCP(ctx)
	if err != nil {
		j.fail("mcp-initialize", "kenaz mcp", err.Error())
		return
	}
	defer c.close()
	name, err := c.initialize(j.h.mcpClient)
	if err != nil || name != "kenaz" {
		j.fail("mcp-initialize", "kenaz mcp: initialize", fmt.Sprintf("handshake failed or server name %q: %v", name, err))
		return
	}
	j.pass("mcp-initialize", "kenaz mcp: initialize", "the installed binary answers the MCP handshake over stdio as server kenaz, announced to it as "+j.h.mcpClient)

	served, err := c.tools()
	if err != nil {
		j.fail("mcp-tools", "kenaz mcp: tools/list", err.Error())
		return
	}
	var problems []string
	for _, n := range sortedNames(served) {
		if why := checkSchema(served[n].InputSchema); why != "" {
			problems = append(problems, n+": "+why)
		}
	}
	want := append([]string(nil), statusTools...)
	var absent []string
	note := ""
	switch j.o.Feature {
	case "context":
		want = append(want, contextTools...)
		absent = append(absent, contextTeamFiles...)
		if j.tags["kenaz_context_library"] {
			want = append(want, contextFileTools...)
			note = "; local context file tools are in this build (kenaz_context_library)"
		} else {
			absent = append(absent, contextFileTools...)
			note = "; local context file tools are not in this build (no kenaz_context_library tag; the official build has them)"
		}
	case "private-memory":
		want = append(want, memoryTools...)
	case "skills":
		want = append(want, skillsTools...)
	}
	for _, n := range want {
		if _, ok := served[n]; !ok {
			problems = append(problems, "tool "+n+" is not served")
		}
	}
	for _, n := range absent {
		if _, ok := served[n]; ok {
			problems = append(problems, "tool "+n+" is served, but this build and sign-in state must not serve it")
		}
	}
	if !j.check("mcp-tools", "kenaz mcp: tools/list", fmt.Sprintf("%d tools served, every input schema parses; the %d tools this cell needs are present%s", len(served), len(want), note), problems) {
		return
	}
	switch j.o.Feature {
	case "context":
		j.contextCalls(c)
	case "private-memory":
		j.memoryCalls(c)
	case "skills":
		j.skillsCalls(c)
	case "otel-export":
		j.statusCall(c)
	}
}

// refused reports whether r is the unofficial build's documented refusal of
// a Fleet-backed tool.
func refused(r callResult) bool {
	return r.errorCode() == "unofficial_build" || (r.IsError && strings.Contains(r.Text, unofficialMessage))
}

func (j *journey) refusals(c *mcpClient, id string, calls map[string]map[string]any) {
	var names, problems []string
	for n := range calls {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r, err := c.call(n, calls[n])
		switch {
		case err != nil:
			problems = append(problems, err.Error())
		case !refused(r):
			problems = append(problems, n+" did not answer with the unofficial-build refusal (code "+r.errorCode()+")")
		}
	}
	j.check(id, "kenaz mcp: tools/call "+strings.Join(names, ", "), "each Fleet-backed tool refuses with the documented unofficial-build message and returns no data (an official signed-out build answers with its sign-in remedy instead)", problems)
}

func (j *journey) statusCall(c *mcpClient) {
	r, err := c.call("fleet_status", nil)
	var problems []string
	switch {
	case err != nil:
		problems = append(problems, err.Error())
	case r.IsError:
		problems = append(problems, "fleet_status returned an error")
	default:
		if r.Structured["flavor"] != "unofficial" {
			problems = append(problems, "fleet_status does not report the unofficial flavor")
		}
		rep, _ := r.Structured["reporting"].(map[string]any)
		if rep["signed_in"] != false {
			problems = append(problems, "fleet_status does not report signed_in false")
		}
		d, _ := r.Structured["daemon"].(map[string]any)
		if d["running"] != true {
			problems = append(problems, "fleet_status does not see the running relay")
		}
	}
	j.check("mcp-status", "kenaz mcp: tools/call fleet_status", "fleet_status reports the unofficial flavor, the running relay and signed_in false", problems)
}

func (j *journey) contextCalls(c *mcpClient) {
	j.refusals(c, "context-fleet-refusals", map[string]map[string]any{
		"context_search":  {"query": "conformance"},
		"context_pull":    {"ids": []string{"conformance-node"}},
		"context_share":   {"title": "conformance", "body": "conformance", "kind": "fact"},
		"context_promote": {"node_id": "conformance-node"},
		"team_members":    {},
	})
	const id = "context-local-files"
	if !j.tags["kenaz_context_library"] {
		j.notRun(id, "", "this build has no local context file tools (no kenaz_context_library tag); run the journey with KENAZ_CONFORMANCE_TAGS=kenaz_context_library to exercise them")
		return
	}
	const path, content = "personal/conformance-probe.md", "# Conformance probe\n\nA synthetic file written by the installed-tier journey.\n"
	sum := sha256.Sum256([]byte(content))
	want := hex.EncodeToString(sum[:])
	var problems []string
	w, err := c.call("context_write", map[string]any{"path": path, "content": content})
	if err != nil || w.IsError || w.Structured["created"] != true || w.Structured["sha256"] != want {
		problems = append(problems, "context_write did not create the personal file with the expected digest")
	}
	l, err := c.call("context_list", nil)
	found := false
	if err == nil {
		files, _ := l.Structured["files"].([]any)
		for _, f := range files {
			fm, _ := f.(map[string]any)
			if fm["path"] == path {
				found = fm["source"] == "personal" && fm["trust"] == "yours"
			}
		}
	}
	if !found {
		problems = append(problems, "context_list does not list the file as a personal file of the user's own")
	}
	r, err := c.call("context_read", map[string]any{"path": path})
	if err != nil || r.IsError || r.Structured["sha256"] != want || r.Structured["content"] != content {
		problems = append(problems, "context_read did not return what was written")
	}
	t, err := c.call("context_write", map[string]any{"path": "team/conformance/probe.md", "content": content})
	if err != nil || t.errorCode() != "not_signed_in" {
		problems = append(problems, "writing a team file signed out was not refused with not_signed_in")
	}
	// The file is on disk inside the sandbox and nowhere else.
	onDisk := 0
	_ = filepath.WalkDir(j.s.kenazHome, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "conformance-probe.md" {
			onDisk++
		}
		return nil
	})
	if onDisk != 1 {
		problems = append(problems, fmt.Sprintf("want the file once under the sandbox's kenaz home, found %d", onDisk))
	}
	j.check(id, "kenaz mcp: tools/call context_write, context_list, context_read", "a personal context file is written, listed as the user's own and read back with the same digest, on this device only; a team file is refused with not_signed_in", problems)
}

func (j *journey) memoryCalls(c *mcpClient) {
	const id = "memory-local"
	const title, body = "Conformance probe", "A synthetic note saved by the installed-tier journey: quokka-lantern-0413."
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	saved, err := c.call("memory_save", map[string]any{"title": title, "body": body})
	noteID, _ := saved.Structured["id"].(string)
	if err != nil || saved.IsError || noteID == "" || saved.Structured["where"] != "device" || saved.Structured["sync_state"] != "local" {
		bad("memory_save did not save a device-only note")
	}
	hit := func(r callResult, key string) bool {
		items, _ := r.Structured[key].([]any)
		for _, it := range items {
			if m, _ := it.(map[string]any); m["id"] == noteID {
				return true
			}
		}
		return false
	}
	if r, err := c.call("memory_search", map[string]any{"query": "quokka-lantern-0413"}); err != nil || !hit(r, "results") {
		bad("memory_search does not find the note")
	}
	if r, err := c.call("memory_recent", nil); err != nil || !hit(r, "entries") {
		bad("memory_recent does not list the note")
	}
	if r, err := c.call("memory_get", map[string]any{"ids": []string{noteID}}); err != nil || !hit(r, "notes") {
		bad("memory_get does not return the note")
	}
	if r, err := c.call("memory_update", map[string]any{"id": noteID, "title": title + " (edited)"}); err != nil || r.IsError || r.Structured["status"] != "done" {
		bad("memory_update did not change the note on this device")
	}
	if r, err := c.call("memory_forget", map[string]any{"id": noteID}); err != nil || r.Structured["deleted"] != true || r.Structured["where"] != "device" {
		bad("memory_forget did not delete the note on this device")
	}
	if r, err := c.call("memory_search", map[string]any{"query": "quokka-lantern-0413"}); err != nil || hit(r, "results") {
		bad("the note is still found after memory_forget")
	}
	if _, err := os.Stat(filepath.Join(j.s.kenazHome, "plugin", "memory", "memory.db")); err != nil {
		bad("the notebook is not inside the sandbox's kenaz home")
	}
	j.check(id, "kenaz mcp: tools/call memory_save, memory_search, memory_recent, memory_get, memory_update, memory_forget", "a note is saved on this device only, found, read, edited and forgotten, and is then gone", problems)

	problems = nil
	if r, err := c.call("memory_sync_status", nil); err != nil || r.Structured["available"] != false {
		bad("memory_sync_status does not say sync is unavailable signed out")
	}
	if r, err := c.call("memory_sync_settings", map[string]any{"enabled": true}); err != nil || r.errorCode() != "needs_terminal" {
		bad("turning memory sync on from a tool was not refused with needs_terminal")
	}
	if r, err := c.call("memory_forget_all", nil); err != nil || r.errorCode() != "approval_unavailable" {
		bad("memory_forget_all signed out was not refused with approval_unavailable")
	}
	j.check("memory-signed-out-refusals", "kenaz mcp: tools/call memory_sync_status, memory_sync_settings, memory_forget_all", "signed out, sync is reported unavailable, a tool cannot turn sync on (needs_terminal) and forget-all is refused (approval_unavailable)", problems)
}

func (j *journey) skillsCalls(c *mcpClient) {
	j.refusals(c, "skills-fleet-refusals", map[string]map[string]any{
		"skills_search":  {"query": "conformance"},
		"skills_install": {"catalog_id": "conformance-skill"},
	})
	var problems []string
	r, err := c.call("skills_uninstall", map[string]any{"catalog_id": "conformance-skill"})
	if n, _ := r.Structured["removed"].(float64); err != nil || r.IsError || n != 0 {
		problems = append(problems, "skills_uninstall of a skill that is not installed did not report removed 0")
	}
	j.check("skills-local", "kenaz mcp: tools/call skills_uninstall", "removing a skill that is not installed removes nothing and says so", problems)
}

// skillsCLI checks the local listing and that the plugin's own skill is in
// the copy the host loads.
func (j *journey) skillsCLI(ctx context.Context) {
	out, code := j.s.kenaz(ctx, "skills")
	var problems []string
	if code != 0 {
		problems = append(problems, fmt.Sprintf("kenaz skills exited %d", code))
	}
	if !strings.Contains(out, "No Kameas Fleet skills installed") && !strings.Contains(out, "No supported agent harness") {
		problems = append(problems, "kenaz skills did not report an empty local list")
	}
	j.check("skills-local-list", "kenaz skills", "the local listing runs signed out and reports no Fleet skills installed", problems)

	const id = "skills-plugin-skill"
	if j.rendered == "" {
		j.notRun(id, "", "host CLI absent: no plugin copy was registered, so there is no host-loaded skill directory to check")
		return
	}
	problems = nil
	want, err1 := os.ReadFile(filepath.Join(j.o.Root, "plugin", "skills", "kenaz", "SKILL.md"))
	got, err2 := os.ReadFile(filepath.Join(j.rendered, "plugin", "skills", "kenaz", "SKILL.md"))
	if err1 != nil || err2 != nil || !bytes.Equal(want, got) {
		problems = append(problems, "the registered plugin copy does not carry the plugin's kenaz skill unchanged")
	}
	j.check(id, "", "the plugin copy the host loads carries the plugin's own kenaz skill, byte for byte", problems)
}

// undo checks `kenaz setup --undo` twice, because undo has two ways to put
// a host's config back and a user can be on either:
//
//   - setup-undo: the file is exactly as setup left it, so undo restores
//     the snapshot it took. The file must be, byte for byte, the one that
//     existed before setup.
//   - setup-undo-after-edit: setup runs again and the user then edits a key
//     of their own, as a host or a person does all the time. Undo can no
//     longer restore a snapshot: it must restore each owned key (the user's
//     prior value, or its absence) and leave the edit alone. The file must
//     be, byte for byte, the pre-setup file with that one edit.
//
// Both also require that the plugin is no longer registered with the host.
func (j *journey) undo(ctx context.Context) {
	if _, err := os.Stat(filepath.Join(j.stateDir(), "setup-state.json")); err != nil {
		j.notRun("setup-undo", "kenaz setup --undo "+j.h.setupName+" --yes", "setup did not complete, so there is nothing to undo")
		j.notRun("setup-undo-after-edit", "", "setup did not complete, so there is nothing to undo")
		return
	}
	if !j.undoOnce(ctx, "setup-undo", "", j.hostOriginal, false,
		"the host's config is %s the file as it was before setup and registers no kenaz plugin or marketplace") {
		j.notRun("setup-undo-after-edit", "", "the first undo failed; the second cycle would not start from the pre-setup file")
		return
	}

	const id = "setup-undo-after-edit"
	args := []string{"setup", j.h.setupName}
	if j.hostCLI == "" {
		args = append(args, "--no-bundle")
	}
	args = append(args, "--yes")
	again := "kenaz " + strings.Join(args, " ")
	if out, code := j.s.kenaz(ctx, args...); code != 0 {
		j.fail(id, again, fmt.Sprintf("setting up again: exit %d: %s", code, lastLine(out)))
		return
	}
	cur, err := os.ReadFile(j.hostConfigPath())
	edited, ok := userEdit(j.h.cli, cur)
	want, ok2 := userEdit(j.h.cli, j.hostOriginal)
	if err != nil || !ok || !ok2 {
		j.fail(id, again, "could not make the user's edit to the host's config after setup: "+firstDifference(j.hostOriginal, cur))
		return
	}
	if err := os.WriteFile(j.hostConfigPath(), edited, 0o600); err != nil {
		j.fail(id, again, err.Error())
		return
	}
	j.undoOnce(ctx, id, again+"; the user edits a key of their own; ", want, j.hostCLI != "",
		"after the user edited a key of their own, undo restored each key setup owns to what the user had before (or removed it) and left the edit alone: the host's config is %s the pre-setup file with that edit, and registers no kenaz plugin or marketplace")
}

// undoOnce runs `kenaz setup --undo` and records step id: the host's config
// must then be exactly want, not point at the relay and not register the
// plugin; with the host CLI, the host's own listing must not show it.
// hostRewrote is checkUndone's; detail has one %s for how the file compared.
func (j *journey) undoOnce(ctx context.Context, id, before string, want []byte, hostRewrote bool, detail string) bool {
	command := before + "kenaz setup --undo " + j.h.setupName + " --yes"
	out, code := j.s.kenaz(ctx, "setup", "--undo", j.h.setupName, "--yes")
	var problems []string
	if code != 0 {
		problems = append(problems, fmt.Sprintf("exit %d: %s", code, lastLine(out)))
	}
	after, err := os.ReadFile(j.hostConfigPath())
	if err != nil {
		after = nil
	}
	more, identical := checkUndone(j.h.cli, want, after, fmt.Sprintf("http://127.0.0.1:%d", j.port), hostRewrote)
	problems = append(problems, more...)
	if identical {
		detail = fmt.Sprintf(detail, "byte-identical to")
	} else {
		detail = fmt.Sprintf(detail, "the same content as (not byte-identical: the host's own CLI rewrote the file while unregistering the plugin, so key order, blank lines and plugin maps it left empty are ignored)")
	}
	if j.hostCLI != "" {
		// Ask the host itself, with a listing command only.
		args := []string{"plugin", "list"}
		if j.h.cli == "claude" {
			args = append(args, "--json")
		}
		name := j.h.cli + " " + strings.Join(args, " ")
		command += "; " + name
		switch lout, lcode := j.s.run(ctx, 90*time.Second, j.hostCLI, args...); {
		case lcode == 0 && strings.Contains(lout, "kenaz@kenaz"):
			problems = append(problems, "the host still lists kenaz@kenaz after --undo")
		case lcode == 0:
			detail += "; the host's own plugin listing no longer shows kenaz@kenaz"
		case signInRe.MatchString(lout):
			detail += "; the host's plugin listing needs sign-in, so the host was not asked"
		default:
			problems = append(problems, fmt.Sprintf("%s: exit %d without asking for sign-in, so the host did not confirm the plugin is gone", name, lcode))
		}
	} else {
		detail += "; no host CLI was present, so no plugin was registered with a host and none was asked"
	}
	return j.check(id, command, detail, problems)
}
