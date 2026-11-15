package installed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/test/conformance"
)

// The host's config before setup. A journey never starts from an empty
// file: a real user's config has content of its own, and what setup and
// undo must prove is that they change only the keys setup owns and put
// back exactly what was there. Each seed holds unrelated content, an
// unrelated key inside the container setup writes to, and one owned key
// with a value of the user's (so undo has a prior value to restore, not
// only keys to delete).
const (
	claudeSeed = `{
  "theme": "dark",
  "permissions": {
    "allow": [
      "Bash(ls:*)"
    ]
  },
  "env": {
    "CONFORMANCE_UNRELATED": "kept",
    "OTEL_LOG_USER_PROMPTS": "1"
  }
}
`
	codexSeed = `# A user's own Codex config (conformance seed).
model = "conformance-unrelated"

[otel]
environment = "conformance"
log_user_prompt = true

[profiles.conformance]
model = "conformance-unrelated"
`
)

// hostSeed is the pre-existing config for a host CLI ("claude" or "codex").
func hostSeed(cli string) []byte {
	if cli == "claude" {
		return []byte(claudeSeed)
	}
	return []byte(codexSeed)
}

// userEdit makes the change a user makes to their own config after setup:
// one unrelated key gets another value. ok is false unless exactly that key
// was found. The same edit applied to the seed is what undo must leave.
func userEdit(cli string, config []byte) (edited []byte, ok bool) {
	const head = "# A user's own Codex config (conformance seed).\nmodel = "
	from, to := head+`"conformance-unrelated"`, head+`"conformance-edited"`
	if cli == "claude" {
		from, to = `"theme": "dark"`, `"theme": "light"`
	}
	if bytes.Count(config, []byte(from)) != 1 {
		return nil, false
	}
	return bytes.Replace(config, []byte(from), []byte(to), 1), true
}

// claudeTelemetryEnv is the closed set of settings.json env keys setup owns,
// with the values it must write for a relay at endpoint.
func claudeTelemetryEnv(endpoint string) map[string]string {
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1", "OTEL_LOGS_EXPORTER": "otlp", "OTEL_METRICS_EXPORTER": "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
		"OTEL_LOG_USER_PROMPTS": "0", "OTEL_LOG_ASSISTANT_RESPONSES": "0", "OTEL_LOG_TOOL_DETAILS": "0",
		"OTEL_LOG_TOOL_CONTENT": "0", "OTEL_LOG_RAW_API_BODIES": "0",
	}
}

// claudePluginKeys are the top-level settings.json keys the plugin
// registration (done through the host's own CLI) may change.
var claudePluginKeys = []string{"enabledPlugins", "extraKnownMarketplaces"}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// checkClaudeConfig compares the whole settings.json after setup with the
// file as it was before, and returns every difference setup does not own.
// Setup owns the telemetry keys under env, each with one exact value. With
// the host CLI the plugin registration may also change claudePluginKeys.
// Any other top-level key added, changed or removed is a problem, and so is
// any env key outside the owned set.
func checkClaudeConfig(before, after []byte, endpoint string, hostCLI bool) []string {
	var b, a map[string]json.RawMessage
	if err := json.Unmarshal(after, &a); err != nil {
		return []string{"settings.json does not parse after setup"}
	}
	if err := json.Unmarshal(before, &b); err != nil {
		return []string{"the settings.json from before setup does not parse"}
	}
	var problems []string
	var benv, aenv map[string]json.RawMessage
	_ = json.Unmarshal(b["env"], &benv)
	if err := json.Unmarshal(a["env"], &aenv); err != nil {
		problems = append(problems, "env is not an object")
	}
	want := claudeTelemetryEnv(endpoint)
	for k, v := range want {
		var got string
		if json.Unmarshal(aenv[k], &got) != nil || got != v {
			problems = append(problems, "env."+k+" is not "+v)
		}
	}
	for k, v := range aenv {
		if _, owned := want[k]; owned {
			continue
		}
		if bv, was := benv[k]; !was {
			problems = append(problems, "setup wrote an env key it does not own: "+k)
		} else if !jsonEqual(bv, v) {
			problems = append(problems, "setup changed env."+k+", which it does not own")
		}
	}
	for k := range benv {
		if _, owned := want[k]; !owned && aenv[k] == nil {
			problems = append(problems, "setup removed env."+k+", which it does not own")
		}
	}
	allowed := map[string]bool{"env": true}
	if hostCLI {
		for _, k := range claudePluginKeys {
			allowed[k] = true
		}
	}
	for k, v := range a {
		if allowed[k] {
			continue
		}
		if bv, was := b[k]; !was {
			problems = append(problems, "setup added a top-level settings.json key it does not own: "+k)
		} else if !jsonEqual(bv, v) {
			problems = append(problems, "setup changed the top-level settings.json key "+k+", which it does not own")
		}
	}
	for k := range b {
		if _, still := a[k]; !still && !allowed[k] {
			problems = append(problems, "setup removed the top-level settings.json key "+k)
		}
	}
	sort.Strings(problems)
	return problems
}

// codexOwnedOtel is the closed set of [otel] keys setup owns, with the
// exact values it must write for a relay at endpoint (spaces ignored).
func codexOwnedOtel(endpoint string) map[string]string {
	exp := func(signal string) string {
		return fmt.Sprintf(`{ otlp-http = { endpoint = "%s/v1/%s", protocol = "binary" } }`, endpoint, signal)
	}
	return map[string]string{
		"exporter": exp("logs"), "trace_exporter": exp("traces"), "metrics_exporter": exp("metrics"),
		"log_user_prompt": "false", "tool_result": "{ max_bytes = 0 }",
	}
}

// codexPluginTables are the config.toml tables the plugin registration
// (done through the host's own CLI) may add.
var codexPluginTables = []string{"[marketplaces.kenaz]", `[plugins."kenaz@kenaz"]`}

// tomlTables splits a config.toml into its tables: header ("" for the keys
// before the first table) to the table's non-blank lines, trimmed. It reads
// only what setup, the seed and the host write: one key per line.
func tomlTables(text string) map[string][]string {
	out := map[string][]string{}
	header := ""
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			header = line
			if _, ok := out[header]; !ok {
				out[header] = []string{}
			}
		default:
			out[header] = append(out[header], line)
		}
	}
	return out
}

func noSpace(s string) string { return strings.Join(strings.Fields(s), "") }

// checkCodexConfig compares the whole config.toml after setup with the file
// as it was before, and returns every difference setup does not own. Setup
// owns five keys of [otel], each with one exact value. With the host CLI
// the plugin registration may also add codexPluginTables. Every other
// table and key must be as it was.
func checkCodexConfig(before, after []byte, endpoint string, hostCLI bool) []string {
	bt, at := tomlTables(string(before)), tomlTables(string(after))
	owned := codexOwnedOtel(endpoint)
	var problems []string
	otel, has := at["[otel]"]
	if !has {
		problems = append(problems, "config.toml has no [otel] table")
	}
	// rest returns the lines of an [otel] table that are not owned keys.
	rest := func(lines []string, seen map[string]string) []string {
		var out []string
		for _, l := range lines {
			k, v, ok := strings.Cut(l, "=")
			if _, own := owned[strings.TrimSpace(k)]; ok && own {
				if seen != nil {
					if _, dup := seen[strings.TrimSpace(k)]; dup {
						problems = append(problems, "[otel] "+strings.TrimSpace(k)+" is set twice")
					}
					seen[strings.TrimSpace(k)] = noSpace(v)
				}
				continue
			}
			out = append(out, l)
		}
		return out
	}
	got := map[string]string{}
	afterRest := rest(otel, got)
	for k, want := range owned {
		if v, ok := got[k]; !ok {
			problems = append(problems, "[otel] has no "+k)
		} else if v != noSpace(want) {
			problems = append(problems, "[otel] "+k+" is not exactly "+want)
		}
	}
	if !reflect.DeepEqual(afterRest, rest(bt["[otel]"], nil)) {
		problems = append(problems, "setup added, changed or removed an [otel] key it does not own")
	}
	allowed := map[string]bool{"[otel]": true}
	if hostCLI {
		for _, t := range codexPluginTables {
			allowed[t] = true
		}
	}
	name := func(t string) string {
		if t == "" {
			return "the keys before the first table"
		}
		return "table " + t
	}
	for t, lines := range at {
		if allowed[t] {
			continue
		}
		if was, ok := bt[t]; !ok {
			problems = append(problems, "setup added "+name(t)+", which it does not own")
		} else if !reflect.DeepEqual(was, lines) {
			problems = append(problems, "setup changed "+name(t)+", which it does not own")
		}
	}
	for t := range bt {
		if _, still := at[t]; !still && !allowed[t] {
			problems = append(problems, "setup removed "+name(t))
		}
	}
	sort.Strings(problems)
	return problems
}

// pluginRegistered reports whether a host's config still registers the
// kenaz plugin or its marketplace.
func pluginRegistered(cli string, config []byte) bool {
	if cli == "claude" {
		var c struct {
			EnabledPlugins map[string]json.RawMessage `json:"enabledPlugins"`
			Marketplaces   map[string]json.RawMessage `json:"extraKnownMarketplaces"`
		}
		if json.Unmarshal(config, &c) != nil {
			return bytes.Contains(config, []byte("kenaz@kenaz"))
		}
		_, plugin := c.EnabledPlugins["kenaz@kenaz"]
		_, market := c.Marketplaces["kenaz"]
		return plugin || market
	}
	t := tomlTables(string(config))
	for _, table := range codexPluginTables {
		if _, ok := t[table]; ok {
			return true
		}
	}
	return false
}

// checkUndone compares the host's config after `kenaz setup --undo` with
// want, the file as it must be again: it must no longer point at the relay,
// no longer register the plugin, and be byte-identical to want (undo
// restores the user's prior values and removes what setup added, nothing
// more). after is nil when the file is gone.
//
// hostRewrote is for an undo in which the host's own CLI rewrote the file
// while unregistering the plugin, after kenaz could no longer restore a
// snapshot: a host's writer may reorder keys and leave empty plugin maps,
// which no undo of ours controls. Then, and only then, a file that is not
// byte-identical still passes when it holds the same content (sameContent);
// identical reports which of the two held.
func checkUndone(cli string, want, after []byte, endpoint string, hostRewrote bool) (problems []string, identical bool) {
	if after == nil {
		return []string{"the host's config file is gone after --undo; it existed before setup"}, false
	}
	if bytes.Contains(after, []byte(strings.TrimPrefix(endpoint, "http://"))) {
		problems = append(problems, "the host's config still points at the relay after --undo")
	}
	if pluginRegistered(cli, after) {
		problems = append(problems, "the plugin is still registered in the host's config after --undo")
	}
	identical = bytes.Equal(want, after)
	if !identical && !(hostRewrote && sameContent(cli, want, after)) {
		problems = append(problems, "the host's config after --undo is not byte-identical to the file as it was before setup ("+firstDifference(want, after)+")")
	}
	return problems, identical
}

// sameContent reports whether two host configs hold the same keys and
// values, ignoring what a host's own writer changes: for settings.json, key
// order, whitespace and plugin maps left empty; for config.toml, blank
// lines and the order of tables.
func sameContent(cli string, want, got []byte) bool {
	if cli != "claude" {
		return reflect.DeepEqual(tomlTables(string(want)), tomlTables(string(got)))
	}
	var w, g map[string]any
	if json.Unmarshal(want, &w) != nil || json.Unmarshal(got, &g) != nil {
		return false
	}
	for _, m := range []map[string]any{w, g} {
		for _, k := range claudePluginKeys {
			if v, ok := m[k].(map[string]any); ok && len(v) == 0 {
				delete(m, k)
			}
		}
	}
	return reflect.DeepEqual(w, g)
}

// firstDifference says where two configs first differ, by line. The lines
// are the journey's synthetic seed or what setup and the host wrote.
func firstDifference(want, got []byte) string {
	w, g := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("first difference at line %d: want %q, got %q; %d bytes, want %d", i+1, wl, gl, len(got), len(want))
		}
	}
	return "same lines"
}

// listingResult is one host listing command's outcome.
type listingResult struct {
	name string // the command, for the record
	code int    // exit code
	out  string // combined output
	why  string // on exit 0: "" when the host shows the plugin, else what is wrong
}

// listingVerdict is how the host-visibility step ends.
type listingVerdict struct {
	status     string // conformance.StepPass, StepFail or StepNotRun
	visibility string // conformance.VisibilityVerified, VisibilityNeedsSignIn or VisibilityNotRun
	worked     []string
	needSignIn []string
	problems   []string
}

// classifyListings decides the host-visibility step from the listing
// commands' outcomes. A listing that exits 0 either shows the plugin or is
// a problem. One that exits non-zero is "needs sign-in" only when its
// output says so; any other non-zero exit is a failure, never "not run":
// a host that cannot list its plugins has not shown that it sees this one.
func classifyListings(results []listingResult) listingVerdict {
	v := listingVerdict{status: conformance.StepNotRun, visibility: conformance.VisibilityNotRun}
	for _, r := range results {
		switch {
		case r.code == 0 && r.why == "":
			v.worked = append(v.worked, r.name)
		case r.code == 0:
			v.problems = append(v.problems, r.name+": "+r.why)
		case signInRe.MatchString(r.out):
			v.needSignIn = append(v.needSignIn, r.name)
		default:
			v.problems = append(v.problems, fmt.Sprintf("%s: exit %d without asking for sign-in: %s", r.name, r.code, lastLine(r.out)))
		}
	}
	switch {
	case len(v.problems) > 0:
		v.status = conformance.StepFail
		if len(v.needSignIn) > 0 {
			v.visibility = conformance.VisibilityNeedsSignIn
		}
	case len(v.worked) > 0:
		v.status, v.visibility = conformance.StepPass, conformance.VisibilityVerified
	case len(v.needSignIn) > 0:
		v.visibility = conformance.VisibilityNeedsSignIn
	}
	return v
}
