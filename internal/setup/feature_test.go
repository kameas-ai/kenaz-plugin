package setup

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
)

func setOpenCode(t *testing.T, on bool) {
	t.Helper()
	old := features.OpenCode
	features.OpenCode = on
	t.Cleanup(func() { features.OpenCode = old })
}

// Shipped builds leave OpenCode out: `kenaz setup opencode` and its undo
// are not offered or listed, doctor and status do not mention it, and
// opencode telemetry is dropped on the device whatever schema Fleet
// advertises. A build with the switch on offers all of it.
func TestOpenCodeSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		setOpenCode(t, on)
		if _, ok := parseHarness("opencode"); ok != on {
			t.Errorf("on=%v: parseHarness(opencode) = %v", on, ok)
		}
		if strings.Contains(Usage(), "opencode") != on {
			t.Errorf("on=%v: usage = %q", on, Usage())
		}
		for _, args := range [][]string{{"opencode", "--dry-run"}, {"--undo", "opencode", "--dry-run"}} {
			if on {
				break // the real command would read this machine's config
			}
			var errOut bytes.Buffer
			code := Command(context.Background(), args, &bytes.Buffer{}, &errOut)
			if code != exitUsage || strings.Contains(errOut.String(), "opencode") {
				t.Errorf("off: setup %v = %d %q", args, code, errOut.String())
			}
		}
		hosts := HostStates(t.TempDir())
		mentions := false
		for _, h := range hosts {
			mentions = mentions || h.Host == "opencode"
		}
		if mentions != on {
			t.Errorf("on=%v: host states %+v", on, hosts)
		}
		e, _ := openCodeEnv(t)
		run := func(context.Context, string, ...string) (string, error) { return "", errors.New("not found") }
		named := false
		for _, c := range Doctor(context.Background(), e.opts, run) {
			named = named || c.Name == "OpenCode" || strings.Contains(c.Detail, "opencode")
		}
		if named != on {
			t.Errorf("on=%v: doctor mentions OpenCode = %v", on, named)
		}
		if fleetschema.ClientKindSupported("opencode", 99) != on {
			t.Errorf("on=%v: opencode telemetry at a high schema", on)
		}
	}
}
