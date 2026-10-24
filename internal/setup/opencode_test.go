package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openCodeEnv extends newEnv with a fake adapter package and fixed binary
// path. No relay port is needed: the adapter discovers it at run time.
func openCodeEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t, 4318)
	root := filepath.Dir(e.opts.StateDir)
	e.opts.OpenCodeDir = filepath.Join(root, "home", ".config", "opencode")
	e.opts.KenazBin = filepath.Join(root, "opt", "kenaz", "bin", "kenaz")
	e.opts.AdapterDir = filepath.Join(root, "opt", "kenaz", "opencode")
	os.MkdirAll(filepath.Join(e.opts.AdapterDir, "src"), 0o700)
	os.WriteFile(filepath.Join(e.opts.AdapterDir, "package.json"), []byte(`{"name":"@kameas/kenaz-opencode","main":"./src/index.js"}`), 0o600)
	os.WriteFile(filepath.Join(e.opts.AdapterDir, "src", "index.js"), []byte("export default {}\n"), 0o600)
	os.MkdirAll(filepath.Dir(e.opts.KenazBin), 0o700)
	os.WriteFile(e.opts.KenazBin, []byte("#!/bin/sh\n"), 0o700)
	e.opts.OpenCodeVersion = func() (string, bool) { return "1.19.4\n", true }
	return e, root
}

// normalized replaces the temp root so goldens are stable.
func normalized(b []byte, root string) []byte {
	return bytes.ReplaceAll(b, []byte(root), []byte("/ROOT"))
}

func TestOpenCodeGolden(t *testing.T) {
	cases := []struct{ fixture, golden, env string }{
		{"", "opencode/absent", ""},
		{"opencode/existing.json", "opencode/existing", ""},
		{"opencode/manual-kenaz.json", "opencode/manual-kenaz", "stage"},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			e, root := openCodeEnv(t)
			e.opts.FleetEnv = c.env
			in := e.seed(t, harnessOpenCode, c.fixture)
			if err := Apply(e.opts, harnessOpenCode); err != nil {
				t.Fatal(err)
			}
			got, _ := e.read(t, harnessOpenCode)
			golden(t, c.golden, normalized(got, root))

			// Everything that was there is still there, unchanged.
			if c.fixture != "" {
				var before, after map[string]json.RawMessage
				json.Unmarshal(in, &before)
				json.Unmarshal(got, &after)
				for k, v := range before {
					if k == "mcp" || k == "plugin" {
						continue
					}
					if !sameJSON(v, after[k]) {
						t.Errorf("%s changed", k)
					}
				}
			}

			if err := Undo(e.opts, harnessOpenCode); err != nil {
				t.Fatal(err)
			}
			after, exists := e.read(t, harnessOpenCode)
			if c.fixture == "" {
				if exists {
					t.Fatalf("undo left a file setup created:\n%s", after)
				}
			} else if !bytes.Equal(after, in) {
				t.Fatalf("undo is not byte-identical:\n%s", unifiedDiff("in", "after", string(in), string(after)))
			}
		})
	}
}

func TestOpenCodeIdempotentAndNoPortNeeded(t *testing.T) {
	e, _ := openCodeEnv(t)
	os.Remove(filepath.Join(e.opts.StateDir, "daemon.json"))
	e.seed(t, harnessOpenCode, "opencode/existing.json")
	if err := Apply(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}
	first, _ := e.read(t, harnessOpenCode)
	e.out.Reset()
	if err := Apply(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}
	second, _ := e.read(t, harnessOpenCode)
	backups, _ := filepath.Glob(e.opts.path(harnessOpenCode) + backupInfix + "*")
	if !bytes.Equal(first, second) || len(backups) != 1 || !strings.Contains(e.out.String(), "already configured") {
		t.Fatalf("not idempotent (backups %d): %s", len(backups), e.out)
	}
}

// Moving the binary or the relay state re-renders the owned entries in
// place; undo still restores the original file.
func TestOpenCodeReapplyAfterMove(t *testing.T) {
	e, root := openCodeEnv(t)
	in := e.seed(t, harnessOpenCode, "opencode/existing.json")
	if err := Apply(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}
	e.opts.KenazBin = filepath.Join(root, "opt", "kenaz", "bin", "kenaz-new")
	if err := Apply(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}
	got, _ := e.read(t, harnessOpenCode)
	if !bytes.Contains(got, []byte("kenaz-new")) || bytes.Count(got, []byte(`"kenaz"`)) != 1 {
		t.Fatalf("re-render failed:\n%s", got)
	}
	var cfg struct{ Plugin []json.RawMessage }
	json.Unmarshal(got, &cfg)
	if len(cfg.Plugin) != 3 {
		t.Fatalf("plugin entries: %d", len(cfg.Plugin))
	}
	if err := Undo(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}
	after, _ := e.read(t, harnessOpenCode)
	if !bytes.Equal(after, in) {
		t.Fatalf("undo not byte-identical:\n%s", unifiedDiff("in", "after", string(in), string(after)))
	}
}

func TestOpenCodeRefusals(t *testing.T) {
	cases := []struct {
		name    string
		content string
		mutate  func(*Options)
		want    string
	}{
		{name: "comments", content: "{\n  // mine\n  \"theme\": \"x\"\n}\n", want: "not plain JSON"},
		{name: "mcp not object", content: `{"mcp": []}`, want: `"mcp" in opencode.json is not an object`},
		{name: "plugin not array", content: `{"plugin": "x"}`, want: `"plugin" in opencode.json is not an array`},
		{name: "foreign kenaz name", content: `{"mcp": {"kenaz": {"type": "remote", "url": "https://x.example"}}}`, want: "does not run kenaz"},
		{name: "duplicate server", content: `{"mcp": {"fleet": {"type": "local", "command": ["/usr/bin/kenaz", "mcp"]}}}`, want: "registered twice"},
		{name: "npm adapter", content: `{"plugin": ["@kameas/kenaz-opencode@0.3.0"]}`, want: "from npm"},
		{name: "old opencode", content: `{}`, mutate: func(o *Options) { o.OpenCodeVersion = func() (string, bool) { return "1.18.9", true } }, want: "older than the oldest supported"},
		{name: "no adapter", content: `{}`, mutate: func(o *Options) { o.AdapterDir = "" }, want: "--adapter"},
		{name: "wrong adapter", content: `{}`, mutate: func(o *Options) { o.AdapterDir = o.StateDir }, want: "no such file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := openCodeEnv(t)
			if c.mutate != nil {
				c.mutate(&e.opts)
			}
			in := []byte(c.content)
			os.MkdirAll(e.opts.OpenCodeDir, 0o700)
			os.WriteFile(e.opts.path(harnessOpenCode), in, 0o600)
			err := Apply(e.opts, harnessOpenCode)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error containing %q", err, c.want)
			}
			got, _ := e.read(t, harnessOpenCode)
			if !bytes.Equal(got, in) {
				t.Fatal("file was modified")
			}
			if _, err := os.Stat(filepath.Join(e.opts.StateDir, stateFile)); err == nil {
				t.Fatal("state written on refusal")
			}
		})
	}
}

func TestOpenCodeUndoKeepsUserChanges(t *testing.T) {
	e, _ := openCodeEnv(t)
	e.seed(t, harnessOpenCode, "opencode/existing.json")
	if err := Apply(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}
	// The user edits our MCP entry and adds a server of their own.
	b, _ := e.read(t, harnessOpenCode)
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	mcp := cfg["mcp"].(map[string]any)
	mcp["kenaz"].(map[string]any)["timeout"] = 9000
	mcp["extra"] = map[string]any{"type": "remote", "url": "https://extra.example"}
	b, _ = json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(e.opts.path(harnessOpenCode), b, 0o600)

	if err := Undo(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.err.String(), "mcp.kenaz changed since setup") {
		t.Fatalf("no warning: %s", e.err)
	}
	after, _ := e.read(t, harnessOpenCode)
	var got struct {
		MCP    map[string]json.RawMessage `json:"mcp"`
		Plugin []json.RawMessage          `json:"plugin"`
	}
	json.Unmarshal(after, &got)
	if _, ok := got.MCP["kenaz"]; !ok {
		t.Fatal("user-edited entry removed")
	}
	if _, ok := got.MCP["extra"]; !ok {
		t.Fatal("user server removed")
	}
	if len(got.Plugin) != 2 {
		t.Fatalf("adapter entry not removed: %s", after)
	}
}

func TestOpenCodeDoctor(t *testing.T) {
	e, _ := openCodeEnv(t)
	e.seed(t, harnessOpenCode, "opencode/existing.json")
	if err := Apply(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "opencode" && len(args) == 1 && args[0] == "--version" {
			return "1.19.4\n", nil
		}
		return "", errors.New("not found")
	}
	cs := doctorOpenCode(context.Background(), e.opts, run)
	want := map[string]string{"MCP server kenaz runs": "ok", "the Kenaz adapter is loaded from": "ok"}
	for sub, status := range want {
		found := false
		for _, c := range cs {
			if c.Status == status && strings.Contains(c.Detail, sub) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s %q in %+v", status, sub, cs)
		}
	}

	e.opts.StateDir = filepath.Join(e.opts.StateDir, "other-env")
	cs = doctorOpenCode(context.Background(), e.opts, run)
	found := false
	for _, c := range cs {
		if c.Status == "warn" && strings.Contains(c.Detail, "reports to") {
			found = true
		}
	}
	if !found {
		t.Errorf("state dir mismatch not reported: %+v", cs)
	}
}
