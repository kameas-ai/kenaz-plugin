package setup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// A host has one plugin marketplace slot and one set of OTEL settings, and
// setup state is per environment. Setting a host up for a second
// environment is refused, and the refusal and the doctor name the
// environment that holds it and the undo that frees it, instead of calling
// the registration one setup did not make.
func TestSetUpForAnotherEnvironment(t *testing.T) {
	for _, h := range []harness{harnessClaude, harnessCodex} {
		t.Run(h.label(), func(t *testing.T) {
			e, f, _ := bundleEnv(t, false)
			root := filepath.Join(filepath.Dir(e.opts.StateDir), "plugin")
			dev, stage := filepath.Join(root, "dev"), filepath.Join(root, "stage")
			undo := "`" + variant.Current.Binary + " setup --undo " + hostCLI(h) + " --env dev`"

			e.opts.StateDir = dev
			e.setPort(t, 4318)
			if err := Apply(e.opts, h); err != nil {
				t.Fatal(err)
			}
			before, _ := e.read(t, h)
			f.calls = nil

			e.opts.StateDir = stage
			e.setPort(t, 47318)
			err := Apply(e.opts, h)
			if err == nil || len(f.calls) != 0 {
				t.Fatalf("got %v, calls %v", err, f.calls)
			}
			for _, want := range []string{"set up for the dev environment", "to use stage instead", undo} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal lacks %q: %v", want, err)
				}
			}
			for _, not := range []string{"did not register", "marketplace remove"} {
				if strings.Contains(err.Error(), not) {
					t.Errorf("refusal says %q: %v", not, err)
				}
			}
			if after, _ := e.read(t, h); string(after) != string(before) {
				t.Fatal("file modified")
			}

			// The doctor (init's summary) explains the port the same way.
			run := func(_ context.Context, name string, args ...string) (string, error) {
				if len(args) == 1 && args[0] == "--version" {
					return "999.0.0", nil
				}
				return "", errors.New("not found")
			}
			var port []Check
			for _, c := range Doctor(context.Background(), e.opts, run) {
				if c.Name == h.label() && strings.Contains(c.Detail, ":4318") {
					port = append(port, c)
				}
			}
			if len(port) == 0 {
				t.Fatal("no endpoint check")
			}
			for _, c := range port {
				if c.Status != "warn" || !strings.Contains(c.Detail, "the relay of the dev environment") || !strings.Contains(c.Detail, "port 47318") || !strings.Contains(c.Detail, undo) || strings.Contains(c.Detail, "re-run setup") {
					t.Errorf("endpoint check: %+v", c)
				}
			}

			// The undo it names frees the host for this environment.
			e.opts.StateDir = dev
			if err := Undo(e.opts, h); err != nil {
				t.Fatal(err)
			}
			e.opts.StateDir = stage
			if err := Apply(e.opts, h); err != nil {
				t.Fatal(err)
			}
			if s, ok := e.opts.otherEnvRegistered(h, e.opts.renderDir(mustBundle(t, e.opts.BundleRoot))); ok {
				t.Fatalf("still attributed to %s", s.Env)
			}
		})
	}
}

func mustBundle(t *testing.T, root string) *Bundle {
	t.Helper()
	b, err := LoadBundle(root)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A relay that only changed its port, with no other environment involved,
// keeps the plain advice.
func TestPortMismatchWithoutAnotherEnvironment(t *testing.T) {
	e := newEnv(t, 4318)
	e.opts.StateDir = filepath.Join(filepath.Dir(e.opts.StateDir), "plugin", "dev")
	e.setPort(t, 4318)
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	e.setPort(t, 4319)
	var got []string
	add := func(status, format string, a ...any) { got = append(got, status+" "+format) }
	e.opts.checkUserEndpoint(harnessClaude, add, "http://127.0.0.1:4318", 4319)
	if len(got) != 1 || !strings.Contains(got[0], "re-run setup") {
		t.Fatalf("got %v", got)
	}
}
