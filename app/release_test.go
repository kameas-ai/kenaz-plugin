package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
)

func TestVersionJSONAndExtraLicenses(t *testing.T) {
	var out, errb bytes.Buffer
	if code := app.Run(context.Background(), stub.New(), []string{"version", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var bi app.BuildInfo
	if err := json.Unmarshal(out.Bytes(), &bi); err != nil {
		t.Fatal(err)
	}
	if bi.Flavor != "unofficial" || bi.PublicCommit != "unknown" || bi.GoVersion == "" {
		t.Fatalf("unexpected provenance %+v", bi)
	}

	app.ExtraLicenses = []app.License{{Name: "LICENSE", Text: "Kameas Client Software License (test)"}}
	defer func() { app.ExtraLicenses = nil }()
	out.Reset()
	if code := app.Run(context.Background(), stub.New(), []string{"licenses"}, &out, &errb); code != 0 {
		t.Fatalf("licenses exit %d", code)
	}
	for _, want := range []string{"== NOTICE ==", "== THIRD_PARTY_NOTICES ==", "github.com/zalando/go-keyring", "Kameas Client Software License (test)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("licenses output missing %q", want)
		}
	}
}
