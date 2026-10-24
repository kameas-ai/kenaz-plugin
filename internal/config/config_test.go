package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveEnvPrecedence(t *testing.T) {
	file := File{Values: map[string]string{"env": "stage"}}
	cases := []struct {
		name    string
		flag    string
		vars    map[string]string
		file    File
		want    fleetapi.Env
		src     Source
		wantErr bool
	}{
		{"default", "", nil, File{}, fleetapi.EnvProd, SourceDefault, false},
		{"file", "", nil, file, fleetapi.EnvStage, SourceFile, false},
		{"var beats file", "", map[string]string{"KENAZ_ENV": "dev"}, file, fleetapi.EnvDev, SourceEnvVar, false},
		{"spec var name", "", map[string]string{"FLEET_PLUGIN_ENV": "dev"}, file, fleetapi.EnvDev, SourceEnvVar, false},
		{"flag beats all", "prod", map[string]string{"KENAZ_ENV": "dev"}, file, fleetapi.EnvProd, SourceFlag, false},
		{"codex placeholder skipped", "", map[string]string{"KENAZ_ENV": "${user_config.env}"}, file, fleetapi.EnvStage, SourceFile, false},
		{"placeholder everywhere", "${x}", map[string]string{"KENAZ_ENV": "${y}"}, File{Values: map[string]string{"env": "${z}"}}, fleetapi.EnvProd, SourceDefault, false},
		{"case-insensitive", "DEV", nil, File{}, fleetapi.EnvDev, SourceFlag, false},
		{"unknown is error", "", map[string]string{"KENAZ_ENV": "qa"}, file, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, src, err := ResolveEnv(c.flag, envMap(c.vars), c.file)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if got != c.want || src != c.src {
				t.Fatalf("got (%q,%q), want (%q,%q)", got, src, c.want, c.src)
			}
		})
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	if f, err := LoadFile(filepath.Join(dir, "missing.toml")); err != nil || len(f.Values) != 0 {
		t.Fatalf("missing file: %v %v", f, err)
	}
	p := filepath.Join(dir, "config.toml")
	os.WriteFile(p, []byte("# comment\nenv = \"dev\"\nservice = resident # trailing\n\n"), 0o600)
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Get("env") != "dev" || f.Get("service") != "resident" {
		t.Fatalf("values = %v", f.Values)
	}
	os.WriteFile(p, []byte("garbage\n"), 0o600)
	if _, err := LoadFile(p); err == nil {
		t.Fatal("want parse error")
	}
}

func TestPathsFor(t *testing.T) {
	home := func() (string, error) { return "/home/u", nil }
	ucfg := func() (string, error) { return "/Users/u/Library/Application Support", nil }
	p, _ := pathsFor("darwin", envMap(nil), home, ucfg)
	if p.ConfigDir != "/Users/u/Library/Application Support/kameas/kenaz" ||
		p.State(fleetapi.EnvDev) != "/Users/u/Library/Application Support/kameas/kenaz/state/dev" {
		t.Fatalf("darwin = %+v", p)
	}
	p, _ = pathsFor("linux", envMap(nil), home, ucfg)
	if p.ConfigDir != "/home/u/.config/kameas/kenaz" || p.StateRoot != "/home/u/.local/state/kameas/kenaz" {
		t.Fatalf("linux = %+v", p)
	}
	p, _ = pathsFor("linux", envMap(map[string]string{"XDG_STATE_HOME": "/s"}), home, ucfg)
	if p.StateRoot != "/s/kameas/kenaz" {
		t.Fatalf("xdg = %+v", p)
	}
	p, _ = pathsFor("darwin", envMap(map[string]string{"KENAZ_HOME": "/h"}), home, ucfg)
	if p.ConfigDir != "/h/config" || p.StateRoot != "/h/state" {
		t.Fatalf("override = %+v", p)
	}
	bad := func() (string, error) { return "", errors.New("no home") }
	if _, err := pathsFor("linux", envMap(nil), bad, ucfg); err == nil {
		t.Fatal("want error")
	}
}

func TestSetValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "config.toml")
	if err := SetValue(path, "memory", "off"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("# mine\nenv = \"dev\"\nmemory = \"off\"\n"), 0o600)
	if err := SetValue(path, "memory", "on"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "# mine\nenv = \"dev\"\nmemory = \"on\"\n" {
		t.Fatalf("got %q", b)
	}
	f, err := LoadFile(path)
	if err != nil || f.Get("memory") != "on" || f.Get("env") != "dev" {
		t.Fatalf("reload: %+v %v", f, err)
	}
	if err := SetValue(path, "bad\"key", "x"); err == nil {
		t.Fatal("invalid key accepted")
	}
}

// Hook commands get no .mcp.json env block; Claude Code exports the
// plugin's fleet_env option as CLAUDE_PLUGIN_OPTION_FLEET_ENV, so hooks
// resolve the same environment as the MCP server (the banner's
// approval link opens the selected environment's dashboard).
func TestResolveEnvPluginOption(t *testing.T) {
	for _, c := range []struct {
		name string
		vars map[string]string
		want fleetapi.Env
	}{
		{"hook: only the exported option", map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, fleetapi.EnvStage},
		{"KENAZ_ENV wins", map[string]string{"KENAZ_ENV": "dev", "CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, fleetapi.EnvDev},
		{"placeholder KENAZ_ENV skipped", map[string]string{"KENAZ_ENV": "${user_config.fleet_env}", "CLAUDE_PLUGIN_OPTION_FLEET_ENV": "dev"}, fleetapi.EnvDev},
		{"option wins over legacy", map[string]string{"FLEET_PLUGIN_ENV": "stage", "CLAUDE_PLUGIN_OPTION_FLEET_ENV": "dev"}, fleetapi.EnvDev},
	} {
		got, src, err := ResolveEnv("", envMap(c.vars), File{Values: map[string]string{"env": "prod"}})
		if err != nil || got != c.want || src != SourceEnvVar {
			t.Errorf("%s: %s %s %v", c.name, got, src, err)
		}
	}
	if _, _, err := ResolveEnv("", envMap(map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "qa"}), File{}); err == nil {
		t.Error("an unknown option value must be an error, not a silent prod fallback")
	}
}

// on Windows the state lives in %LocalAppData% (machine-local, never
// roams); config.toml stays in %AppData%, and the old state root is recorded
// for the migration.
func TestPathsForWindows(t *testing.T) {
	home := func() (string, error) { return filepath.Join("C:", "Users", "u"), nil }
	ucfg := func() (string, error) { return filepath.Join("C:", "Users", "u", "AppData", "Roaming"), nil }
	p, err := pathsFor("windows", envMap(map[string]string{"LocalAppData": filepath.Join("C:", "Users", "u", "AppData", "Local")}), home, ucfg)
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		ConfigDir:       filepath.Join("C:", "Users", "u", "AppData", "Roaming", "kameas", "kenaz"),
		StateRoot:       filepath.Join("C:", "Users", "u", "AppData", "Local", "kameas", "kenaz", "state"),
		LegacyStateRoot: filepath.Join("C:", "Users", "u", "AppData", "Roaming", "kameas", "kenaz", "state"),
	}
	if p != want {
		t.Fatalf("got %+v\nwant %+v", p, want)
	}
	// No LocalAppData in the environment: derived from the home directory.
	p, _ = pathsFor("windows", envMap(nil), home, ucfg)
	if p.StateRoot != want.StateRoot {
		t.Fatalf("fallback: %s", p.StateRoot)
	}
	// KENAZ_HOME still overrides everything, with no migration.
	p, _ = pathsFor("windows", envMap(map[string]string{"KENAZ_HOME": "h"}), home, ucfg)
	if p.LegacyStateRoot != "" {
		t.Fatalf("KENAZ_HOME: %+v", p)
	}
}

func TestMigrateLegacyState(t *testing.T) {
	setup := func(t *testing.T) Paths {
		root := t.TempDir()
		return Paths{
			ConfigDir:       filepath.Join(root, "Roaming", "kameas", "kenaz"),
			StateRoot:       filepath.Join(root, "Local", "kameas", "kenaz", "state"),
			LegacyStateRoot: filepath.Join(root, "Roaming", "kameas", "kenaz", "state"),
		}
	}
	write := func(t *testing.T, p string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("nothing to move", func(t *testing.T) {
		p := setup(t)
		if got := p.MigrateLegacyState(); got != p {
			t.Fatalf("%+v", got)
		}
		if _, err := os.Stat(p.StateRoot); !os.IsNotExist(err) {
			t.Fatal("created the state root")
		}
	})

	t.Run("moves legacy state", func(t *testing.T) {
		p := setup(t)
		write(t, filepath.Join(p.LegacyStateRoot, "prod", "setup-state.json"))
		write(t, filepath.Join(p.LegacyStateRoot, "credentials", "STORE"))
		got := p.MigrateLegacyState()
		if got.StateRoot != p.StateRoot {
			t.Fatalf("%+v", got)
		}
		for _, rel := range []string{"prod/setup-state.json", "credentials/STORE"} {
			if _, err := os.Stat(filepath.Join(p.StateRoot, filepath.FromSlash(rel))); err != nil {
				t.Fatalf("%s not moved: %v", rel, err)
			}
		}
		if _, err := os.Stat(p.LegacyStateRoot); !os.IsNotExist(err) {
			t.Fatal("legacy root left behind")
		}
		// Idempotent.
		if again := got.MigrateLegacyState(); again.StateRoot != p.StateRoot {
			t.Fatalf("second run: %+v", again)
		}
	})

	t.Run("new state wins", func(t *testing.T) {
		p := setup(t)
		write(t, filepath.Join(p.LegacyStateRoot, "prod", "old"))
		write(t, filepath.Join(p.StateRoot, "prod", "new"))
		if got := p.MigrateLegacyState(); got.StateRoot != p.StateRoot {
			t.Fatalf("%+v", got)
		}
		if _, err := os.Stat(filepath.Join(p.LegacyStateRoot, "prod", "old")); err != nil {
			t.Fatal("legacy state touched")
		}
	})

	t.Run("rename fails: keep using legacy", func(t *testing.T) {
		p := setup(t)
		write(t, filepath.Join(p.LegacyStateRoot, "prod", "x"))
		// The new root's parent is a file, so neither MkdirAll nor the
		// rename can succeed (as when Windows refuses to move open files).
		write(t, filepath.Dir(p.StateRoot))
		if got := p.MigrateLegacyState(); got.StateRoot != p.LegacyStateRoot {
			t.Fatalf("%+v", got)
		}
	})
}

// kenaz mcp and the SessionStart hook start together after an upgrade, so
// two processes migrate at once. Both must end up on the same, moved state.
func TestMigrateLegacyStateConcurrent(t *testing.T) {
	base := t.TempDir()
	runs := 2000
	if testing.Short() {
		runs = 200
	}
	for i := 0; i < runs; i++ {
		root := filepath.Join(base, fmt.Sprint(i))
		p := Paths{
			StateRoot:       filepath.Join(root, "Local", "kameas", "kenaz", "state"),
			LegacyStateRoot: filepath.Join(root, "Roaming", "kameas", "kenaz", "state"),
		}
		os.MkdirAll(filepath.Join(p.LegacyStateRoot, "prod"), 0o700)
		os.WriteFile(filepath.Join(p.LegacyStateRoot, "prod", "f"), []byte("x"), 0o600)
		var wg sync.WaitGroup
		got := make([]string, 2)
		start := make(chan struct{})
		for g := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got[g] = p.MigrateLegacyState().StateRoot
			}()
		}
		close(start)
		wg.Wait()
		if got[0] != p.StateRoot || got[1] != p.StateRoot {
			t.Fatalf("run %d: split state %v", i, got)
		}
		if _, err := os.Stat(filepath.Join(p.StateRoot, "prod", "f")); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}
