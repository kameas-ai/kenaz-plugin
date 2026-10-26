package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveEnvPrecedence(t *testing.T) {
	id := variant.Current
	// other is an allowed env that is not the default; file a third value
	// (stage) where the build allows it.
	def := fleetapi.Env(id.DefaultEnv)
	other := fleetapi.EnvDev
	if def == fleetapi.EnvDev {
		other = fleetapi.EnvStage
	}
	file := File{Values: map[string]string{"env": "stage"}}
	ev := variant.Shared.EnvVar
	cases := []struct {
		name    string
		flag    string
		vars    map[string]string
		file    File
		want    fleetapi.Env
		src     Source
		wantErr bool
	}{
		{"default", "", nil, File{}, def, SourceDefault, false},
		{"file", "", nil, file, fleetapi.EnvStage, SourceFile, false},
		{"var beats file", "", map[string]string{ev: "dev"}, file, fleetapi.EnvDev, SourceEnvVar, false},
		{"flag beats all", string(def), map[string]string{ev: string(other)}, file, def, SourceFlag, false},
		{"codex placeholder skipped", "", map[string]string{ev: "${user_config.env}"}, file, fleetapi.EnvStage, SourceFile, false},
		{"placeholder everywhere", "${x}", map[string]string{ev: "${y}"}, File{Values: map[string]string{"env": "${z}"}}, def, SourceDefault, false},
		{"case-insensitive", "DEV", nil, File{}, fleetapi.EnvDev, SourceFlag, false},
		{"unknown is error", "", map[string]string{ev: "qa"}, file, "", "", true},
	}
	if id.Name == variant.Kenaz.Name {
		cases = append(cases, struct {
			name    string
			flag    string
			vars    map[string]string
			file    File
			want    fleetapi.Env
			src     Source
			wantErr bool
		}{"spec var name", "", map[string]string{"FLEET_PLUGIN_ENV": "dev"}, file, fleetapi.EnvDev, SourceEnvVar, false})
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

// An env the build does not connect to is refused at every level, never
// replaced by the default; the public build allows every env.
func TestResolveEnvRefusesDisallowed(t *testing.T) {
	id := variant.Current
	for _, e := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvStage, fleetapi.EnvDev} {
		allowed := id.Allowed(string(e))
		for _, c := range []struct {
			src  Source
			flag string
			vars map[string]string
			file File
		}{
			{SourceFlag, string(e), nil, File{}},
			{SourceEnvVar, "", map[string]string{variant.Shared.EnvVar: string(e)}, File{}},
			{SourceEnvVar, "", map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": string(e)}, File{}},
			{SourceFile, "", nil, File{Values: map[string]string{"env": string(e)}}},
		} {
			got, src, err := ResolveEnv(c.flag, envMap(c.vars), c.file)
			switch {
			case allowed && (err != nil || got != e || src != c.src):
				t.Errorf("%s from %s: %q %q %v", e, c.src, got, src, err)
			case !allowed && !errors.Is(err, ErrEnvNotAllowed):
				t.Errorf("%s from %s: want ErrEnvNotAllowed, got %q %v", e, c.src, got, err)
			case !allowed && !strings.Contains(err.Error(), id.Binary+" connects only to "):
				t.Errorf("%s from %s: message %q", e, c.src, err)
			}
		}
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

// The layout is ~/.kenaz/plugin on every platform and for every build;
// each platform's earlier layout is recorded for the migration.
func TestPathsFor(t *testing.T) {
	home := func() (string, error) { return "/home/u", nil }
	ucfg := func() (string, error) { return "/Users/u/Library/Application Support", nil }
	root := filepath.FromSlash("/home/u/.kenaz/plugin")
	for _, c := range []struct {
		goos   string
		env    map[string]string
		legacy Legacy
	}{
		{"darwin", nil, Legacy{
			ConfigDir:  "/Users/u/Library/Application Support/kameas/kenaz",
			StateRoots: []string{"/Users/u/Library/Application Support/kameas/kenaz/state"},
		}},
		{"linux", nil, Legacy{ConfigDir: "/home/u/.config/kameas/kenaz", StateRoots: []string{"/home/u/.local/state/kameas/kenaz"}}},
		{"linux", map[string]string{"XDG_STATE_HOME": "/s", "XDG_CONFIG_HOME": "/c"}, Legacy{ConfigDir: "/c/kameas/kenaz", StateRoots: []string{"/s/kameas/kenaz"}}},
		{"windows", map[string]string{"LocalAppData": "/L"}, Legacy{
			ConfigDir:  "/Users/u/Library/Application Support/kameas/kenaz",
			StateRoots: []string{"/L/kameas/kenaz/state", "/Users/u/Library/Application Support/kameas/kenaz/state"},
		}},
		{"windows", nil, Legacy{
			ConfigDir:  "/Users/u/Library/Application Support/kameas/kenaz",
			StateRoots: []string{"/home/u/AppData/Local/kameas/kenaz/state", "/Users/u/Library/Application Support/kameas/kenaz/state"},
		}},
	} {
		p, err := pathsFor(c.goos, envMap(c.env), home, ucfg)
		if err != nil {
			t.Fatal(err)
		}
		want := Paths{ConfigDir: root, StateRoot: root, Legacy: slashLegacy(c.legacy)}
		if fmt.Sprint(p) != fmt.Sprint(want) {
			t.Errorf("%s %v:\n got %+v\nwant %+v", c.goos, c.env, p, want)
		}
		if got := p.State(fleetapi.EnvDev); got != filepath.Join(root, "dev") {
			t.Errorf("%s: dev state %s", c.goos, got)
		}
	}
	// KENAZ_HOME replaces ~/.kenaz, as it does for the archive installer;
	// its earlier layout was $KENAZ_HOME/{config,state}.
	p, _ := pathsFor("darwin", envMap(map[string]string{variant.Shared.HomeVar: "/h"}), home, ucfg)
	h, _ := filepath.Abs(filepath.FromSlash("/h")) // a drive letter on Windows
	want := Paths{ConfigDir: filepath.Join(h, "plugin"), StateRoot: filepath.Join(h, "plugin"),
		Legacy: Legacy{ConfigDir: filepath.Join(h, "config"), StateRoots: []string{filepath.Join(h, "state")}}}
	if fmt.Sprint(p) != fmt.Sprint(want) {
		t.Fatalf("override = %+v", p)
	}
	bad := func() (string, error) { return "", errors.New("no home") }
	if _, err := pathsFor("linux", envMap(nil), bad, ucfg); err == nil {
		t.Fatal("want error")
	}
}

func slashLegacy(l Legacy) Legacy {
	out := Legacy{ConfigDir: filepath.FromSlash(l.ConfigDir)}
	for _, r := range l.StateRoots {
		out.StateRoots = append(out.StateRoots, filepath.FromSlash(r))
	}
	return out
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

// SetValue through the link Migrate leaves at the old config.toml writes
// the file in use, and the link stays.
func TestSetValueThroughLink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "plugin", "config.toml")
	link := filepath.Join(dir, "old", "config.toml")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.MkdirAll(filepath.Dir(link), 0o700)
	os.WriteFile(real, []byte("env = \"prod\"\n"), 0o600)
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if err := SetValue(link, "memory", "off"); err != nil {
		t.Fatal(err)
	}
	if f, _ := LoadFile(real); f.Get("memory") != "off" || f.Get("env") != "prod" {
		t.Fatalf("the file in use: %v", f.Values)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
	if exists(real+".tmp") || exists(link+".tmp") {
		t.Fatal("temp file left")
	}
}

// Hook commands get no .mcp.json env block; Claude Code exports the
// plugin's fleet_env option as CLAUDE_PLUGIN_OPTION_FLEET_ENV, so hooks
// resolve the same environment as the MCP server (the banner's
// approval link opens the selected environment's dashboard).
func TestResolveEnvPluginOption(t *testing.T) {
	ev := variant.Shared.EnvVar
	for _, c := range []struct {
		name string
		vars map[string]string
		want fleetapi.Env
	}{
		{"hook: only the exported option", map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, fleetapi.EnvStage},
		{"env var wins", map[string]string{ev: "dev", "CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, fleetapi.EnvDev},
		{"placeholder env var skipped", map[string]string{ev: "${user_config.fleet_env}", "CLAUDE_PLUGIN_OPTION_FLEET_ENV": "dev"}, fleetapi.EnvDev},
		{"option wins over legacy", map[string]string{"FLEET_PLUGIN_ENV": "stage", "CLAUDE_PLUGIN_OPTION_FLEET_ENV": "dev"}, fleetapi.EnvDev},
	} {
		got, src, err := ResolveEnv("", envMap(c.vars), File{Values: map[string]string{"env": variant.Current.DefaultEnv}})
		if err != nil || got != c.want || src != SourceEnvVar {
			t.Errorf("%s: %s %s %v", c.name, got, src, err)
		}
	}
	if _, _, err := ResolveEnv("", envMap(map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "qa"}), File{}); err == nil {
		t.Error("an unknown option value must be an error, not a silent prod fallback")
	}
}
