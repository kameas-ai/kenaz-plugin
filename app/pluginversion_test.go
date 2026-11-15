package app_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

func writeSetupState(t *testing.T, dir, bundle string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := `{"version":1,"variant":"` + variant.Current.Name + `","codex":{"path":"x","written_sha256":"","created_container":false,"keys":{},"bundle":` + bundle + `}}`
	if err := os.WriteFile(filepath.Join(dir, "setup-state.json"), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
}

// After `brew upgrade` the apps start the new kenaz with the plugin files
// copied at the last init (Codex's list of tools that ask before writing
// among them). `kenaz mcp` says so in one stderr line, never on stdout (the
// MCP protocol), and says nothing when the files are this version's.
func TestMCPReportsStalePluginFilesOnce(t *testing.T) {
	defer func(v string) { app.Version = v }(app.Version)
	app.Version = "0.8.2"
	h := newHarness(t, stub.New())
	exe, err := os.Executable() // a launcher that exists
	if err != nil {
		t.Fatal(err)
	}
	mcp := func() (string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		env := h.env(context.Background(), &out, &errb)
		env.ServeMCP = func(context.Context, *sdk.Server) error { return nil }
		if code := app.RunEnv(env, []string{"mcp"}); code != 0 {
			t.Fatalf("mcp exit=%d err=%s", code, errb.String())
		}
		return out.String(), errb.String()
	}
	bundle := func(v string) string {
		return `{"root":"r","version":"` + v + `","rendered":"r","digest":"d","launcher":` + strconv.Quote(exe) + `}`
	}

	writeSetupState(t, h.state(), bundle("0.8.0"))
	stdout, stderr := mcp()
	bin := variant.Current.Binary
	want := bin + ": the plugin files registered in Codex are from " + bin + " 0.8.0 and this " + bin + " is 0.8.2; run `" + variant.Current.Command("init", "dev") + "` to refresh them\n"
	if stderr != want {
		t.Fatalf("stderr = %q\nwant     %q", stderr, want)
	}
	if stdout != "" {
		t.Fatalf("mcp wrote to stdout outside the protocol: %q", stdout)
	}

	writeSetupState(t, h.state(), bundle("0.8.2"))
	if _, stderr := mcp(); stderr != "" {
		t.Fatalf("current plugin files reported: %q", stderr)
	}

	// A build without a release version has no plugin files to refresh to.
	app.Version = "0.0.0-dev"
	writeSetupState(t, h.state(), bundle("0.8.0"))
	if _, stderr := mcp(); stderr != "" {
		t.Fatalf("a development build reported: %q", stderr)
	}
}

// The startup check for a registration whose kenaz is gone looks at the
// Fleet environment the command runs in (--env), not only the default one.
func TestStartupRegistrationCheckFollowsEnvFlag(t *testing.T) {
	other := ""
	for _, e := range variant.Current.AllowedEnvs {
		if e != "dev" { // the harness's default environment
			other = e
			break
		}
	}
	if other == "" {
		t.Skip("this build connects to one Fleet environment")
	}
	h := newHarness(t, stub.New())
	gone := filepath.Join(h.home, "gone")
	writeSetupState(t, filepath.Join(h.home, "plugin", other), `{"root":`+strconv.Quote(gone)+`,"version":"0.5.0","rendered":"r","digest":"d"}`)
	const msg = "that no longer exists"
	for _, args := range [][]string{{"status", "--json", "--env", other}, {"status", "--env=" + other, "--json"}} {
		if _, _, errb := h.run(t, args...); !strings.Contains(errb, "Codex points at a "+variant.Current.Binary+" "+msg) {
			t.Fatalf("%v stderr: %q", args, errb)
		}
	}
	// The default environment's registrations are fine, and stay unreported.
	if _, _, errb := h.run(t, "status", "--json"); strings.Contains(errb, msg) {
		t.Fatalf("status without --env reported another environment's registration: %q", errb)
	}
}
