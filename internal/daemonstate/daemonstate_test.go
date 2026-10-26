package daemonstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

func resolver(env map[string]string, goos string) Resolver {
	return Resolver{Home: "/home/u", GOOS: goos, Getenv: func(k string) string { return env[k] }}
}

func TestEnv(t *testing.T) {
	def := variant.Current.DefaultEnv
	cases := map[string]string{
		"":                         def,
		"dev":                      "dev",
		"stage":                    "stage",
		"${user_config.fleet_env}": def,
		"bogus":                    "error",
	}
	// A known env the build does not connect to is an error, as it is for
	// config.ResolveEnv (the staff dev build refuses prod).
	if !variant.Current.Allowed("prod") {
		cases["prod"] = "error"
	} else {
		cases["prod"] = "prod"
	}
	for in, want := range cases {
		got, err := resolver(map[string]string{variant.Shared.EnvVar: in}, "linux").Env()
		if err != nil {
			got = "error"
		}
		if got != want {
			t.Errorf("Env(%q) = %q (%v), want %q", in, got, err, want)
		}
	}
	// Every env variable selects it, in order, like config.ResolveEnv.
	if got, _ := resolver(map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, "linux").Env(); got != "stage" {
		t.Errorf("plugin option: %q", got)
	}
}

// The state directory is ~/.kenaz/plugin/<env> on every platform and for
// every build, or $KENAZ_HOME/plugin/<env>.
func TestStateDir(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		if got, want := resolver(map[string]string{"XDG_STATE_HOME": "/x", "LocalAppData": "/l"}, goos).StateDir("dev"), filepath.FromSlash("/home/u/.kenaz/plugin/dev"); got != want {
			t.Errorf("%s: got %q want %q", goos, got, want)
		}
	}
	if got, want := resolver(map[string]string{variant.Shared.HomeVar: "/kh"}, "linux").StateDir("prod"), filepath.FromSlash("/kh/plugin/prod"); got != want {
		t.Errorf("KENAZ_HOME: got %q want %q", got, want)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	if _, err := Read(dir); !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("missing file: got %v", err)
	}
	os.WriteFile(filepath.Join(dir, FileName), []byte(`{"pid":1,"port":4318,"version":"1.0.0","nonce":"n"}`), 0o600)
	s, err := Read(dir)
	if err != nil || s.Port != 4318 || s.Nonce != "n" {
		t.Fatalf("got %+v %v", s, err)
	}
	if s.BaseURL() != "http://127.0.0.1:4318" {
		t.Fatal(s.BaseURL())
	}
	os.WriteFile(filepath.Join(dir, FileName), []byte(`{"port":0}`), 0o600)
	if _, err := Read(dir); err == nil {
		t.Fatal("port 0 accepted")
	}
}

func TestSessionHash(t *testing.T) {
	h := SessionHash("abc")
	if len(h) != 32 || h == "abc" || h != SessionHash("abc") || h == SessionHash("abd") {
		t.Fatalf("bad hash %q", h)
	}
}
