package app_test

import (
	"bytes"
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
)

// fleetCounter counts every use of a part of the provider that talks to
// Kameas Fleet. Auth (local token state) and Flavor are not counted.
type fleetCounter struct {
	fleetapi.Provider
	n atomic.Int32
}

func (c *fleetCounter) Enroller() fleetapi.Enroller { c.n.Add(1); return c.Provider.Enroller() }
func (c *fleetCounter) Me() fleetapi.Me             { c.n.Add(1); return c.Provider.Me() }
func (c *fleetCounter) Pollers() fleetapi.Pollers   { c.n.Add(1); return c.Provider.Pollers() }
func (c *fleetCounter) OptIns() fleetapi.OptIns     { c.n.Add(1); return c.Provider.OptIns() }
func (c *fleetCounter) Catalog() fleetapi.Catalog   { c.n.Add(1); return c.Provider.Catalog() }
func (c *fleetCounter) Context() fleetapi.Context   { c.n.Add(1); return c.Provider.Context() }
func (c *fleetCounter) Team() fleetapi.Team         { c.n.Add(1); return c.Provider.Team() }
func (c *fleetCounter) OTLP() fleetapi.OTLPPoster   { c.n.Add(1); return c.Provider.OTLP() }

// withStdin feeds payload to the hook as its stdin, the way the harness does.
func withStdin(t *testing.T, payload string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
	go func() { _, _ = w.WriteString(payload); w.Close() }()
}

// TestHooksSilentWhenSignedOut covers a fresh session on a device where
// nobody has signed in and no relay is running, as in a new Cowork session
// on the user's computer. The plugin's SessionStart and SessionEnd hooks
// must exit 0, print nothing on stdout or stderr, finish inside their
// hooks.json timeouts (5 s and 1 s), and not reach Kameas Fleet at all,
// whether or not the relay can be started.
func TestHooksSilentWhenSignedOut(t *testing.T) {
	// Claude Code and Cowork set CLAUDE_PLUGIN_ROOT for plugin hooks.
	t.Setenv("CLAUDE_PLUGIN_ROOT", t.TempDir())
	t.Setenv("PLUGIN_ROOT", "")

	for _, tc := range []struct {
		name  string
		p     fleetapi.Provider
		spawn func(string, fleetapi.Env) error
	}{
		{"official, relay starts", &official{Provider: stub.New()}, func(string, fleetapi.Env) error { return nil }},
		{"official, relay cannot start", &official{Provider: stub.New()}, func(string, fleetapi.Env) error { return os.ErrPermission }},
		{"public build", stub.New(), func(string, fleetapi.Env) error { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fleetCounter{Provider: tc.p}
			h := newHarness(t, fc)
			for _, c := range []struct {
				event, payload string
				budget         time.Duration
			}{
				{"session-start", `{"session_id":"s-1","cwd":"` + t.TempDir() + `","hook_event_name":"SessionStart","source":"startup"}`, 5 * time.Second},
				{"session-end", `{"session_id":"s-1","reason":"other","hook_event_name":"SessionEnd"}`, time.Second},
			} {
				withStdin(t, c.payload)
				var out, errb bytes.Buffer
				e := h.env(context.Background(), &out, &errb)
				e.Spawn = tc.spawn
				start := time.Now()
				code := app.RunEnv(e, []string{"hook", c.event})
				if took := time.Since(start); took >= c.budget {
					t.Errorf("%s took %v, over its %v hook timeout", c.event, took, c.budget)
				}
				if code != 0 || out.Len() != 0 || errb.Len() != 0 {
					t.Errorf("%s: exit=%d stdout=%q stderr=%q, want 0 and no output", c.event, code, out.String(), errb.String())
				}
			}
			if n := fc.n.Load(); n != 0 {
				t.Errorf("signed-out hooks used the Fleet connection %d times, want 0", n)
			}
		})
	}
}
