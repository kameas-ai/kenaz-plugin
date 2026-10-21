package app_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
)

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := app.Run(context.Background(), stub.New(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestLoginUnofficialBuild(t *testing.T) {
	code, _, stderr := run("login", "--env", "dev")
	if code != app.ExitUnofficial {
		t.Fatalf("exit = %d, want %d", code, app.ExitUnofficial)
	}
	if !strings.Contains(stderr, fleetapi.ErrUnofficialBuild.Error()) {
		t.Fatalf("stderr missing unofficial message: %q", stderr)
	}
}

func TestVersionReportsFlavor(t *testing.T) {
	code, out, _ := run("--version")
	if code != app.ExitOK || !strings.Contains(out, "unofficial build") {
		t.Fatalf("exit=%d out=%q", code, out)
	}
}

func TestHookNeverFails(t *testing.T) {
	for _, ev := range []string{"session-start", "session-end", "bogus"} {
		if code, out, _ := run("hook", ev); code != 0 || out != "" {
			t.Errorf("hook %s: exit=%d out=%q", ev, code, out)
		}
	}
}

func TestLicensesPrintsApacheAndNotice(t *testing.T) {
	code, out, _ := run("licenses")
	if code != 0 || !strings.Contains(out, "Apache License") || !strings.Contains(out, "Kameas AI, Inc.") {
		t.Fatalf("exit=%d", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	if code, _, _ := run("frobnicate"); code != app.ExitUsage {
		t.Fatalf("exit = %d", code)
	}
}
