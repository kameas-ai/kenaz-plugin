package config

import (
	"errors"
	"os"
	"path/filepath"
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
