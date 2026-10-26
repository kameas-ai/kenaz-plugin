package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/logx"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// An upgrade from a release that kept its files in $KENAZ_HOME/{config,
// state} (the platform directories without KENAZ_HOME are handled the same
// way, see internal/config). Every command keeps using the old files where
// they are; nothing moves on its own. `kenaz migrate` refuses while the old
// relay runs, then moves everything once it has stopped, keeps the
// credential store choice, config and queue, and the startup check names
// `kenaz init` for the registration that still uses the old plugin path.
func TestUpgradeKeepsFilesUntilMigrate(t *testing.T) {
	h := newHarness(t, stub.New())
	oldState := filepath.Join(h.home, "state")
	oldDev := filepath.Join(oldState, "dev")
	root := filepath.Join(h.home, "plugin")
	write := func(rel, c string) {
		t.Helper()
		p := filepath.Join(h.home, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config/config.toml", "memory = \"off\"\n")
	if err := credstore.WriteMarker(filepath.Join(oldState, "credentials"), credstore.Files); err != nil {
		t.Fatal(err)
	}
	write("state/dev/spool/queued", "batch")
	launcher := filepath.Join(h.home, "bin", "kenaz")
	write("bin/kenaz", "")
	rendered := filepath.Join(oldDev, "setup", "bundle", "kenaz_1")
	write("state/dev/setup/bundle/kenaz_1/plugin/.mcp.json", "{}")
	st, _ := json.Marshal(map[string]any{"version": 1, "variant": variant.Current.Name, "claude": map[string]any{"path": "x", "keys": map[string]any{},
		"bundle": map[string]any{"root": h.home, "version": "1", "rendered": rendered, "digest": "d", "launcher": launcher}}})
	write("state/dev/setup-state.json", string(st))

	// Every command uses the old files in place.
	if _, out, errb := h.run(t, "status"); !strings.Contains(out, "memory: off") || strings.Contains(errb, "moved") {
		t.Fatalf("status before migrate: %s %q", out, errb)
	}
	if _, out, _ := h.run(t, "doctor"); !strings.Contains(out, "files (no OS keychain) in "+filepath.Join(oldState, "credentials")) || !strings.Contains(out, "migrate` with your AI apps closed") {
		t.Fatalf("doctor before migrate:\n%s", out)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("something moved without kenaz migrate")
	}

	// The relay of the release before the upgrade is running.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.New(daemon.Options{Provider: stub.New(), Env: fleetapi.EnvDev, StateDir: oldDev, Version: "old",
			Logger: logx.Discard(), Ports: []int{0}, Host: enroll.Host{GOOS: "linux", GOARCH: "amd64"}}).Run(ctx)
	}()
	waitForRelay(t, oldDev)
	if _, _, errb := h.run(t, "hook", "session-end"); errb != "" {
		t.Fatalf("hook: %q", errb)
	}
	code, out, _ := h.run(t, "migrate")
	if code == 0 || !strings.Contains(out, "close Claude Code, Codex and Claude Desktop") || !strings.Contains(out, "keeps working") {
		t.Fatalf("migrate while the relay runs: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(oldDev, "spool", "queued")); err != nil {
		t.Fatal("moved files under a running relay")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("old relay: %v", err)
	}

	code, out, errb := h.run(t, "migrate")
	if code != 0 || !strings.Contains(out, "moved its files to "+root) || !strings.Contains(out, "init") {
		t.Fatalf("migrate: %d %s %q", code, out, errb)
	}
	for _, rel := range []string{"config.toml", "credentials/" + credstore.MarkerName, "dev/spool/queued", "dev/setup-state.json"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s not moved: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rendered, "plugin", ".mcp.json")); err != nil {
		t.Error("the old plugin path no longer works")
	}
	if _, err := os.Stat(filepath.Join(h.home, "config", config.Breadcrumb)); err != nil {
		t.Error("no breadcrumb in the old config directory")
	}
	_, out, errb = h.run(t, "status")
	if !strings.Contains(out, "memory: off") || !strings.Contains(errb, "still uses its files from") {
		t.Errorf("status after migrate: %s %q", out, errb)
	}
	if _, out, _ := h.run(t, "doctor"); !strings.Contains(out, "files (no OS keychain) in "+filepath.Join(root, "credentials")) || strings.Contains(out, "migrate` with your AI apps closed") {
		t.Errorf("doctor after migrate:\n%s", out)
	}
	if code, out, _ := h.run(t, "migrate"); code != 0 || !strings.Contains(out, "Nothing to move") {
		t.Errorf("second migrate: %d %s", code, out)
	}
}

func waitForRelay(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := daemon.ConnectAny(context.Background(), dir); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("relay did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Another build's relay holds the environment: mcp still serves (the
// handshake never fails) with the relay left alone, doctor diagnoses it,
// and other commands refuse with the build's name.
func TestOtherBuildRelayInEnv(t *testing.T) {
	h := newHarness(t, stub.New())
	dir := h.state()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.New(daemon.Options{Provider: stub.New(), Env: fleetapi.EnvDev, StateDir: dir, Version: "other",
			Logger: logx.Discard(), Ports: []int{0}, Host: enroll.Host{GOOS: "linux", GOARCH: "amd64"}}).Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	waitForRelay(t, dir)
	in, err := daemon.ReadInfo(dir)
	if err != nil {
		t.Fatal(err)
	}
	other := variant.KenazDev.Name
	if variant.Current.Name == other {
		other = variant.Kenaz.Name
	}
	in.Variant = other
	if other == variant.Kenaz.Name {
		in.Variant = ""
	}
	if err := daemon.WriteInfo(dir, in); err != nil {
		t.Fatal(err)
	}
	msg := other + " is using this Fleet environment"

	// mcp serves, and a Fleet tool that would otherwise work (signed in,
	// catalog answering) reports the conflict.
	p := &catalogFleet{official: &official{Provider: stub.New()}}
	p.signedIn.Store(true)
	h.p = p
	served := false
	var toolText string
	var out, errb bytes.Buffer
	env := h.env(context.Background(), &out, &errb)
	env.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
		served = true
		ct, st := sdk.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			return err
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			return err
		}
		defer cs.Close()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "skills_search", Arguments: map[string]any{"query": "x"}})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(res)
		toolText = string(raw)
		return nil
	}
	if code := app.RunEnv(env, []string{"mcp"}); code != 0 || !served || !strings.Contains(errb.String(), msg) {
		t.Fatalf("mcp: %d served=%v %q", code, served, errb.String())
	}
	if !strings.Contains(toolText, msg) || strings.Contains(toolText, "catalog-skill") {
		t.Fatalf("skills_search with the environment in use by %s: %s", other, toolText)
	}
	if code, out, _ := h.run(t, "doctor"); code == 0 || !strings.Contains(out, "[FAIL] relay") || !strings.Contains(out, msg) || !strings.Contains(out, "state directory") {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	if code, _, errb := h.run(t, "status"); code == 0 || !strings.Contains(errb, msg) {
		t.Fatalf("status: %d %q", code, errb)
	}
}

// catalogFleet is a signed-in official provider whose catalog answers.
type catalogFleet struct{ *official }

func (p *catalogFleet) Catalog() fleetapi.Catalog { return answeringCatalog{} }

type answeringCatalog struct{}

func (answeringCatalog) List(context.Context, fleetapi.CatalogQuery) ([]fleetapi.CatalogEntry, error) {
	return []fleetapi.CatalogEntry{{ID: "c1", Slug: "catalog-skill", Kind: fleetapi.CatalogKindAgentSkill}}, nil
}
func (answeringCatalog) Get(context.Context, string, string) (fleetapi.CatalogItem, error) {
	return fleetapi.CatalogItem{}, nil
}
func (answeringCatalog) Publish(context.Context, fleetapi.PublishRequest) (fleetapi.CatalogEntry, error) {
	return fleetapi.CatalogEntry{}, nil
}

// The root already has its files, so an earlier release's state is not
// used; it still holds that release's memories and sign-in data, and an app
// may still load the plugin from it. doctor and migrate never call it
// deletable before `kenaz init`, and not at all while a recorded
// registration points into it.
func TestOldStateNotDeletableWhileRegistered(t *testing.T) {
	h := newHarness(t, stub.New())
	oldState := filepath.Join(h.home, "state")
	write := func(rel, c string) {
		t.Helper()
		p := filepath.Join(h.home, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("plugin/dev/consent.json", "{}")
	write("state/memory/memory.db", "notes")
	rendered := filepath.Join(oldState, "dev", "setup", "bundle", "kenaz_1")
	write("state/dev/setup/bundle/kenaz_1/plugin/.mcp.json", "{}")
	st, _ := json.Marshal(map[string]any{"version": 1, "variant": variant.Current.Name, "claude": map[string]any{"path": "x", "keys": map[string]any{},
		"bundle": map[string]any{"root": h.home, "version": "1", "rendered": rendered, "digest": "d"}}})
	write("state/dev/setup-state.json", string(st))

	line := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, oldState) {
				return l
			}
		}
		return ""
	}
	_, out, _ := h.run(t, "doctor")
	if l := line(out); !strings.Contains(l, "do not delete it") || !strings.Contains(l, "init` first") || !strings.Contains(l, "memories and sign-in data") {
		t.Fatalf("doctor, registered:\n%s", out)
	}
	_, out, _ = h.run(t, "migrate")
	if l := line(out); !strings.Contains(l, "do not delete it") {
		t.Fatalf("migrate, registered:\n%s", out)
	}

	// After `kenaz init` from the root: the root's record registers the
	// app from the root; the old record, never rewritten, no longer counts.
	newRendered := filepath.Join(h.home, "plugin", "dev", "setup", "bundle", "kenaz_2")
	st2, _ := json.Marshal(map[string]any{"version": 1, "variant": variant.Current.Name, "claude": map[string]any{"path": "x", "keys": map[string]any{},
		"bundle": map[string]any{"root": h.home, "version": "2", "rendered": newRendered, "digest": "d"}}})
	write("plugin/dev/setup-state.json", string(st2))
	_, out, _ = h.run(t, "doctor")
	if l := line(out); strings.Contains(l, "do not delete it") || strings.Contains(l, "init` first") || !strings.Contains(l, "no longer used") || !strings.Contains(l, "memories and sign-in data") {
		t.Fatalf("doctor, after init:\n%s", out)
	}
	os.Remove(filepath.Join(h.home, "plugin", "dev", "setup-state.json"))

	os.Remove(filepath.Join(oldState, "dev", "setup-state.json"))
	_, out, _ = h.run(t, "doctor")
	if l := line(out); strings.Contains(l, "do not delete it") || !strings.Contains(l, "init` first, then delete it") || !strings.Contains(l, "memories and sign-in data") {
		t.Fatalf("doctor, not registered:\n%s", out)
	}
}
