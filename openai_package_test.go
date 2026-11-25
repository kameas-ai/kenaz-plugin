package kenaz

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image/png"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The directory package in openai/ lists the hosted Kameas Fleet connector
// in OpenAI's plugin directory (openai/SUBMISSION.md). The directory rejects
// lifecycle hooks, app references, local MCP servers and auth headers, so
// this package carries only the hosted server, skills that need no local
// program, and the listing. These tests hold it to the directory's
// final-submission limits.

const openAIPackageDir = "openai"

// DisclaimerG0 is the attribution for surfaces that name no third-party
// product. The directory listing names none, so it ends with this.
const DisclaimerG0 = "Kenaz and Kameas are trademarks of Kameas AI, Inc. Names of AI tools and platforms made by other companies are trademarks of their respective owners and are used only to identify those products. Kenaz is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by any of them."

// hostedConnectorURL is the one MCP server the package may declare.
const hostedConnectorURL = "https://mcp.fleet.kameas.ai/mcp"

// hostedTools are the hosted connector's tools; review test cases may name
// only these.
var hostedTools = []string{
	"context_search", "context_pull", "skills_search", "skills_get", "fleet_status",
	"memory_search", "memory_get", "memory_recent", "memory_export", "approval_status",
	"memory_save", "memory_update", "memory_forget", "memory_forget_all",
	"context_share", "context_promote", "skills_publish",
	// Tool list v6 adds these two read-only tools.
	"memory_timeline", "memory_sync_status",
}

// openAIPackageFiles is every file the package may contain. A stray file
// under openai/ (other than SUBMISSION.md) fails the test.
var openAIPackageFiles = []string{
	".codex-plugin/plugin.json",
	".mcp.json",
	"assets/composer-icon.png",
	"assets/logo.png",
	"skills/get-started/SKILL.md",
	"skills/kenaz/SKILL.md",
}

// promotion matches sign-up, pricing, upgrade and endorsement wording, and
// encryption claims, none of which the listing or skills may contain.
var promotion = regexp.MustCompile(`(?i)\b(buy|buying|purchase\w*|pro|premium|pricing|prices?|trials?|free|discounts?|coupons?|subscri\w*|upgrad\w*|sign[- ]?up|plan at|plans start|per (month|seat|user)|official|certified|partner\w*|end-to-end|encrypt\w*)\b`)

var directoryCategories = []string{
	"Productivity", "Creativity", "Developer Tools", "Business & Operations", "Data & Analytics",
	"Communication", "Education & Research", "Security", "Finance", "Healthcare", "Travel",
	"Entertainment", "Other",
}

// thirdPartyMark matches marks that may not appear in any name we choose.
var thirdPartyMark = regexp.MustCompile(`(?i)claude|codex|anthropic|openai|chatgpt|cowork|opencode`)

type openAIManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"author"`
	Homepage   string   `json:"homepage"`
	Keywords   []string `json:"keywords"`
	Skills     string   `json:"skills"`
	McpServers string   `json:"mcpServers"`
	Interface  struct {
		DisplayName       string   `json:"displayName"`
		ShortDescription  string   `json:"shortDescription"`
		LongDescription   string   `json:"longDescription"`
		DeveloperName     string   `json:"developerName"`
		Category          string   `json:"category"`
		Capabilities      []string `json:"capabilities"`
		WebsiteURL        string   `json:"websiteURL"`
		SupportURL        string   `json:"supportURL"`
		PrivacyPolicyURL  string   `json:"privacyPolicyURL"`
		TermsOfServiceURL string   `json:"termsOfServiceURL"`
		DefaultPrompt     []string `json:"defaultPrompt"`
		ComposerIcon      string   `json:"composerIcon"`
		Logo              string   `json:"logo"`
	} `json:"interface"`
	Extensions struct {
		OpenAI struct {
			OnboardingSkill string `json:"onboardingSkill"`
			Review          struct {
				TestCases struct {
					Positive []struct {
						Description      string `json:"description"`
						Prompt           string `json:"prompt"`
						ToolsTriggered   string `json:"tools_triggered"`
						ExpectedBehavior string `json:"expected_behavior"`
					} `json:"positive"`
					Negative []struct {
						Description string `json:"description"`
						Prompt      string `json:"prompt"`
					} `json:"negative"`
				} `json:"test_cases"`
			} `json:"review"`
		} `json:"com.openai"`
	} `json:"extensions"`
}

// openAIFS is the package content, read either from openai/ or from a
// built ZIP, keyed by slash path relative to the plugin root.
type openAIFS map[string][]byte

func readOpenAIDir(t *testing.T) openAIFS {
	t.Helper()
	files := openAIFS{}
	err := filepath.WalkDir(openAIPackageDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(openAIPackageDir, p)
		rel = filepath.ToSlash(rel)
		if rel == "SUBMISSION.md" || d.Name() == ".DS_Store" {
			return nil // not packaged
		}
		b, err := os.ReadFile(p)
		files[rel] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestOpenAIPackage(t *testing.T) {
	checkOpenAIPackage(t, readOpenAIDir(t))
}

// TestOpenAIPackageZip builds the ZIP twice with the build script, checks
// the two are byte-identical, and validates what is inside.
func TestOpenAIPackageZip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the build script needs bash")
	}
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	dir := t.TempDir()
	build := func(name string) []byte {
		out := filepath.Join(dir, name)
		cmd := exec.Command("bash", "scripts/build-openai-package.sh", out)
		cmd.Env = append(os.Environ(), "TMPDIR="+dir)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, b)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first, second := build("a.zip"), build("b.zip")
	if !bytes.Equal(first, second) {
		t.Fatal("two builds of the package differ: the ZIP must be deterministic")
	}

	zr, err := zip.NewReader(bytes.NewReader(first), int64(len(first)))
	if err != nil {
		t.Fatal(err)
	}
	files := openAIFS{}
	var names []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, "\\") || slices.Contains(strings.Split(f.Name, "/"), "..") {
			t.Errorf("unsafe entry path %q", f.Name)
		}
		if !f.Modified.IsZero() && f.Modified.Year() != 1980 {
			t.Errorf("%s: timestamp %v, want the fixed 1980 date", f.Name, f.Modified)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name] = b
		names = append(names, f.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Error("ZIP entries must be sorted")
	}
	src := readOpenAIDir(t)
	if len(files) != len(src) {
		t.Errorf("ZIP has %d files, openai/ has %d package files", len(files), len(src))
	}
	for name, b := range src {
		if !bytes.Equal(files[name], b) {
			t.Errorf("%s: missing from the ZIP or different", name)
		}
	}
	if _, ok := files["SUBMISSION.md"]; ok {
		t.Error("SUBMISSION.md must not be packaged")
	}
	checkOpenAIPackage(t, files)
}

func checkOpenAIPackage(t *testing.T, files openAIFS) {
	t.Helper()
	var got []string
	for name := range files {
		got = append(got, name)
	}
	sort.Strings(got)
	if !slices.Equal(got, openAIPackageFiles) {
		t.Errorf("package files %q, want exactly %q", got, openAIPackageFiles)
	}

	raw, ok := files[".codex-plugin/plugin.json"]
	if !ok {
		t.Fatal(".codex-plugin/plugin.json: missing")
	}
	var m openAIManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf(".codex-plugin/plugin.json: %v", err)
	}

	// A portable root manifest would take precedence over this one.
	for _, p := range []string{"plugin.json", "mcp.json", ".claude-plugin/plugin.json", ".agent-plugin/plugin.json"} {
		if _, ok := files[p]; ok {
			t.Errorf("%s: the package uses the Codex layout only", p)
		}
	}

	// No hooks and no app references, anywhere.
	var generic any
	_ = json.Unmarshal(raw, &generic)
	for _, k := range jsonKeys(generic) {
		switch k {
		case "hooks", "apps", "test_credentials", "reviewer_instructions", "clientSecret":
			t.Errorf("plugin.json: %q is not allowed in a directory package", k)
		}
	}
	for name := range files {
		base := path.Base(name)
		if strings.HasPrefix(name, "hooks/") || base == "hooks.json" || base == ".app.json" {
			t.Errorf("%s: hooks and app references are not allowed", name)
		}
	}

	// Package identity.
	if !regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(m.Name) || len(m.Name) > 64 {
		t.Errorf("name %q: lowercase letters, digits and single hyphens, at most 64", m.Name)
	}
	if !regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`).MatchString(m.Version) {
		t.Errorf("version %q: want an explicit semantic version", m.Version)
	}
	checkText(t, "description", m.Description, 1024, false)
	checkText(t, "author.name", m.Author.Name, 120, true)
	checkHTTPS(t, "author.url", m.Author.URL, 2048)
	checkHTTPS(t, "homepage", m.Homepage, 2048)
	if len(m.Keywords) > 25 {
		t.Errorf("keywords: %d, at most 25", len(m.Keywords))
	}
	if m.Skills != "./skills/" {
		t.Errorf(`skills %q, want "./skills/"`, m.Skills)
	}
	if m.McpServers != "./.mcp.json" {
		t.Errorf(`mcpServers %q, want "./.mcp.json"`, m.McpServers)
	}

	// Listing.
	in := m.Interface
	checkText(t, "interface.displayName", in.DisplayName, 30, true)
	checkText(t, "interface.shortDescription", in.ShortDescription, 30, true)
	checkText(t, "interface.longDescription", in.LongDescription, 4000, false)
	checkText(t, "interface.developerName", in.DeveloperName, 80, true)
	if !slices.Contains(directoryCategories, in.Category) {
		t.Errorf("interface.category %q is not a directory category", in.Category)
	}
	if in.Capabilities == nil || len(in.Capabilities) > 20 {
		t.Errorf("interface.capabilities: required, at most 20, got %d", len(in.Capabilities))
	}
	for i, c := range in.Capabilities {
		checkText(t, "interface.capabilities["+strconv.Itoa(i)+"]", c, 120, true)
	}
	for field, u := range map[string]string{
		"websiteURL": in.WebsiteURL, "supportURL": in.SupportURL,
		"privacyPolicyURL": in.PrivacyPolicyURL, "termsOfServiceURL": in.TermsOfServiceURL,
	} {
		checkHTTPS(t, "interface."+field, u, 1024)
	}
	if len(in.DefaultPrompt) > 3 {
		t.Errorf("interface.defaultPrompt: %d prompts, at most 3", len(in.DefaultPrompt))
	}
	seen := map[string]bool{}
	for i, p := range in.DefaultPrompt {
		checkText(t, "interface.defaultPrompt["+strconv.Itoa(i)+"]", p, 128, true)
		key := strings.Join(strings.Fields(strings.ToLower(p)), " ")
		if seen[key] {
			t.Errorf("interface.defaultPrompt[%d]: duplicate", i)
		}
		seen[key] = true
		if strings.Contains(p, "@") {
			t.Errorf("interface.defaultPrompt[%d]: no @mentions", i)
		}
	}

	// Icons: square PNGs, 48 to 4096 px, at most 5 MiB.
	for field, p := range map[string]string{"composerIcon": in.ComposerIcon, "logo": in.Logo} {
		checkIcon(t, files, "interface."+field, p)
	}

	// Copy rules: no third-party mark in any name we choose, the G0
	// attribution at the end of both descriptions, no encryption claim, and
	// no sign-up, pricing or upgrade promotion.
	names := map[string]string{"name": m.Name, "interface.displayName": in.DisplayName}
	if thirdPartyMark.MatchString(in.LongDescription + in.ShortDescription + m.Description) {
		t.Error("the listing names a third-party product; it must then carry that owner's attribution, not G0")
	}
	for _, field := range []struct{ name, text string }{{"description", m.Description}, {"interface.longDescription", in.LongDescription}} {
		if !strings.HasSuffix(field.text, DisclaimerG0) {
			t.Errorf("%s must end with the G0 attribution", field.name)
		}
	}
	listing := strings.Join(append(append([]string{m.Description, in.ShortDescription, in.LongDescription}, in.Capabilities...), in.DefaultPrompt...), "\n")
	if w := promotion.FindString(listing); w != "" {
		t.Errorf("listing text must not say %q (promotion, endorsement or encryption claim)", w)
	}

	// Exactly one remote MCP server, the hosted connector, with OAuth and
	// nothing that runs locally or carries a credential.
	var mcp struct {
		McpServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(files[".mcp.json"], &mcp); err != nil {
		t.Fatalf(".mcp.json: %v", err)
	}
	if len(mcp.McpServers) != 1 {
		t.Fatalf(".mcp.json: %d servers, want exactly 1", len(mcp.McpServers))
	}
	for name, srv := range mcp.McpServers {
		names["MCP server"] = name
		if srv["url"] != hostedConnectorURL {
			t.Errorf(".mcp.json %s: url %v, want %s", name, srv["url"], hostedConnectorURL)
		}
		for _, k := range []string{"command", "args", "env", "headers", "cwd"} {
			if _, ok := srv[k]; ok {
				t.Errorf(".mcp.json %s: %q is not allowed (remote server only, no auth headers)", name, k)
			}
		}
		for _, k := range jsonKeys(srv) {
			if k == "clientSecret" || k == "clientId" {
				t.Errorf(".mcp.json %s: %q must not be in the package", name, k)
			}
		}
		ext, _ := srv["extensions"].(map[string]any)
		oa, _ := ext["com.openai"].(map[string]any)
		auth, _ := oa["auth"].(map[string]any)
		if auth["type"] != "oauth" {
			t.Errorf(".mcp.json %s: auth.type %v, want oauth", name, auth["type"])
		}
	}

	// Skills: each a directory with SKILL.md and valid front matter, none
	// relying on a local program.
	skills := map[string]bool{}
	for p := range files {
		if dir, ok := strings.CutPrefix(p, "skills/"); ok {
			skills[strings.SplitN(dir, "/", 2)[0]] = true
		}
	}
	if len(skills) == 0 {
		t.Error("skills/: the directory requires at least one skill")
	}
	localProgram := regexp.MustCompile("`kenaz[ `]|/kenaz:|(?i:\\brun\\s+`?kenaz\\b)|kenaz (login|init|memory|context|approve|status|grant|hook|mcp)\\b")
	for dir := range skills {
		p := "skills/" + dir + "/SKILL.md"
		body, ok := files[p]
		if !ok {
			t.Errorf("%s: missing", p)
			continue
		}
		name, desc, rest := splitFrontMatter(t, p, string(body))
		if name != dir {
			t.Errorf("%s: name %q must match its directory", p, name)
		}
		names["skill "+dir] = name
		if len(m.Name)+1+len(name) > 64 {
			t.Errorf("%s: %s:%s is longer than 64", p, m.Name, name)
		}
		checkText(t, p+" description", desc, 1024, true)
		if strings.Contains(desc, ": ") || strings.HasPrefix(desc, "\"") {
			t.Errorf("%s: keep the description a plain YAML scalar (no \": \", no quotes)", p)
		}
		if strings.TrimSpace(rest) == "" {
			t.Errorf("%s: empty instructions", p)
		}
		if w := promotion.FindString(string(body)); w != "" {
			t.Errorf("%s: must not say %q (promotion, endorsement or encryption claim)", p, w)
		}
		if loc := localProgram.FindString(string(body)); loc != "" {
			t.Errorf("%s: refers to the local kenaz program (%q); the package has none", p, loc)
		}
		for _, tool := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(rest, -1) {
			if strings.Contains(tool[1], "_") && !slices.Contains(hostedTools, tool[1]) {
				t.Errorf("%s: names %q, which the hosted connector does not have", p, tool[1])
			}
		}
	}
	for where, n := range names {
		if thirdPartyMark.MatchString(n) {
			t.Errorf("%s %q contains a third-party mark", where, n)
		}
	}

	// Onboarding and review material.
	ox := m.Extensions.OpenAI
	if s := strings.TrimPrefix(ox.OnboardingSkill, "./"); s == ox.OnboardingSkill || files[s] == nil {
		t.Errorf("onboardingSkill %q: want a ./ path to a packaged SKILL.md", ox.OnboardingSkill)
	}
	tc := ox.Review.TestCases
	if len(tc.Positive) != 5 || len(tc.Negative) != 3 {
		t.Errorf("review.test_cases: %d positive and %d negative, want 5 and 3", len(tc.Positive), len(tc.Negative))
	}
	for i, c := range tc.Positive {
		if c.Description == "" || c.Prompt == "" || c.ToolsTriggered == "" || c.ExpectedBehavior == "" {
			t.Errorf("positive case %d: description, prompt, tools_triggered and expected_behavior are required", i)
		}
		for _, tool := range strings.Split(c.ToolsTriggered, ",") {
			if !slices.Contains(hostedTools, strings.TrimSpace(tool)) {
				t.Errorf("positive case %d: %q is not a hosted connector tool", i, tool)
			}
		}
	}
	for i, c := range tc.Negative {
		if c.Description == "" || c.Prompt == "" {
			t.Errorf("negative case %d: description and prompt are required", i)
		}
	}
}

func checkText(t *testing.T, field, s string, max int, oneLine bool) {
	t.Helper()
	switch {
	case strings.TrimSpace(s) == "":
		t.Errorf("%s: required", field)
	case utf8.RuneCountInString(s) > max:
		t.Errorf("%s: %d characters, at most %d", field, utf8.RuneCountInString(s), max)
	case s != strings.TrimSpace(s):
		t.Errorf("%s: leading or trailing whitespace", field)
	}
	for _, r := range s {
		if (r < 0x20 && r != '\n') || r == 0x7f || r == ' ' || r == ' ' || r == '\t' {
			t.Errorf("%s: unsupported character %U", field, r)
		}
		if oneLine && r == '\n' {
			t.Errorf("%s: must fit on one line", field)
		}
	}
}

func checkHTTPS(t *testing.T, field, u string, max int) {
	t.Helper()
	if !regexp.MustCompile(`^https://[a-z0-9.-]+(/[^\s@]*)?$`).MatchString(u) || len(u) > max {
		t.Errorf("%s %q: want an HTTPS URL without credentials, at most %d characters", field, u, max)
	}
}

func checkIcon(t *testing.T, files openAIFS, field, p string) {
	t.Helper()
	rel, ok := strings.CutPrefix(p, "./")
	if !ok || strings.Contains(rel, "..") {
		t.Errorf("%s %q: want a ./ path inside the package", field, p)
		return
	}
	b, ok := files[rel]
	if !ok {
		t.Errorf("%s: %s is not in the package", field, p)
		return
	}
	if !strings.HasSuffix(rel, ".png") {
		t.Errorf("%s: %s must be a PNG", field, p)
		return
	}
	if len(b) > 5<<20 {
		t.Errorf("%s: %d bytes, at most 5 MiB", field, len(b))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Errorf("%s: %v", field, err)
		return
	}
	if cfg.Width != cfg.Height || cfg.Width < 48 || cfg.Width > 4096 {
		t.Errorf("%s: %dx%d, want square, 48 to 4096 px", field, cfg.Width, cfg.Height)
	}
}

func splitFrontMatter(t *testing.T, p, s string) (name, desc, rest string) {
	t.Helper()
	head, rest, ok := strings.Cut(strings.TrimPrefix(s, "---\n"), "\n---\n")
	if !strings.HasPrefix(s, "---\n") || !ok {
		t.Errorf("%s: want YAML front matter between --- lines", p)
		return "", "", s
	}
	for _, line := range strings.Split(head, "\n") {
		k, v, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(k) {
		case "name":
			name = strings.TrimSpace(v)
		case "description":
			desc = strings.TrimSpace(v)
		}
	}
	return name, desc, rest
}

func jsonKeys(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			out = append(out, k)
			out = append(out, jsonKeys(e)...)
		}
	case []any:
		for _, e := range x {
			out = append(out, jsonKeys(e)...)
		}
	}
	return out
}
