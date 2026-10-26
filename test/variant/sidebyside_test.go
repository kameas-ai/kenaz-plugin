package variant_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// install is one build variant set up on a machine.
type install struct {
	v   variant.Identity
	l   layout
	bin string // the bundle's executable
}

func (i install) state() string { return i.l.state(i.v.DefaultEnv) }

// startRelay runs the variant's relay for its default environment in the
// background (file credential store, so no OS keychain) and returns its
// daemon.json.
func (m *machine) startRelay(i install) daemonstate.State {
	m.t.Helper()
	if err := credstore.WriteMarker(filepath.Join(i.l.StateRoot, "credentials"), credstore.Files); err != nil {
		m.t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(m.root, "tmp"), 0o700)
	cmd := exec.Command(i.bin, "daemon")
	cmd.Env = m.environ()
	cmd.Dir = m.home
	if err := cmd.Start(); err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err := daemonstate.Read(i.state())
		if err == nil && st.PID == cmd.Process.Pid {
			return st
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("%s relay did not start: %v", i.v.Name, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// snapshot is everything one variant owns on the machine that the other
// must leave alone: its setup state files, and its registrations in each
// host's config.
func (m *machine) snapshot(i install) string {
	m.t.Helper()
	var b strings.Builder
	setupFiles := func(rel string) bool { return true }
	fmt.Fprintf(&b, "setup-state.json:\n%s\n", readOptional(filepath.Join(i.state(), "setup-state.json")))
	b.WriteString(m.tree(filepath.Join(i.state(), "setup"), setupFiles))
	fmt.Fprintf(&b, "claude: %s\n", m.claudeReg(i.v))
	fmt.Fprintf(&b, "codex: %s\n", m.codexReg(i.v))
	return b.String()
}

func readOptional(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// claudeReg is v's marketplace and plugin entry in Claude Code's settings.
func (m *machine) claudeReg(v variant.Identity) string {
	var s struct {
		Extra   map[string]json.RawMessage `json:"extraKnownMarketplaces"`
		Enabled map[string]json.RawMessage `json:"enabledPlugins"`
	}
	json.Unmarshal([]byte(readOptional(filepath.Join(m.home, ".claude", "settings.json"))), &s)
	return fmt.Sprintf("marketplace=%s plugin=%s", s.Extra[v.Marketplace], s.Enabled[v.PluginID()])
}

// codexReg is v's marketplace and plugin tables in Codex's config.toml.
func (m *machine) codexReg(v variant.Identity) string {
	text := readOptional(filepath.Join(m.home, ".codex", "config.toml"))
	var out []string
	for _, h := range []string{"[marketplaces." + v.Marketplace + "]", "[plugins.\"" + v.PluginID() + "\"]"} {
		i := strings.Index(text, h+"\n")
		if i < 0 {
			out = append(out, h+" absent")
			continue
		}
		rest := text[i+len(h)+1:]
		if j := strings.Index(rest, "\n["); j >= 0 {
			rest = rest[:j+1]
		}
		out = append(out, h+" "+strings.TrimSpace(rest))
	}
	return strings.Join(out, "; ")
}

func (m *machine) claudeEndpoint() string {
	var s struct{ Env map[string]string }
	json.Unmarshal([]byte(readOptional(filepath.Join(m.home, ".claude", "settings.json"))), &s)
	return s.Env["OTEL_EXPORTER_OTLP_ENDPOINT"]
}

func (m *machine) mustRun(bin string, extra []string, args ...string) result {
	m.t.Helper()
	r := m.run(bin, extra, args...)
	if r.Code != 0 {
		m.t.Fatalf("%s %v: exit %d\n%s%s", filepath.Base(bin), args, r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// The public kenaz (prod) and the staff kenaz-dev (dev) on one machine,
// both set up for Claude Code and Codex with their relays running. They
// keep their files the way the harness does: per Fleet environment, under
// one root, ~/.kenaz/plugin/<env>, whichever build talks to that
// environment. The host plugin names differ, each build sees the shared env
// variables, kenaz-dev refuses prod, and undoing one leaves the other
// exactly as it was.
func TestSideBySide(t *testing.T) {
	m := newMachine(t)
	var all []install
	for _, v := range []variant.Identity{variant.Kenaz, variant.KenazDev} {
		i := install{v: v, l: m.layout()}
		i.bin = m.installBundle(v.BundleName, built.bins[v.Name], pluginTree(t, v))
		all = append(all, i)
	}
	pub, dev := all[0], all[1]
	if pub.state() == dev.state() || filepath.Dir(pub.state()) != filepath.Dir(dev.state()) {
		t.Fatalf("state directories: %s, %s", pub.state(), dev.state())
	}

	// Both relays run at once, one per environment, on different ports.
	pst, dst := m.startRelay(pub), m.startRelay(dev)
	if pst.Port == dst.Port {
		t.Fatalf("both relays on port %d", pst.Port)
	}
	for _, c := range []struct {
		i  install
		st daemonstate.State
	}{{pub, pst}, {dev, dst}} {
		var st struct {
			Env    string
			Daemon struct {
				Running bool
				Port    int
			}
		}
		r := m.mustRun(c.i.bin, nil, "status", "--json")
		if err := json.Unmarshal([]byte(r.Stdout), &st); err != nil {
			t.Fatalf("%s status: %v\n%s", c.i.v.Name, err, r.Stdout)
		}
		if st.Env != c.i.v.DefaultEnv || !st.Daemon.Running || st.Daemon.Port != c.st.Port {
			t.Errorf("%s status reports %+v, want env %s on port %d", c.i.v.Name, st, c.i.v.DefaultEnv, c.st.Port)
		}
		var bi struct{ Variant string }
		json.Unmarshal([]byte(m.mustRun(c.i.bin, nil, "version", "--json").Stdout), &bi)
		if bi.Variant != c.i.v.Name {
			t.Errorf("%s version --json variant = %q", c.i.v.Name, bi.Variant)
		}
	}

	// One environment, one build at a time: kenaz --env dev while
	// kenaz-dev's dev relay runs is refused, naming kenaz-dev, and the
	// reverse. The env variable is shared, so KENAZ_ENV=stage moves both.
	for _, c := range []struct {
		i     install
		env   string
		other variant.Identity
	}{{pub, dev.v.DefaultEnv, dev.v}, {dev, pub.v.DefaultEnv, pub.v}} {
		if !c.i.v.Allowed(c.env) {
			continue
		}
		r := m.run(c.i.bin, nil, "status", "--env", c.env)
		if r.Code == 0 || !strings.Contains(r.Stderr, c.other.Name+" is using this Fleet environment") {
			t.Errorf("%s --env %s next to %s's relay: exit %d %q", c.i.v.Name, c.env, c.other.Name, r.Code, r.Stderr)
		}
	}
	for _, i := range all {
		var st struct{ Env string }
		json.Unmarshal([]byte(m.mustRun(i.bin, []string{variant.Shared.EnvVar + "=stage"}, "status", "--json").Stdout), &st)
		if st.Env != "stage" {
			t.Errorf("%s=stage left %s on %s", variant.Shared.EnvVar, i.v.Name, st.Env)
		}
	}
	// kenaz-dev refuses prod, however it is selected.
	for _, c := range []struct {
		env  []string
		args []string
	}{
		{nil, []string{"status", "--env", "prod"}},
		{nil, []string{"login", "--env=prod"}},
		{[]string{variant.Shared.EnvVar + "=prod"}, []string{"status"}},
		{[]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV=prod"}, []string{"doctor"}},
		{nil, []string{"setup", "claude", "--env", "prod", "--yes"}},
	} {
		r := m.run(dev.bin, c.env, c.args...)
		if r.Code != 64 || !strings.Contains(r.Stderr, "kenaz-dev connects only to dev or stage Fleet; use kenaz for prod") {
			t.Errorf("kenaz-dev %v %v: exit %d %q", c.env, c.args, r.Code, r.Stderr)
		}
	}

	// Set up both, public first.
	for _, i := range all {
		for _, h := range []string{"claude", "codex"} {
			m.mustRun(i.bin, nil, "setup", h, "--yes")
		}
	}
	for _, i := range all {
		if c := m.claudeReg(i.v); !strings.Contains(c, `"path":`) || !strings.Contains(c, "plugin=true") {
			t.Errorf("%s not registered in Claude Code: %s", i.v.Name, c)
		}
		if c := m.codexReg(i.v); strings.Contains(c, "absent") || !strings.Contains(c, "enabled = true") {
			t.Errorf("%s not registered in Codex: %s", i.v.Name, c)
		}
		if !strings.Contains(m.claudeReg(i.v), filepath.Join(i.state(), "setup", "bundle")) {
			t.Errorf("%s registered a plugin outside its environment's state: %s", i.v.Name, m.claudeReg(i.v))
		}
	}
	// Each host exports telemetry to one endpoint; the last setup took it.
	// (Choosing which build owns it is a separate change.)
	if got, want := m.claudeEndpoint(), fmt.Sprintf("http://127.0.0.1:%d", dst.Port); got != want {
		t.Errorf("Claude Code endpoint %s, want %s", got, want)
	}
	// kenaz cannot set up dev while kenaz-dev uses it.
	if r := m.run(pub.bin, nil, "setup", "claude", "--env", "dev", "--yes"); r.Code == 0 {
		t.Errorf("kenaz set up dev over kenaz-dev's setup: %q", r.Stdout)
	}

	// Undoing kenaz-dev leaves kenaz byte-identical and gives telemetry
	// back to its relay.
	pubBefore := m.snapshot(pub)
	for _, h := range []string{"claude", "codex"} {
		m.mustRun(dev.bin, nil, "setup", "--undo", h, "--yes")
	}
	if got := m.snapshot(pub); got != pubBefore {
		t.Errorf("kenaz-dev --undo changed kenaz:\n--- before\n%s\n--- after\n%s", pubBefore, got)
	}
	if c := m.claudeReg(dev.v) + " " + m.codexReg(dev.v); !strings.Contains(c, "marketplace=null plugin=null") || strings.Count(c, "absent") != 2 {
		t.Errorf("kenaz-dev still registered: %s", c)
	}
	if got, want := m.claudeEndpoint(), fmt.Sprintf("http://127.0.0.1:%d", pst.Port); got != want {
		t.Errorf("after kenaz-dev --undo, Claude Code endpoint %s, want %s", got, want)
	}

	// And the reverse: undoing kenaz leaves kenaz-dev byte-identical.
	for _, h := range []string{"claude", "codex"} {
		m.mustRun(dev.bin, nil, "setup", h, "--yes")
	}
	devBefore := m.snapshot(dev)
	for _, h := range []string{"claude", "codex"} {
		m.mustRun(pub.bin, nil, "setup", "--undo", h, "--yes")
	}
	if got := m.snapshot(dev); got != devBefore {
		t.Errorf("kenaz --undo changed kenaz-dev:\n--- before\n%s\n--- after\n%s", devBefore, got)
	}
	if c := m.claudeReg(pub.v) + " " + m.codexReg(pub.v); !strings.Contains(c, "marketplace=null plugin=null") || strings.Count(c, "absent") != 2 {
		t.Errorf("kenaz still registered: %s", c)
	}
	if got, want := m.claudeEndpoint(), fmt.Sprintf("http://127.0.0.1:%d", dst.Port); got != want {
		t.Errorf("after kenaz --undo, Claude Code endpoint %s, want kenaz-dev's %s", got, want)
	}
}

// A build never registers another variant's bundle.
func TestBundleOfOtherVariantRefused(t *testing.T) {
	m := newMachine(t)
	for _, c := range []struct{ bin, tree variant.Identity }{{variant.Kenaz, variant.KenazDev}, {variant.KenazDev, variant.Kenaz}} {
		// The other variant's plugin tree in this variant's bundle.
		bin := m.installBundle(c.bin.BundleName, built.bins[c.bin.Name], pluginTree(t, c.tree))
		m.prepare(m.layout(), c.bin.DefaultEnv, variant.Shared.RelayPorts[0])
		before := readOptional(m.log)
		r := m.run(bin, nil, "setup", "claude", "--yes")
		if r.Code == 0 || !strings.Contains(r.Stderr, "registers only "+c.bin.BundleName+" bundles") {
			t.Errorf("%s with a %s plugin tree: exit %d %q", c.bin.Name, c.tree.Name, r.Code, r.Stderr)
		}
		// Neither a host command nor the settings file registered it.
		if log := strings.TrimPrefix(readOptional(m.log), before); strings.Contains(log, "marketplace add") ||
			strings.Contains(readOptional(filepath.Join(m.home, ".claude", "settings.json")), c.tree.PluginID()) {
			t.Errorf("%s registered %s: %s", c.bin.Name, c.tree.PluginID(), log)
		}
	}
}
