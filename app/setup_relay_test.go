package app_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
)

// `kenaz setup` before the relay has ever run starts it itself instead of
// failing.
func TestSetupStartsRelay(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	h := newHarness(t, stub.New())
	spawned := 0
	var out, errb bytes.Buffer
	env := h.env(context.Background(), &out, &errb)
	env.Spawn = func(dir string, e fleetapi.Env) error {
		spawned++
		if e != "dev" || dir != h.state() {
			t.Errorf("spawn %s %s", dir, e)
		}
		return os.WriteFile(filepath.Join(dir, "daemon.json"), []byte(`{"pid":1,"port":47318,"version":"t","nonce":"n"}`), 0o600)
	}
	code := app.RunEnv(env, []string{"setup", "claude", "--yes", "--no-bundle"})
	if code != 0 || spawned != 1 {
		t.Fatalf("exit %d spawned %d\n%s\n%s", code, spawned, out.String(), errb.String())
	}
	got, _ := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if !strings.Contains(string(got), "127.0.0.1:47318") || !strings.Contains(out.String(), "Starting the Kenaz relay") {
		t.Fatalf("%s\n%s", out.String(), got)
	}
}
