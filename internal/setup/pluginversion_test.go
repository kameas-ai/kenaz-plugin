package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

func writeBundleState(t *testing.T, dir, claude, codex string) {
	t.Helper()
	reg := func(v string) string {
		if v == "" {
			return "null"
		}
		return `{"path":"x","written_sha256":"","created_container":false,"keys":{},"bundle":{"root":"r","version":"` + v + `","rendered":"r","digest":"d","launcher":"l"}}`
	}
	st := `{"version":1,"variant":"` + variant.Current.Name + `","claude":` + reg(claude) + `,"codex":` + reg(codex) + `}`
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
}

// After an upgrade the apps run the new kenaz with the plugin files copied
// at the last init: StalePluginFiles names the apps and both versions in one
// line, and says nothing when the files match or setup registered none.
func TestStalePluginFiles(t *testing.T) {
	bin := variant.Current.Binary
	for _, tc := range []struct {
		name, claude, codex, version, want string
	}{
		{"no setup", "", "", "0.8.2", ""},
		{"current", "0.8.2", "0.8.2", "0.8.2", ""},
		{"v prefix is the same version", "v0.8.2", "", "0.8.2", ""},
		{"one app", "0.8.2", "0.8.0", "0.8.2", "the plugin files registered in Codex are from " + bin + " 0.8.0 and this " + bin + " is 0.8.2; run `" + bin + " init` to refresh them"},
		{"both apps", "0.8.0", "0.8.0", "0.8.2", "the plugin files registered in Claude Code and Codex are from " + bin + " 0.8.0 and this " + bin + " is 0.8.2; run `" + bin + " init` to refresh them"},
		{"two versions", "0.7.0", "0.8.0", "0.8.2", "the plugin files registered in Claude Code are from " + bin + " 0.7.0, those in Codex from " + bin + " 0.8.0, and this " + bin + " is 0.8.2; run `" + bin + " init` to refresh them"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.claude != "" || tc.codex != "" {
				writeBundleState(t, dir, tc.claude, tc.codex)
			}
			got := StalePluginFiles(dir, tc.version)
			if got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
			if strings.Contains(got, "\n") {
				t.Fatalf("more than one line: %q", got)
			}
		})
	}
}

// The advice read from one environment's setup state names that
// environment, unless it is the build's default (a plain init acts on
// that one) or one the build cannot use; a directory that is no
// environment's gets the plain command.
func TestRegistrationAdviceNamesTheEnvironment(t *testing.T) {
	flag := map[string]map[string]bool{ // build, then the state directory's name
		"kenaz":     {"prod": false, "stage": false, "dev": true, "elsewhere": false},
		"kenaz-dev": {"prod": false, "stage": false, "dev": false, "elsewhere": false},
	}[variant.Current.Name]
	if flag == nil {
		t.Fatalf("no expectations for build %s", variant.Current.Name)
	}
	for env, withFlag := range flag {
		t.Run(env, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), env)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			want := "run `" + variant.Current.Binary + " init` to"
			if withFlag {
				want = "run `" + variant.Current.Binary + " init --env " + env + "` to"
			}
			check := func(what, got string) {
				t.Helper()
				if !strings.Contains(strings.ReplaceAll(got, "Run `", "run `"), want) {
					t.Errorf("%s: %q does not have %q", what, got, want)
				}
			}
			// Plugin files of another version; the launcher "l" and the
			// bundle root "r" do not exist.
			writeBundleState(t, dir, "0.8.0", "")
			check("StalePluginFiles", StalePluginFiles(dir, "0.8.2"))
			broken := BrokenRegistrations(dir)
			if len(broken) != 1 {
				t.Fatalf("BrokenRegistrations: %q", broken)
			}
			check("BrokenRegistrations (launcher gone)", broken[0])
			o := Options{StateDir: dir}
			_, problem := o.registeredLauncher(&BundleReg{Root: "r", Launcher: "l"})
			check("doctor (launcher gone)", problem)
			_, problem = o.registeredLauncher(&BundleReg{Root: "r"})
			check("doctor (bundle gone)", problem)

			// The launcher exists, the files are outside the state directory.
			launcher := filepath.Join(t.TempDir(), "launcher")
			os.WriteFile(launcher, nil, 0o755)
			s := &State{Version: 1, Claude: &FileState{Path: "x", Keys: map[string]KeyState{},
				Bundle: &BundleReg{Root: "r", Rendered: filepath.Join(t.TempDir(), "x"), Launcher: launcher}}}
			if err := saveState(dir, s); err != nil {
				t.Fatal(err)
			}
			if broken = BrokenRegistrations(dir); len(broken) != 1 {
				t.Fatalf("BrokenRegistrations after a move: %q", broken)
			}
			check("BrokenRegistrations (moved)", broken[0])
		})
	}
}
