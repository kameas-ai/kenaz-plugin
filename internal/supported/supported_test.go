package supported

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// SupportedToolsEnv names a supported-tools.json to check instead of the
// vendored copy (scripts/check-supported-tools.sh sets it to the live docs
// file at release time).
const SupportedToolsEnv = "KENAZ_SUPPORTED_TOOLS_JSON"

type doc struct {
	Schema int `json:"schema"`
	Tools  []struct {
		ID             string   `json:"id"`
		Name           string   `json:"name"`
		Maker          string   `json:"maker"`
		Connection     string   `json:"connection"`
		UsageTelemetry *bool    `json:"usage_telemetry"`
		Status         string   `json:"status"`
		ListedSince    string   `json:"listed_since"`
		Hosts          []string `json:"hosts"`
	} `json:"tools"`
}

// TestSupportedToolsMatchHosts fails when the docs Supported Tools list and
// this build drift apart: every host this build supports is on the list,
// every on-device host on the list is one this build supports, and each
// row's usage_telemetry says what kenaz actually does for its hosts.
// Connection types are those the legal texts define (on-device, hosted
// connector); a new type needs review by Kameas legal first.
func TestSupportedToolsMatchHosts(t *testing.T) {
	path := os.Getenv(SupportedToolsEnv)
	if path == "" {
		path = "testdata/supported-tools.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if d.Schema != 1 {
		t.Fatalf("%s: schema %d, want 1", path, d.Schema)
	}

	want := map[string]bool{} // host -> usage telemetry
	for _, h := range Hosts() {
		want[h.ID] = h.UsageTelemetry
	}
	listed := map[string]bool{}
	for _, tool := range d.Tools {
		if tool.ID == "" || tool.Name == "" || tool.Maker == "" || tool.Status == "" || tool.ListedSince == "" || tool.UsageTelemetry == nil {
			t.Errorf("%s: row %q is missing a required field", path, tool.ID)
			continue
		}
		switch tool.Connection {
		case "on-device":
		case "hosted-connector":
			continue // served by Fleet, not by this program
		default:
			t.Errorf("%s: row %q has connection %q; only on-device and hosted-connector are defined (a new type needs review by Kameas legal)", path, tool.ID, tool.Connection)
			continue
		}
		if len(tool.Hosts) == 0 {
			t.Errorf("%s: on-device row %q names no hosts", path, tool.ID)
		}
		for _, h := range tool.Hosts {
			tel, ok := want[h]
			if !ok {
				t.Errorf("%s: row %q lists host %q, which this build does not support", path, tool.ID, h)
				continue
			}
			if listed[h] {
				t.Errorf("%s: host %q is listed twice", path, h)
			}
			listed[h] = true
			if tel != *tool.UsageTelemetry {
				t.Errorf("%s: row %q says usage_telemetry=%v, but kenaz relays usage telemetry for %q: %v", path, tool.ID, *tool.UsageTelemetry, h, tel)
			}
		}
	}
	var missing []string
	for h := range want {
		if !listed[h] {
			missing = append(missing, h)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s: this build supports %v but the Supported Tools list does not name them; add the rows (and a change-log line) before release", path, missing)
	}
}
