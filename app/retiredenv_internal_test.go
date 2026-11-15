package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// `init` moves an earlier release's files into the root before it runs.
// The env gate runs first and must not: a refused init leaves the old
// state root a folder where it was, with no link and nothing new beside
// it. (The gate is called on its own here, so that init itself never runs
// in this process.)
func TestRefusedInitMovesNothing(t *testing.T) {
	tree := func(dir string) string {
		var b strings.Builder
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			info, _ := os.Lstat(p)
			c, _ := os.ReadFile(p)
			b.WriteString(rel + " " + info.Mode().String() + " " + string(c) + "\n")
			return nil
		})
		return b.String()
	}
	for _, c := range []struct {
		name   string
		vars   map[string]string
		config string
		args   []string
	}{
		{"flag", nil, "", []string{"--env", "stage", "--yes"}},
		{"env var", map[string]string{variant.Shared.EnvVar: "stage"}, "", []string{"--yes"}},
		{"old config file", nil, "env = \"stage\"\n", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			for p, text := range map[string]string{
				filepath.Join(home, "state", "stage", "consent.json"): "{}",
				filepath.Join(home, "state", "dev", "consent.json"):   "{}",
				filepath.Join(home, "config", "config.toml"):          c.config,
			} {
				os.MkdirAll(filepath.Dir(p), 0o700)
				if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := tree(home)
			var errb bytes.Buffer
			env := Env{Provider: stub.New(), Stdout: &bytes.Buffer{}, Stderr: &errb, layout: &layoutState{cmd: "init"},
				Getenv: func(k string) string {
					if k == variant.Shared.HomeVar {
						return home
					}
					return c.vars[k]
				}}
			code, refused := env.refuseEnv("init", c.args)
			if !refused || code != ExitUsage || !strings.HasPrefix(errb.String(), variant.Current.RetiredMessage("stage")) {
				t.Fatalf("refused %v, exit %d, stderr %q", refused, code, errb.String())
			}
			if after := tree(home); after != before {
				t.Fatalf("the refused init changed files:\n%s\nwere:\n%s", after, before)
			}
			// The same layout, not refused: init does move them (so the check
			// above can fail).
			env.layout = &layoutState{cmd: "init"}
			env.Stderr = &bytes.Buffer{}
			if _, err := env.paths(); err != nil {
				t.Fatal(err)
			}
			if after := tree(home); after == before {
				t.Fatal("init's own layout step moved nothing: the test proves nothing")
			}
		})
	}
}

// Only the commands that remove what was set up may name a retired
// environment: logout and setup --undo.
func TestRetiredEnvCleanup(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"logout"}, true},
		{[]string{"logout", "--env", "stage"}, true},
		{[]string{"setup", "--undo", "claude", "--env", "stage"}, true},
		{[]string{"setup", "-undo", "codex"}, true},
		{[]string{"setup", "claude", "--env", "stage", "--undo"}, true},
		{[]string{"setup", "--undo=true", "claude"}, true},
		{[]string{"setup", "--undo", "claude", "--dry-run"}, true},
		{[]string{"setup", "--undo=false", "claude"}, false},
		{[]string{"setup", "--undo=x", "claude"}, false},
		{[]string{"setup", "claude"}, false},
		{[]string{"setup", "undo"}, false},
		// The last value wins, as it does for setup itself: these apply.
		{[]string{"setup", "--undo", "--undo=false", "claude", "--env", "stage"}, false},
		{[]string{"setup", "--undo", "claude", "--env", "stage", "--undo=false"}, false},
		// A flag's value is not a flag: --bundle takes "--undo" as its directory.
		{[]string{"setup", "claude", "--env", "stage", "--bundle", "--undo"}, false},
		{[]string{"setup", "claude", "--env", "--undo"}, false},
		// Only the undo of a host: not the doctor, print-managed or a bad command.
		{[]string{"setup", "--undo", "doctor", "--env", "stage"}, false},
		{[]string{"setup", "--undo", "print-managed", "claude"}, false},
		{[]string{"setup", "--undo"}, false},
		{[]string{"setup", "--undo", "claude", "codex"}, false},
		{[]string{"setup", "--undo", "claude", "--no-such-flag"}, false},
		{[]string{"login", "--undo"}, false},
		{[]string{"status"}, false},
		{[]string{"init"}, false},
		{[]string{"mcp"}, false},
		{[]string{"daemon"}, false},
	} {
		if got := retiredEnvCleanup(c.args[0], c.args[1:]); got != c.want {
			t.Errorf("retiredEnvCleanup(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

// A stage session reads as signed out in every build, except for a
// cleanup command, which must see it to clear it.
func TestStageSessionIsSeenOnlyByCleanup(t *testing.T) {
	inner := sessionAuth{stub.New().Auth(), fleetapi.EnvStage}
	if _, err := (envAuth{inner, false}).Session(context.Background()); !errors.Is(err, fleetapi.ErrNotSignedIn) {
		t.Errorf("stage session outside cleanup: %v", err)
	}
	if s, err := (envAuth{inner, true}).Session(context.Background()); err != nil || s.Env != fleetapi.EnvStage {
		t.Errorf("stage session in cleanup: %+v %v", s, err)
	}
	// Cleanup widens nothing else: an environment this build never allowed
	// stays signed out.
	for _, env := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvDev} {
		_, plain := (envAuth{sessionAuth{stub.New().Auth(), env}, false}).Session(context.Background())
		_, cleanup := (envAuth{sessionAuth{stub.New().Auth(), env}, true}).Session(context.Background())
		if (plain == nil) != (cleanup == nil) {
			t.Errorf("%s: plain %v, cleanup %v", env, plain, cleanup)
		}
	}
}

// The gate reads --env the way the command will. Setup and init parse
// their own flags; the forms below are the ones a plain scan of the
// arguments gets wrong.
func TestCommandEnvFlag(t *testing.T) {
	for _, c := range []struct {
		args  []string
		want  string
		exact bool
	}{
		{[]string{"setup", "claude", "--env", "stage"}, "stage", true},
		{[]string{"setup", "--", "claude", "--env", "stage", "--yes"}, "stage", true},
		{[]string{"setup", "claude", "--yes", "--env", "stage", "--bundle", "--env=dev"}, "stage", true},
		{[]string{"setup", "claude", "--env", "stage", "--env", "dev"}, "dev", true},
		{[]string{"setup", "claude", "--bundle", "--env=stage"}, "", true},
		{[]string{"setup", "claude", "--no-such-flag", "--env", "stage"}, "stage", false},
		{[]string{"init", "--env", "stage", "--bundle", "--env=dev"}, "stage", true},
		{[]string{"init", "--yes", "-env=stage", "-bundle", "-env=dev"}, "stage", true},
		{[]string{"init", "--bundle", "--env=stage"}, "", true},
		{[]string{"init", "--env", "stage", "--env=dev"}, "dev", true},
		{[]string{"init", "extra", "--env", "stage"}, "stage", false},
		// mcp reads its own flags too: what follows `--` is not a flag.
		{[]string{"mcp", "--env", "stage"}, "stage", true},
		{[]string{"mcp", "--env", "prod", "--", "--env", "stage"}, "prod", true},
		{[]string{"mcp", "--", "--env", "stage"}, "", true},
		{[]string{"mcp", "--no-such-flag", "--env", "stage"}, "stage", false},
		{[]string{"status", "--env", "stage"}, "stage", false},
		{[]string{"status", "--", "--env", "stage"}, "", false},
	} {
		got, exact := commandEnvFlag(c.args[0], c.args[1:])
		if got != c.want || exact != c.exact {
			t.Errorf("commandEnvFlag(%v) = %q, %v; want %q, %v", c.args, got, exact, c.want, c.exact)
		}
	}
	if got := envFlagValues([]string{"--env", "a", "x", "-env=b", "--", "--env", "c", "--envy=d", "--env"}); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("envFlagValues = %q", got)
	}
}

// Each form is refused by the gate itself: exit 64 and the one message,
// with nothing after it (the command never starts).
func TestGateRefusesStageHoweverItIsSpelled(t *testing.T) {
	want := variant.Current.RetiredMessage("stage") + "\n"
	for _, args := range [][]string{
		{"setup", "--", "claude", "--env", "stage", "--yes"},
		{"setup", "claude", "--yes", "--env", "stage", "--bundle", "--env=dev"},
		{"init", "--yes", "--env", "stage", "--bundle", "--env=dev"},
		{"init", "--env", "stage", "--bundle", "--env=dev"},
		{"doctor", "--", "--env", "stage"},
		{"skills", "--", "list", "--env=stage"},
	} {
		home := t.TempDir()
		var errb bytes.Buffer
		env := Env{Provider: stub.New(), Stdout: &bytes.Buffer{}, Stderr: &errb, layout: &layoutState{cmd: args[0]},
			Getenv: func(k string) string {
				if k == variant.Shared.HomeVar {
					return home
				}
				return ""
			}}
		code, refused := env.refuseEnv(args[0], args[1:])
		if !refused || code != ExitUsage || errb.String() != want {
			t.Errorf("%v: refused %v, exit %d, stderr %q", args, refused, code, errb.String())
		}
		if ents, _ := os.ReadDir(home); len(ents) != 0 {
			t.Errorf("%v: created %v", args, ents)
		}
	}
	// The cleanup commands still take it.
	home := t.TempDir()
	env := Env{Provider: stub.New(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, cleanup: true,
		Getenv: func(k string) string {
			if k == variant.Shared.HomeVar {
				return home
			}
			return ""
		}}
	for _, args := range [][]string{{"logout", "--env", "stage"}, {"setup", "--undo", "--", "claude", "--env", "stage"}} {
		if _, refused := env.refuseEnv(args[0], args[1:]); refused {
			t.Errorf("%v was refused", args)
		}
	}
}
