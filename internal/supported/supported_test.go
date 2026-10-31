package supported

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/features"
)

// SupportedToolsEnv names a supported-tools.json to check instead of the
// vendored copy (scripts/check-supported-tools.sh sets it to the live docs
// file at release time).
const SupportedToolsEnv = "KENAZ_SUPPORTED_TOOLS_JSON"

type doc struct {
	Schema int       `json:"schema"`
	Tools  []toolRow `json:"tools"`
}

type toolRow struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Maker          string   `json:"maker"`
	Connection     string   `json:"connection"`
	UsageTelemetry *bool    `json:"usage_telemetry"`
	Status         string   `json:"status"`
	ListedSince    string   `json:"listed_since"`
	Hosts          []string `json:"hosts"`
}

// TestSupportedToolsMatchHosts fails when the docs Supported Tools list and
// this build drift apart: every host this build supports is on the list,
// every on-device host on the list is one this build supports or a known
// feature-gated host (GatedHosts) that this build leaves off, and each
// row's usage_telemetry says what kenaz actually does for its hosts.
// Connection types are those the legal texts define (on-device, hosted
// connector); a new type needs review by Kameas legal first.
//
// The release check (SupportedToolsEnv set, the live docs JSON, run with
// the release's build tags) requires the list to match this build exactly,
// in both directions: a gated host this build lacks fails, and so does one
// it supports that the list leaves out. Checking the vendored copy is
// lenient about gated hosts only: one CI run covers every build mode, and
// the copy is synced to the docs at release time, so until then it may
// predate the gated rows.
func TestSupportedToolsMatchHosts(t *testing.T) {
	path := os.Getenv(SupportedToolsEnv)
	release := path != ""
	if !release {
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
	problems, pending := checkTools(d, Hosts(), GatedHosts(), release)
	for _, p := range problems {
		t.Errorf("%s: %s", path, p)
	}
	if len(pending) > 0 {
		msg := fmt.Sprintf("%s: this build supports %v but the Supported Tools list does not name them; add the rows (and a change-log line) before release", path, pending)
		if release {
			t.Error(msg)
		} else {
			t.Log(msg + " (the release check enforces this against the live docs list)")
		}
	}
}

// checkTools checks d against the hosts a build supports and the known
// feature-gated hosts. problems always fail; pending lists the gated hosts
// the build supports that d leaves out (the caller fails them in release
// mode). Only when !release is a row for a gated host this build lacks
// accepted; in release mode it is a problem.
func checkTools(d doc, hosts, gated []Host, release bool) (problems, pending []string) {
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	if d.Schema != 1 {
		bad("schema %d, want 1", d.Schema)
		return problems, nil
	}
	want := map[string]bool{} // host -> usage telemetry
	for _, h := range hosts {
		want[h.ID] = h.UsageTelemetry
	}
	isGated := map[string]bool{}
	gatedTel := map[string]bool{}
	for _, h := range gated {
		isGated[h.ID] = true
		gatedTel[h.ID] = h.UsageTelemetry
	}
	listed := map[string]bool{}
	for _, tool := range d.Tools {
		if tool.ID == "" || tool.Name == "" || tool.Maker == "" || tool.Status == "" || tool.ListedSince == "" || tool.UsageTelemetry == nil {
			bad("row %q is missing a required field", tool.ID)
			continue
		}
		switch tool.Connection {
		case "on-device":
		case "hosted-connector":
			continue // served by Fleet, not by this program
		default:
			bad("row %q has connection %q; only on-device and hosted-connector are defined (a new type needs review by Kameas legal)", tool.ID, tool.Connection)
			continue
		}
		if len(tool.Hosts) == 0 {
			bad("on-device row %q names no hosts", tool.ID)
		}
		for _, h := range tool.Hosts {
			tel, ok := want[h]
			if !ok && isGated[h] {
				if release {
					bad("row %q lists host %q, which only builds with its release feature support, and this release build does not", tool.ID, h)
					continue
				}
				// Supported by builds with the feature, not this one.
				tel, ok = gatedTel[h], true
			}
			if !ok {
				bad("row %q lists host %q, which this build does not support and is not a known feature-gated host", tool.ID, h)
				continue
			}
			if listed[h] {
				bad("host %q is listed twice", h)
			}
			listed[h] = true
			if tel != *tool.UsageTelemetry {
				bad("row %q says usage_telemetry=%v, but kenaz relays usage telemetry for %q: %v", tool.ID, *tool.UsageTelemetry, h, tel)
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
	for _, h := range missing {
		if isGated[h] {
			pending = append(pending, h)
		} else {
			problems = append(problems, fmt.Sprintf("this build supports %q but the Supported Tools list does not name it; add the row (and a change-log line) before release", h))
		}
	}
	return problems, pending
}

// row is an on-device Supported Tools row for hosts.
func row(id string, telemetry bool, hosts ...string) toolRow {
	return toolRow{ID: id, Name: id, Maker: "maker", Connection: "on-device", UsageTelemetry: &telemetry, Status: "available", ListedSince: "2026-10-31", Hosts: hosts}
}

// withFeatures runs fn with features.ClaudeDesktopChat and features.Cowork
// set as given, so one test binary covers every build mode.
func withFeatures(t *testing.T, chat, cowork bool, fn func()) {
	t.Helper()
	oldChat, oldCowork := features.ClaudeDesktopChat, features.Cowork
	features.ClaudeDesktopChat, features.Cowork = chat, cowork
	defer func() { features.ClaudeDesktopChat, features.Cowork = oldChat, oldCowork }()
	fn()
}

// buildModes are every combination of the two gated features.
var buildModes = []struct{ chat, cowork bool }{{false, false}, {true, false}, {false, true}, {true, true}}

// enabledGated lists, sorted, the gated hosts a build mode supports, minus
// those in listed.
func enabledGated(chat, cowork bool, listed ...string) []string {
	skip := map[string]bool{}
	for _, h := range listed {
		skip[h] = true
	}
	var out []string
	if chat && !skip["claude-desktop"] {
		out = append(out, "claude-desktop")
	}
	if cowork && !skip[HostCowork] {
		out = append(out, HostCowork)
	}
	return out
}

// Checking the vendored copy (not release mode): a list naming any subset of
// the gated hosts passes every build mode, and a gated host the build
// supports but the list leaves out is pending (release-blocking). Unknown or
// misspelled hosts, and a wrong usage_telemetry on a gated row, fail in
// every mode.
func TestCheckToolsGatedHostsAllBuilds(t *testing.T) {
	base := []toolRow{row("claude-code", true, "claude-code"), row("codex", true, "codex")}
	plus := func(rows ...toolRow) []toolRow { return append(append([]toolRow(nil), base...), rows...) }
	chatRow, coworkRow := row("claude-desktop-chat", false, "claude-desktop"), row("cowork", false, "cowork")
	cases := []struct {
		name         string
		tools        []toolRow
		listed       []string // gated hosts the list names
		problems     bool
		problemMatch string
	}{
		{name: "both gated rows", tools: plus(chatRow, coworkRow), listed: []string{"claude-desktop", "cowork"}},
		{name: "v0.7.0 list (chat only)", tools: plus(chatRow), listed: []string{"claude-desktop"}},
		{name: "cowork row only", tools: plus(coworkRow), listed: []string{"cowork"}},
		{name: "without gated rows", tools: base},
		{name: "misspelled gated host", tools: plus(row("claude-desktop-chat", false, "claude-desktp")), problems: true, problemMatch: `"claude-desktp"`},
		{name: "unknown host", tools: plus(chatRow, coworkRow, row("windsurf", true, "windsurf")), listed: []string{"claude-desktop", "cowork"}, problems: true, problemMatch: `"windsurf"`},
		{name: "gated row claims telemetry", tools: plus(row("claude-desktop-chat", true, "claude-desktop")), listed: []string{"claude-desktop"}, problems: true, problemMatch: "usage_telemetry=true"},
		{name: "cowork row claims telemetry", tools: plus(row("cowork", true, "cowork")), listed: []string{"cowork"}, problems: true, problemMatch: "usage_telemetry=true"},
		{name: "gated host listed twice", tools: plus(chatRow, coworkRow, row("cowork-again", false, "cowork")), listed: []string{"claude-desktop", "cowork"}, problems: true, problemMatch: "listed twice"},
		{name: "always-supported host missing", tools: []toolRow{row("codex", true, "codex")}, problems: true, problemMatch: `"claude-code"`},
	}
	for _, tc := range cases {
		for _, m := range buildModes {
			withFeatures(t, m.chat, m.cowork, func() {
				problems, pending := checkTools(doc{Schema: 1, Tools: tc.tools}, Hosts(), GatedHosts(), false)
				if (len(problems) > 0) != tc.problems {
					t.Errorf("%s, %+v: problems = %q, want any = %v", tc.name, m, problems, tc.problems)
				}
				if tc.problems && tc.problemMatch != "" && !strings.Contains(strings.Join(problems, "\n"), tc.problemMatch) {
					t.Errorf("%s, %+v: problems %q do not mention %s", tc.name, m, problems, tc.problemMatch)
				}
				wantPending := enabledGated(m.chat, m.cowork, tc.listed...)
				if strings.Join(pending, ",") != strings.Join(wantPending, ",") {
					t.Errorf("%s, %+v: pending = %v, want %v", tc.name, m, pending, wantPending)
				}
			})
		}
	}
}

// The release check needs the list to match the release build exactly. The
// v0.7.0 list (the Claude desktop app's chat, no Cowork) passes only the
// build with the chat feature and without Cowork; a list with a Cowork row
// fails any build without features.Cowork. Misspellings fail every build.
func TestCheckToolsReleaseMatchesBuildExactly(t *testing.T) {
	base := []toolRow{row("claude-code", true, "claude-code"), row("codex", true, "codex")}
	plus := func(rows ...toolRow) []toolRow { return append(append([]toolRow(nil), base...), rows...) }
	chatRow, coworkRow := row("claude-desktop-chat", false, "claude-desktop"), row("cowork", false, "cowork")
	cases := []struct {
		name   string
		tools  []toolRow
		passIn func(chat, cowork bool) bool
	}{
		{name: "v0.7.0 list (chat only)", tools: plus(chatRow), passIn: func(c, w bool) bool { return c && !w }},
		{name: "both gated rows", tools: plus(chatRow, coworkRow), passIn: func(c, w bool) bool { return c && w }},
		{name: "cowork row only", tools: plus(coworkRow), passIn: func(c, w bool) bool { return !c && w }},
		{name: "without gated rows", tools: base, passIn: func(c, w bool) bool { return !c && !w }},
		{name: "misspelled gated host", tools: plus(row("claude-desktop-chat", false, "claude-desktp")), passIn: func(c, w bool) bool { return false }},
	}
	for _, tc := range cases {
		for _, m := range buildModes {
			withFeatures(t, m.chat, m.cowork, func() {
				problems, pending := checkTools(doc{Schema: 1, Tools: tc.tools}, Hosts(), GatedHosts(), true)
				fails := len(problems) > 0 || len(pending) > 0 // the release check fails pending too
				if want := !tc.passIn(m.chat, m.cowork); fails != want {
					t.Errorf("%s, %+v: release check fails = %v (problems %q, pending %v), want %v", tc.name, m, fails, problems, pending, want)
				}
				if !m.cowork && hasHost(tc.tools, HostCowork) && !strings.Contains(strings.Join(problems, "\n"), "this release build does not") {
					t.Errorf("%s, %+v: a cowork row in a build without Cowork must be rejected: %q", tc.name, m, problems)
				}
			})
		}
	}
}

func hasHost(rows []toolRow, host string) bool {
	for _, r := range rows {
		for _, h := range r.Hosts {
			if h == host {
				return true
			}
		}
	}
	return false
}

// The vendored copy is the v0.7.0 docs list: the release check passes it for
// the release build (-tags kenaz_claude_desktop_chat, no kenaz_cowork) and
// fails it for every other build mode.
func TestVendoredListReleaseBuild(t *testing.T) {
	b, err := os.ReadFile("testdata/supported-tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	for _, m := range buildModes {
		withFeatures(t, m.chat, m.cowork, func() {
			problems, pending := checkTools(d, Hosts(), GatedHosts(), true)
			pass := len(problems) == 0 && len(pending) == 0
			if want := m.chat && !m.cowork; pass != want {
				t.Errorf("%+v: release check passes = %v (problems %q, pending %v), want %v", m, pass, problems, pending, want)
			}
		})
	}
}

// GatedHosts names exactly the hosts the gated features add, with the
// telemetry they have when on, whatever this build's tags; each feature adds
// only its own host.
func TestGatedHostsMatchFeature(t *testing.T) {
	ids := func(chat, cowork bool) (hs []Host) {
		withFeatures(t, chat, cowork, func() { hs = Hosts() })
		return hs
	}
	added := func(from, to []Host) (out []Host) {
		in := map[string]bool{}
		for _, h := range from {
			in[h.ID] = true
		}
		for _, h := range to {
			if !in[h.ID] {
				out = append(out, h)
			}
		}
		return out
	}
	off := ids(false, false)
	if got, want := fmt.Sprint(GatedHosts()), fmt.Sprint(added(off, ids(true, true))); got != want {
		t.Fatalf("GatedHosts() = %v, want the hosts the features add: %v", got, want)
	}
	if got := fmt.Sprint(added(off, ids(true, false))); got != fmt.Sprint([]Host{{"claude-desktop", false}}) {
		t.Errorf("the Claude Desktop chat feature adds %v, want claude-desktop only", got)
	}
	if got := fmt.Sprint(added(off, ids(false, true))); got != fmt.Sprint([]Host{{HostCowork, false}}) {
		t.Errorf("the Cowork feature adds %v, want cowork only", got)
	}
	for _, h := range off {
		for _, g := range GatedHosts() {
			if h.ID == g.ID {
				t.Errorf("gated host %q is supported without its feature", h.ID)
			}
		}
	}
}

// The Claude desktop app's chat is listed exactly with
// features.ClaudeDesktopChat and Cowork exactly with features.Cowork, neither
// with usage telemetry. Claude Code and Codex are always listed.
func TestHostsClaudeDesktopChatAndCowork(t *testing.T) {
	for _, m := range buildModes {
		withFeatures(t, m.chat, m.cowork, func() {
			got := map[string]bool{}
			for _, h := range Hosts() {
				got[h.ID] = h.UsageTelemetry
			}
			for id, want := range map[string]bool{"claude-desktop": m.chat, HostCowork: m.cowork} {
				tel, ok := got[id]
				if ok != want {
					t.Errorf("%+v: host %q listed = %v, want %v", m, id, ok, want)
				}
				if tel {
					t.Errorf("host %q reports usage telemetry", id)
				}
			}
			if !got["claude-code"] || !got["codex"] {
				t.Errorf("%+v: Claude Code and Codex must always report usage telemetry: %v", m, got)
			}
		})
	}
}
