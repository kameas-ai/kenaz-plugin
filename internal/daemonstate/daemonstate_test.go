package daemonstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func resolver(env map[string]string, goos string) Resolver {
	return Resolver{Home: "/home/u", GOOS: goos, Getenv: func(k string) string { return env[k] }}
}

func TestEnv(t *testing.T) {
	cases := map[string]string{
		"":                         "prod",
		"dev":                      "dev",
		"stage":                    "stage",
		"${user_config.fleet_env}": "prod",
		"bogus":                    "prod",
	}
	for in, want := range cases {
		if got := resolver(map[string]string{"KENAZ_ENV": in}, "linux").Env(); got != want {
			t.Errorf("Env(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStateDir(t *testing.T) {
	cases := []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"darwin", nil, "/home/u/Library/Application Support/kameas/kenaz/state/dev"},
		{"linux", nil, "/home/u/.local/state/kameas/kenaz/dev"},
		{"linux", map[string]string{"XDG_STATE_HOME": "/x"}, "/x/kameas/kenaz/dev"},
	}
	for _, c := range cases {
		if got := resolver(c.env, c.goos).StateDir("dev"); got != filepath.FromSlash(c.want) {
			t.Errorf("%s: got %q want %q", c.goos, got, c.want)
		}
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

// the Windows state directory is %LocalAppData%\kameas\kenaz\state\<env>,
// the same place config.DefaultPaths uses.
func TestStateDirWindows(t *testing.T) {
	r := Resolver{Home: filepath.Join("C:", "Users", "u"), GOOS: "windows", Getenv: func(k string) string {
		if k == "LocalAppData" {
			return filepath.Join("C:", "Users", "u", "AppData", "Local")
		}
		return ""
	}}
	want := filepath.Join("C:", "Users", "u", "AppData", "Local", "kameas", "kenaz", "state", "prod")
	if got := r.StateDir("prod"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
