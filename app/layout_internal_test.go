package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/config"
)

// During `kenaz init` the report never tells the user to run init.
func TestReportMigrationDuringInit(t *testing.T) {
	res := config.MigrateResult{Moved: []string{"/old/state"}, LinkFailed: "no link to /new could be left at /old/state: denied"}
	cur := config.Paths{StateRoot: "/new", ConfigDir: "/new"}
	var b bytes.Buffer
	reportMigration(&b, res, cur, "init")
	if strings.Contains(b.String(), "init`") || !strings.Contains(b.String(), "registers the apps from /new") {
		t.Fatalf("init:\n%s", b.String())
	}
	b.Reset()
	reportMigration(&b, res, cur, "migrate")
	if !strings.Contains(b.String(), "init`") {
		t.Fatalf("migrate:\n%s", b.String())
	}
}
