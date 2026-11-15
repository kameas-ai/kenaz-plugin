package installed

import (
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/test/conformance"
)

const testEndpoint = "http://127.0.0.1:4318"

// What a correct setup leaves in each host's config, starting from the
// seed. The mutants below are edits of these.
const (
	claudeAfterSetup = `{
  "theme": "dark",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "env": {
    "CONFORMANCE_UNRELATED": "kept",
    "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
    "OTEL_LOGS_EXPORTER": "otlp",
    "OTEL_METRICS_EXPORTER": "otlp",
    "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
    "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
    "OTEL_LOG_USER_PROMPTS": "0",
    "OTEL_LOG_ASSISTANT_RESPONSES": "0",
    "OTEL_LOG_TOOL_DETAILS": "0",
    "OTEL_LOG_TOOL_CONTENT": "0",
    "OTEL_LOG_RAW_API_BODIES": "0"
  }
}
`
	codexAfterSetup = `# A user's own Codex config (conformance seed).
model = "conformance-unrelated"

[otel]
environment = "conformance"
log_user_prompt = false
exporter = { otlp-http = { endpoint = "http://127.0.0.1:4318/v1/logs", protocol = "binary" } }
trace_exporter = { otlp-http = { endpoint = "http://127.0.0.1:4318/v1/traces", protocol = "binary" } }
metrics_exporter = { otlp-http = { endpoint = "http://127.0.0.1:4318/v1/metrics", protocol = "binary" } }
tool_result = { max_bytes = 0 }

[profiles.conformance]
model = "conformance-unrelated"
`
	claudeRegistered = `,
  "enabledPlugins": {"kenaz@kenaz": true},
  "extraKnownMarketplaces": {"kenaz": {"source": {"source": "directory", "path": "/x"}}}
}
`
	codexRegistered = `
[marketplaces.kenaz]
source = "/x"

[plugins."kenaz@kenaz"]
enabled = true
`
)

// host-config-telemetry compares the whole file. The reviewer's mutant
// (setup writes an extra top-level settings.json key) and its relatives
// each fail it; the correct result passes.
func TestClaudeConfigAllowsOnlyOwnedKeys(t *testing.T) {
	seed := []byte(claudeSeed)
	if p := checkClaudeConfig(seed, []byte(claudeAfterSetup), testEndpoint, false); len(p) != 0 {
		t.Fatalf("a correct setup is refused: %v", p)
	}
	withPlugin := strings.TrimSuffix(claudeAfterSetup, "\n}\n") + claudeRegistered
	if p := checkClaudeConfig(seed, []byte(withPlugin), testEndpoint, true); len(p) != 0 {
		t.Fatalf("a correct setup with the host CLI is refused: %v", p)
	}
	for name, m := range map[string]struct {
		after string
		want  string
	}{
		"extra top-level key":              {strings.Replace(claudeAfterSetup, `"theme": "dark",`, `"theme": "dark", "kenazTelemetry": true,`, 1), "added a top-level settings.json key it does not own: kenazTelemetry"},
		"changed user key":                 {strings.Replace(claudeAfterSetup, `"dark"`, `"light"`, 1), "changed the top-level settings.json key theme"},
		"removed user key":                 {strings.Replace(claudeAfterSetup, `"theme": "dark",`, ``, 1), "removed the top-level settings.json key theme"},
		"nested user change":               {strings.Replace(claudeAfterSetup, `"Bash(ls:*)"`, `"Bash(ls:*)", "Bash(rm:*)"`, 1), "changed the top-level settings.json key permissions"},
		"extra env key":                    {strings.Replace(claudeAfterSetup, `"CONFORMANCE_UNRELATED": "kept",`, `"CONFORMANCE_UNRELATED": "kept", "OTEL_TRACES_EXPORTER": "otlp",`, 1), "wrote an env key it does not own: OTEL_TRACES_EXPORTER"},
		"changed user env":                 {strings.Replace(claudeAfterSetup, `"kept"`, `"lost"`, 1), "changed env.CONFORMANCE_UNRELATED"},
		"removed user env":                 {strings.Replace(claudeAfterSetup, `"CONFORMANCE_UNRELATED": "kept",`, ``, 1), "removed env.CONFORMANCE_UNRELATED"},
		"content logging on":               {strings.Replace(claudeAfterSetup, `"OTEL_LOG_USER_PROMPTS": "0"`, `"OTEL_LOG_USER_PROMPTS": "1"`, 1), "env.OTEL_LOG_USER_PROMPTS is not 0"},
		"another endpoint":                 {strings.Replace(claudeAfterSetup, `127.0.0.1:4318`, `127.0.0.1:4319`, 1), "env.OTEL_EXPORTER_OTLP_ENDPOINT is not"},
		"plugin keys without the host CLI": {withPlugin, "added a top-level settings.json key it does not own: enabledPlugins"},
	} {
		p := checkClaudeConfig(seed, []byte(m.after), testEndpoint, false)
		if !strings.Contains(strings.Join(p, "; "), m.want) {
			t.Errorf("%s: problems %v, want %q", name, p, m.want)
		}
	}
}

// The Codex side: exact owned keys and values, not their mere presence,
// and nothing else in the file changed.
func TestCodexConfigAllowsOnlyOwnedKeys(t *testing.T) {
	seed := []byte(codexSeed)
	if p := checkCodexConfig(seed, []byte(codexAfterSetup), testEndpoint, false); len(p) != 0 {
		t.Fatalf("a correct setup is refused: %v", p)
	}
	if p := checkCodexConfig(seed, []byte(codexAfterSetup+codexRegistered), testEndpoint, true); len(p) != 0 {
		t.Fatalf("a correct setup with the host CLI is refused: %v", p)
	}
	crlf := strings.ReplaceAll(codexAfterSetup, "\n", "\r\n")
	if p := checkCodexConfig(seed, []byte(crlf), testEndpoint, false); len(p) != 0 {
		t.Fatalf("CRLF line endings are refused: %v", p)
	}
	for name, m := range map[string]struct {
		after string
		want  string
	}{
		"extra top-level key":   {strings.Replace(codexAfterSetup, "model = \"conformance-unrelated\"\n\n[otel]", "model = \"conformance-unrelated\"\nkenaz_telemetry = true\n\n[otel]", 1), "changed the keys before the first table"},
		"extra table":           {codexAfterSetup + "\n[kenaz]\nenabled = true\n", "added table [kenaz]"},
		"changed user table":    {strings.Replace(codexAfterSetup, "[profiles.conformance]\nmodel = \"conformance-unrelated\"", "[profiles.conformance]\nmodel = \"other\"", 1), "changed table [profiles.conformance]"},
		"removed user table":    {strings.Replace(codexAfterSetup, "[profiles.conformance]\nmodel = \"conformance-unrelated\"\n", "", 1), "removed table [profiles.conformance]"},
		"extra otel key":        {strings.Replace(codexAfterSetup, "tool_result = { max_bytes = 0 }", "tool_result = { max_bytes = 0 }\nlog_assistant = true", 1), "an [otel] key it does not own"},
		"removed user otel key": {strings.Replace(codexAfterSetup, "environment = \"conformance\"\n", "", 1), "an [otel] key it does not own"},
		"prompt logging on":     {strings.Replace(codexAfterSetup, "log_user_prompt = false", "log_user_prompt = true", 1), "[otel] log_user_prompt is not exactly false"},
		"tool output kept":      {strings.Replace(codexAfterSetup, "max_bytes = 0", "max_bytes = 4096", 1), "[otel] tool_result is not exactly"},
		"another endpoint":      {strings.Replace(codexAfterSetup, "4318/v1/logs", "4319/v1/logs", 1), "[otel] exporter is not exactly"},
		"missing exporter":      {strings.Replace(codexAfterSetup, "metrics_exporter = { otlp-http = { endpoint = \"http://127.0.0.1:4318/v1/metrics\", protocol = \"binary\" } }\n", "", 1), "[otel] has no metrics_exporter"},
		// Present, as the old check wanted, but as a comment: not set.
		"exporter only in a comment":         {strings.Replace(codexAfterSetup, "exporter = { otlp-http = { endpoint = \"http://127.0.0.1:4318/v1/logs\"", "# exporter = { otlp-http = { endpoint = \"http://127.0.0.1:4318/v1/logs\"", 1), "[otel] has no exporter"},
		"plugin tables without the host CLI": {codexAfterSetup + codexRegistered, "added table [marketplaces.kenaz]"},
	} {
		p := checkCodexConfig(seed, []byte(m.after), testEndpoint, false)
		if !strings.Contains(strings.Join(p, "; "), m.want) {
			t.Errorf("%s: problems %v, want %q", name, p, m.want)
		}
	}
}

// setup-undo: byte-identical to the pre-existing file, and no plugin left
// registered. The reviewer's mutants (undo leaves the plugin registered;
// undo's restore of the user's prior value is broken) each fail it.
func TestUndoneMeansTheOriginalFile(t *testing.T) {
	for _, cli := range []string{"claude", "codex"} {
		seed := hostSeed(cli)
		if p, same := checkUndone(cli, seed, seed, testEndpoint, false); len(p) != 0 || !same {
			t.Fatalf("%s: a correct undo is refused: %v", cli, p)
		}
		if p, _ := checkUndone(cli, seed, nil, testEndpoint, false); len(p) != 1 || !strings.Contains(p[0], "is gone") {
			t.Errorf("%s: a deleted config: %v", cli, p)
		}
		edited, ok := userEdit(cli, seed)
		if !ok || string(edited) == string(seed) {
			t.Fatalf("%s: the user's edit does not apply to the seed", cli)
		}
		if _, ok := userEdit(cli, edited); ok {
			t.Errorf("%s: the user's edit applied twice", cli)
		}
	}
	for name, m := range map[string]struct {
		cli, after, want string
	}{
		"claude: plugin left registered":   {"claude", strings.TrimSuffix(claudeSeed, "\n}\n") + claudeRegistered, "the plugin is still registered"},
		"claude: prior value not restored": {"claude", strings.Replace(claudeSeed, `"OTEL_LOG_USER_PROMPTS": "1"`, `"OTEL_LOG_USER_PROMPTS": "0"`, 1), "not byte-identical"},
		"claude: prior key dropped":        {"claude", strings.Replace(claudeSeed, ",\n    \"OTEL_LOG_USER_PROMPTS\": \"1\"", "", 1), "not byte-identical"},
		"claude: owned key left":           {"claude", claudeAfterSetup, "still points at the relay"},
		"claude: reformatted":              {"claude", strings.ReplaceAll(claudeSeed, "  ", "\t"), "not byte-identical"},
		"claude: empty plugin map left":    {"claude", strings.TrimSuffix(claudeSeed, "\n}\n") + ",\n  \"enabledPlugins\": {}\n}\n", "not byte-identical"},
		"codex: plugin left registered":    {"codex", codexSeed + codexRegistered, "the plugin is still registered"},
		"codex: prior value not restored":  {"codex", strings.Replace(codexSeed, "log_user_prompt = true", "log_user_prompt = false", 1), "not byte-identical"},
		"codex: prior key dropped":         {"codex", strings.Replace(codexSeed, "log_user_prompt = true\n", "", 1), "not byte-identical"},
		"codex: owned keys left":           {"codex", codexAfterSetup, "still points at the relay"},
		"codex: marketplace left":          {"codex", codexSeed + "\n[marketplaces.kenaz]\nsource = \"/x\"\n", "the plugin is still registered"},
	} {
		// Refused whether or not the host's CLI rewrote the file, except the
		// two that differ only in what a host's writer changes.
		for _, hostRewrote := range []bool{false, true} {
			p, _ := checkUndone(m.cli, hostSeed(m.cli), []byte(m.after), testEndpoint, hostRewrote)
			tolerated := hostRewrote && (name == "claude: reformatted" || name == "claude: empty plugin map left")
			if tolerated != (len(p) == 0) || (!tolerated && !strings.Contains(strings.Join(p, "; "), m.want)) {
				t.Errorf("%s (host rewrote %v): problems %v, want %q", name, hostRewrote, p, m.want)
			}
		}
	}
	// What the real Claude CLI leaves after it unregisters the plugin: keys
	// reordered and empty plugin maps. The same content, so it passes only
	// when the host rewrote the file.
	hostLeft := `{"env": {"CONFORMANCE_UNRELATED": "kept", "OTEL_LOG_USER_PROMPTS": "1"}, "permissions": {"allow": ["Bash(ls:*)"]}, "enabledPlugins": {}, "extraKnownMarketplaces": {}, "theme": "dark"}`
	if p, same := checkUndone("claude", []byte(claudeSeed), []byte(hostLeft), testEndpoint, true); len(p) != 0 || same {
		t.Errorf("the host's own rewrite is refused: %v", p)
	}
	if p, _ := checkUndone("claude", []byte(claudeSeed), []byte(hostLeft), testEndpoint, false); len(p) == 0 {
		t.Error("a rewritten file passed as byte-identical")
	}
}

// A host listing that exits non-zero is "needs sign-in" only when it says
// so. Any other non-zero exit fails the step; it is never "not run".
func TestListingVerdicts(t *testing.T) {
	ok := listingResult{name: "host plugin list", code: 0}
	wrong := listingResult{name: "host plugin list", code: 0, why: "the host does not list kenaz@kenaz"}
	signIn := listingResult{name: "host mcp list", code: 1, out: "Error: not logged in. Run `host login`."}
	broken := listingResult{name: "host plugin details", code: 2, out: "error: unknown command 'details'"}
	crashed := listingResult{name: "host plugin list", code: -1, out: ""}
	for name, c := range map[string]struct {
		in         []listingResult
		status     string
		visibility string
	}{
		"all listings work":             {[]listingResult{ok, ok}, conformance.StepPass, conformance.VisibilityVerified},
		"one works, one needs sign-in":  {[]listingResult{ok, signIn}, conformance.StepPass, conformance.VisibilityVerified},
		"all need sign-in":              {[]listingResult{signIn}, conformance.StepNotRun, conformance.VisibilityNeedsSignIn},
		"no listing":                    {nil, conformance.StepNotRun, conformance.VisibilityNotRun},
		"host does not show the plugin": {[]listingResult{ok, wrong}, conformance.StepFail, conformance.VisibilityNotRun},
		"non-zero without sign-in":      {[]listingResult{broken}, conformance.StepFail, conformance.VisibilityNotRun},
		"works, then a non-zero exit":   {[]listingResult{ok, broken}, conformance.StepFail, conformance.VisibilityNotRun},
		"sign-in, then a non-zero exit": {[]listingResult{signIn, broken}, conformance.StepFail, conformance.VisibilityNeedsSignIn},
		"could not run":                 {[]listingResult{crashed}, conformance.StepFail, conformance.VisibilityNotRun},
	} {
		v := classifyListings(c.in)
		if v.status != c.status || v.visibility != c.visibility {
			t.Errorf("%s: %s/%s, want %s/%s (%v)", name, v.status, v.visibility, c.status, c.visibility, v.problems)
		}
		if v.status == conformance.StepFail && len(v.problems) == 0 {
			t.Errorf("%s: a failure with no problem recorded", name)
		}
	}
}

// kenaz never runs in a sandbox before the file credential store is
// recorded there (otherwise it would probe the OS keychain).
func TestKenazRefusedBeforeTheFileStoreIsRecorded(t *testing.T) {
	s, err := newSandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	// Not a kenaz: if the guard failed, running it would say so.
	s.exe = "definitely-not-a-binary-that-exists"
	out, code := s.kenaz(t.Context(), "version", "--json")
	if code != -1 || !strings.Contains(out, "credential store is not recorded as files yet") {
		t.Fatalf("kenaz ran before the marker: %d %q", code, out)
	}
	if _, err := s.startMCP(t.Context()); err == nil || !strings.Contains(err.Error(), "not recorded as files yet") {
		t.Fatalf("kenaz mcp started before the marker: %v", err)
	}
	if err := s.useFileCredentialStore(); err != nil {
		t.Fatal(err)
	}
	if err := s.fileStoreRecorded(); err != nil {
		t.Fatal(err)
	}
	if !within(s.credentialsDir(), s.root) {
		t.Fatal("the marker is outside the sandbox")
	}
	if out, _ := s.kenaz(t.Context(), "version"); strings.Contains(out, "not recorded as files yet") {
		t.Fatal("still refused after the marker was written")
	}
}
