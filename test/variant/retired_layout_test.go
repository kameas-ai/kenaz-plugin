package variant_test

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// exact is everything under dir: every directory, link (with its target)
// and file (with its mode and a hash of its bytes), so that two equal
// results mean nothing was created, moved, linked, rewritten or deleted.
func exact(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			fmt.Fprintf(&b, "%s -> %s\n", rel, target)
		case info.IsDir():
			fmt.Fprintf(&b, "%s/ %v\n", rel, info.Mode())
		default:
			c, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s %v %x\n", rel, info.Mode(), sha256.Sum256(c))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// oldLayout seeds what a release up to 0.6.x left: config.toml and a state
// root with one folder per environment (stage included), in that release's
// own locations. With kenazHome the locations are the ones under
// $KENAZ_HOME. It returns the old state root, the root a migration would
// move it to, and the variables that select the layout.
func (m *machine) oldLayout(kenazHome bool, config string) (old, root string, vars []string) {
	m.t.Helper()
	cfg := filepath.Dir(m.legacyState())
	old, root = m.legacyState(), m.layout().StateRoot
	if runtime.GOOS != "darwin" {
		cfg = filepath.Join(m.home, ".config", "kameas", "kenaz")
	}
	if kenazHome {
		kh := filepath.Join(m.home, "kenaz-home")
		cfg, old, root = filepath.Join(kh, "config"), filepath.Join(kh, "state"), filepath.Join(kh, "plugin")
		vars = []string{variant.Shared.HomeVar + "=" + kh}
	}
	files := map[string]string{
		filepath.Join(cfg, "config.toml"):           config,
		filepath.Join(old, "prod", "consent.json"):  "{}",
		filepath.Join(old, "stage", "consent.json"): "{}",
		filepath.Join(old, "dev", "consent.json"):   "{}",
		filepath.Join(old, "memory", "note.txt"):    "kept",
	}
	for p, c := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			m.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			m.t.Fatal(err)
		}
	}
	// The file credential store, so that nothing touches an OS keychain.
	if err := credstore.WriteMarker(filepath.Join(old, "credentials"), credstore.Files); err != nil {
		m.t.Fatal(err)
	}
	return old, root, vars
}

// A machine that still has an earlier release's layout: a command refused
// for the retired stage environment is refused before anything moves.
// `init` otherwise moves the whole old state root to ~/.kenaz/plugin and
// leaves a link; refused, it leaves the home directory exactly as it was.
// The commands that still take stage (the undo of a host's setup) use the
// old files where they are, and move nothing either.
func TestRefusedStageMovesNothingOnAnOldLayout(t *testing.T) {
	id := variant.Current
	want := retiredStage[id.Name]
	for _, kenazHome := range []bool{false, true} {
		t.Run(fmt.Sprintf("KENAZ_HOME=%v", kenazHome), func(t *testing.T) {
			m := newMachine(t)
			old, root, vars := m.oldLayout(kenazHome, "memory = \"on\"\n")
			bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
			before := exact(t, m.home)
			unchanged := func(what string) {
				t.Helper()
				if after := exact(t, m.home); after != before {
					t.Fatalf("%s changed the home directory:\n--- before\n%s--- after\n%s", what, before, after)
				}
			}
			for _, c := range []struct {
				env  []string
				args []string
				want string
			}{
				{nil, []string{"init", "--env", "stage", "--yes"}, want},
				{nil, []string{"init", "--env=stage"}, want},
				{nil, []string{"init", "--yes", "--no-path", "-env", "STAGE"}, want},
				{[]string{variant.Shared.EnvVar + "=stage"}, []string{"init", "--yes"}, want + " (stage was selected by env)"},
				{[]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV=stage"}, []string{"init"}, want + " (stage was selected by env)"},
				{nil, []string{"status", "--env", "stage"}, want},
				{nil, []string{"login", "--env", "stage"}, want},
				{nil, []string{"setup", "claude", "--env", "stage", "--yes"}, want},
				{nil, []string{"doctor", "--env", "stage"}, want},
			} {
				r := m.run(bin, append(c.env, vars...), c.args...)
				if r.Code != 64 || r.Stderr != c.want+"\n" || r.Stdout != "" {
					t.Errorf("%v %v: exit %d\nstdout %q\nstderr %q\nwant   %q", c.env, c.args, r.Code, r.Stdout, r.Stderr, c.want+"\n")
				}
				unchanged(strings.Join(c.args, " "))
			}

			// The undo for stage is not refused. It reads the old files where
			// they are, finds nothing recorded, and moves nothing.
			r := m.run(bin, vars, "setup", "--undo", "claude", "--env", "stage", "--yes")
			if r.Code != 0 || !strings.Contains(r.Stdout, "Nothing to undo") || strings.Contains(r.Stderr, "retired") {
				t.Fatalf("undo: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
			}
			unchanged("setup --undo claude --env stage")
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Fatalf("%s was created: %v", root, err)
			}
			if fi, err := os.Lstat(old); err != nil || !fi.IsDir() {
				t.Fatalf("the old state root is no longer a folder: %v", err)
			}
		})
	}
}

// config.toml selecting stage refuses init the same way, where the file is
// the earlier release's own.
func TestStageInAnOldConfigFileRefusesInitBeforeItMoves(t *testing.T) {
	id := variant.Current
	m := newMachine(t)
	_, _, vars := m.oldLayout(false, "env = \"stage\"\n")
	bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
	before := exact(t, m.home)
	want := retiredStage[id.Name] + " (stage was selected by config)\n"
	if r := m.run(bin, vars, "init", "--yes"); r.Code != 64 || r.Stderr != want || r.Stdout != "" {
		t.Fatalf("exit %d\nstdout %q\nstderr %q\nwant   %q", r.Code, r.Stdout, r.Stderr, want)
	}
	if after := exact(t, m.home); after != before {
		t.Fatalf("a refused init changed the home directory:\n--- before\n%s--- after\n%s", before, after)
	}
}

// Undoing a host's setup for stage where nothing was ever set up for it
// leaves no trace: no stage folder, and nothing else that was not there.
func TestUndoOfNothingForStageLeavesNoTrace(t *testing.T) {
	id := variant.Current
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty home=%v", fresh), func(t *testing.T) {
			m := newMachine(t)
			l := m.layout()
			if !fresh {
				// A device in use for the build's own environment.
				m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
			}
			bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
			before := exact(t, m.home)
			for _, args := range [][]string{
				{"setup", "--undo", "claude", "--env", "stage", "--yes"},
				{"setup", "--undo", "codex", "--env", "stage", "--dry-run"},
				{"setup", "--undo", "claude", "--yes"},
			} {
				var env []string
				if len(args) == 4 {
					env = []string{variant.Shared.EnvVar + "=stage"}
				}
				r := m.run(bin, env, args...)
				if r.Code != 0 || !strings.Contains(r.Stdout, "Nothing to undo") || strings.Contains(r.Stderr, "retired") {
					t.Fatalf("%v: exit %d\n%s%s", args, r.Code, r.Stdout, r.Stderr)
				}
				if _, err := os.Lstat(l.state("stage")); !os.IsNotExist(err) {
					t.Errorf("%v created a stage folder: %v", args, err)
				}
				if after := exact(t, m.home); after != before {
					t.Errorf("%v changed the home directory:\n--- before\n%s--- after\n%s", args, before, after)
				}
			}
		})
	}
}
